package backend

import (
	"bytes"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/lib/pq"
	"github.com/twitchscience/aws_utils/logger"
	"github.com/twitchscience/rs_ingester/redshift"
	"github.com/twitchscience/scoop_protocol/scoop_protocol"
)

var (
	transformerTypeMap = map[string]string{
		"ipCity":            "varchar(64)",
		"ipCountry":         "varchar(2)",
		"ipRegion":          "varchar(64)",
		"ipAsn":             "varchar(128)",
		"ipAsnInteger":      "int",
		"f@timestamp":       "datetime",
		"userIDWithMapping": "bigint",
	}
)

//RedshiftBackend is the struct that holds the RSConnection pool and where backend operations are done from
type RedshiftBackend struct {
	connection  *redshift.RSConnection
	credentials *credentials.Credentials
	tableLocks  map[string]*sync.Mutex
	lockLock    *sync.Mutex
}

//BuildRedshiftBackend builds a new redshift backend by also creating a new rsConnection
func BuildRedshiftBackend(credentials *credentials.Credentials, poolSize int, rsURL string) (*RedshiftBackend, error) {
	conn, err := redshift.BuildRSConnection(rsURL, poolSize)
	if err != nil {
		return nil, err
	}
	for i := 0; i < 5; i++ {
		go conn.Listen()
	}
	return &RedshiftBackend{
		connection:  conn,
		credentials: credentials,
		tableLocks:  make(map[string]*sync.Mutex),
		lockLock:    &sync.Mutex{},
	}, nil
}

//HealthCheck makes sure that redshift is reachable
func (r *RedshiftBackend) HealthCheck() error {
	err := r.connection.Conn.Ping()
	return err
}

//ManifestCopy makes a ManifestRowCopyRequest and returns the function that executes the request
func (r *RedshiftBackend) ManifestCopy(rc *scoop_protocol.ManifestRowCopyRequest) error {
	lock := r.getTableLock(rc.TableName)
	lock.Lock()
	defer lock.Unlock()

	return r.connection.ExecFnInTransaction(redshift.ManifestRowCopyRequest{
		BuiltOn:     time.Now(),
		Name:        rc.TableName,
		ManifestURL: rc.ManifestURL,
		Credentials: redshift.CopyCredentials(r.credentials),
	}.TxExec)
}

//LoadCheck makes a LoadCheckRequest and returns the response of the load check
func (r *RedshiftBackend) LoadCheck(req *scoop_protocol.LoadCheckRequest) (*scoop_protocol.LoadCheckResponse, error) {
	resp := &scoop_protocol.LoadCheckResponse{ManifestURL: req.ManifestURL}
	err := r.connection.ExecFnInTransaction(func(t *sql.Tx) (err error) {
		resp.LoadStatus, err = redshift.CheckLoadStatus(t, req.ManifestURL)
		return
	})
	return resp, err
}

// TableVersions returns the event tables with version numbers
func (r *RedshiftBackend) TableVersions() (map[string]int, error) {
	versions := make(map[string]int)
	rows, err := r.connection.Conn.Query(`SELECT name, MAX(version) FROM infra.table_version GROUP BY name;`)
	if err != nil {
		return nil, fmt.Errorf("Error SELECTing the table versions from ace's infra.table_version: %v", err)
	}
	defer func() {
		err = rows.Close()
		if err != nil {
			logger.WithError(err).Error("Error closing rows")
		}
	}()
	for rows.Next() {
		var table string
		var version int
		if err := rows.Scan(&table, &version); err != nil {
			return nil, err
		}
		versions[table] = version
	}
	return versions, nil
}

type migrationStep scoop_protocol.Operation

func parseFunctionalType(s string) (string, bool) {
	if len(s) > 0 && s[0] == 'f' && s[1] == '@' {
		transformerType, ok := transformerTypeMap[s[:strings.LastIndex(s, "@")]]
		return transformerType, ok
	}
	return "", false
}

func (m *migrationStep) getCreationForm() string {
	tranType, isTranslated := transformerTypeMap[m.ActionMetadata["column_type"]]
	funcType, isFunc := parseFunctionalType(m.ActionMetadata["column_type"])

	var colType string
	if isTranslated {
		colType = tranType
	} else if isFunc {
		colType = funcType
	} else {
		colType = m.ActionMetadata["column_type"]
	}

	maybeColOpts := ""
	if len(m.ActionMetadata["column_options"]) > 1 {
		maybeColOpts = m.ActionMetadata["column_options"]
	}

	return fmt.Sprintf("%s %s%s", pq.QuoteIdentifier(m.Name), colType, maybeColOpts)
}

// expectVersion checks to see if the version in infra.table_version is what was
// given. Special case for version=-1 means you expect table doesn't exist
func expectVersion(tx *sql.Tx, table string, version int) error {
	var readVersion int
	err := tx.QueryRow(`SELECT MAX(version) FROM infra.table_version WHERE name = $1 GROUP BY name;`, table).Scan(&readVersion)
	switch {
	case err == sql.ErrNoRows:
		if version == -1 {
			return nil
		}
		return fmt.Errorf("expected version %d for table %s, but table doesn't exist in infra.table_version", version, table)
	case err != nil:
		return fmt.Errorf("error finding table version from ace: %v", err)
	default:
		if readVersion != version {
			return fmt.Errorf("expected version %d for table %s, but got version %d in infra.table_version", version, table, readVersion)
		}
		return nil
	}
}

//applyOperation applies a single operation to a table given a transaction (no
//rollback or commit)
func applyOperation(op scoop_protocol.Operation, table string, tx *sql.Tx) error {
	var err error
	switch op.Action {
	case scoop_protocol.ADD:
		mStep := migrationStep(op)
		query := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", pq.QuoteIdentifier(table), mStep.getCreationForm())
		_, err = tx.Exec(query)
	case scoop_protocol.DELETE:
		query := fmt.Sprintf("ALTER TABLE %s DROP COLUMN %s CASCADE", pq.QuoteIdentifier(table), pq.QuoteIdentifier(op.Name))
		_, err = tx.Exec(query)
	case scoop_protocol.RENAME:
		query := fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s",
			pq.QuoteIdentifier(table),
			pq.QuoteIdentifier(op.Name),
			pq.QuoteIdentifier(op.ActionMetadata["new_outbound"]),
		)
		_, err = tx.Exec(query)
	case scoop_protocol.REQUEST_DROP_EVENT:
	case scoop_protocol.DROP_EVENT:
	case scoop_protocol.CANCEL_DROP_EVENT:
	default:
		err = fmt.Errorf("Unexpected operation action: %s", op.Action)
	}
	return err
}

//ApplyOperations applies operations to a table and updates the table's version
func (r *RedshiftBackend) ApplyOperations(table string, ops []scoop_protocol.Operation, targetVersion int, timeoutMs int) error {
	lock := r.getTableLock(table)
	lock.Lock()
	defer lock.Unlock()

	return r.connection.ExecFnInTransaction(func(tx *sql.Tx) error {
		err := expectVersion(tx, table, targetVersion-1)
		if err != nil {
			return err
		}
		// set time out for the migration
		_, err = tx.Exec("set statement_timeout to $1", timeoutMs)
		if err != nil {
			return fmt.Errorf("Error setting timeout for migration: %v", err)
		}
		for _, op := range ops {
			err = applyOperation(op, table, tx)
			if err != nil {
				return err
			}
		}
		query := fmt.Sprintf("INSERT INTO infra.table_version (name, version, ts) VALUES ($1, $2, GETDATE())")
		_, err = tx.Exec(query, table, targetVersion)
		if err != nil {
			return fmt.Errorf("Error updating table_version in ace: %v", err)
		}
		return nil
	})
}

type newTable []scoop_protocol.Operation

//buildNewTable creates a newTable from a list of Operations and checks that all the operations
//are add column operations
func buildNewTable(ops []scoop_protocol.Operation) (newTable, error) {
	for _, op := range ops {
		// If we have a DROP_EVENT, treat it as a no-op.
		if op.Action == scoop_protocol.DROP_EVENT {
			return nil, nil
		}
		if op.Action != scoop_protocol.ADD {
			return nil, fmt.Errorf("newTable must be made out of action=%s operations, received action=%s", scoop_protocol.ADD, op.Action)
		}
		_, cOptions := op.ActionMetadata["column_options"]
		_, cType := op.ActionMetadata["column_type"]
		if !cOptions || !cType {
			return nil, fmt.Errorf("newTable must have actionmetadata including 'column_options' and 'column_type'")
		}
	}
	return newTable(ops), nil
}

func (n *newTable) getColumnCreationString() string {
	out := bytes.NewBuffer(make([]byte, 0, 256))
	_, _ = out.WriteRune('(') // WriteRune and WriteString error always nil
	for i, op := range *n {
		step := migrationStep(op)
		_, _ = out.WriteString(step.getCreationForm())
		if i+1 != len(*n) {
			_, _ = out.WriteRune(',')
		}
	}
	_, _ = out.WriteRune(')')
	return out.String()
}

//CreateTable creates a table at logs.`table` with the columns in ops unless the ops have DROP_EVENT.
func (r *RedshiftBackend) CreateTable(table string, ops []scoop_protocol.Operation, version int) error {
	newTable, err := buildNewTable(ops)
	// If we had a problem or the operations are to drop the table, just return.
	if err != nil || newTable == nil {
		return err
	}
	return r.connection.ExecFnInTransaction(func(tx *sql.Tx) error {
		query := fmt.Sprintf(`CREATE TABLE %s%s;`, pq.QuoteIdentifier(table), newTable.getColumnCreationString())
		_, err = tx.Exec(query)
		if err != nil {
			return fmt.Errorf("Error CREATEing TABLE %s: %v", table, err)
		}
		query = "INSERT INTO infra.table_version (name, version, ts) VALUES ($1, $2, GETDATE())"
		_, err = tx.Exec(query, table, version)
		if err != nil {
			return fmt.Errorf("Error updating table_version in ace: %v", err)
		}
		return nil
	})
}

// TableExists returns whether the given table exists in the logs schema.
func (r *RedshiftBackend) TableExists(table string) (bool, error) {
	query := `SELECT EXISTS (
		SELECT 1
		FROM pg_catalog.pg_class
		JOIN pg_catalog.pg_namespace
			ON pg_namespace.oid = pg_class.relnamespace
		WHERE pg_namespace.nspname = 'logs'
			AND pg_class.relname = $1
			AND pg_class.relkind = 'r'    -- ordinary table
	)`
	var exists bool
	err := r.connection.Conn.QueryRow(query, table).Scan(&exists)
	switch {
	case err != nil:
		return false, fmt.Errorf("error querying whether table exists: %v", err)
	default:
		return exists, nil
	}
}

// getTableLock returns a lock for the given table, creating it if necessary.
func (r *RedshiftBackend) getTableLock(table string) *sync.Mutex {
	r.lockLock.Lock()
	defer r.lockLock.Unlock()
	lock, exist := r.tableLocks[table]
	if !exist {
		lock = &sync.Mutex{}
		r.tableLocks[table] = lock
	}
	return lock
}

// TableLocked returns whether the given table exists in the logs schema.
func (r *RedshiftBackend) TableLocked(table string) (bool, error) {
	query := `SELECT EXISTS (
		SELECT 1
		FROM pg_locks l JOIN pg_stat_all_tables t
			ON l.relation = t.relid
		WHERE t.relname = $1
	)`
	var exists bool
	err := r.connection.Conn.QueryRow(query, table).Scan(&exists)
	switch {
	case err != nil:
		return false, fmt.Errorf("error querying whether %s table is locked: %v", table, err)
	default:
		return exists, nil
	}
}
