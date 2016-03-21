package redshift

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/AdRoll/goamz/aws"
	"github.com/lib/pq"
	"github.com/twitchscience/scoop_protocol/scoop_protocol"
)

const (
	// need to provide creds, and lib/pq barfs on paramater insertion in copy commands
	copyCommand       = `COPY %s FROM %s WITH CREDENTIALS '%s' %s`
	copyCommandSearch = `COPY %% FROM '%s' %%`
)

var (
	importOptions = strings.Join([]string{
		"removequotes",
		"delimiter '\\t'",
		"gzip",
		"escape",
		"truncatecolumns",
		"roundec",
		"fillrecord",
		"compupdate on",
		"emptyasnull",
		"acceptinvchars '?'",
		"trimblanks;"},
		" ",
	)
	manifestImportOptions = strings.Join([]string{
		"removequotes",
		"delimiter '\\t'",
		"gzip",
		"escape",
		"truncatecolumns",
		"roundec",
		"fillrecord",
		"compupdate on",
		"emptyasnull",
		"acceptinvchars '?'",
		"manifest",
		"trimblanks;"},
		" ",
	)
)

//RowCopyRequest is the redshift packages representation of the row copy object for running a row copy
type RowCopyRequest struct {
	BuiltOn     time.Time
	Name        string
	Key         string
	Credentials string
}

//ManifestRowCopyRequest is the redshift package's represntation of the manifest row copy object for a manifest row copy
type ManifestRowCopyRequest struct {
	BuiltOn     time.Time
	Name        string
	ManifestURL string
	Credentials string
}

//TxExec runs the execution of the row copy request in a transaction
func (r RowCopyRequest) TxExec(t *sql.Tx) error {
	if strings.IndexRune(r.Key, '\000') != -1 || strings.IndexRune(r.Name, '\000') != -1 {
		return fmt.Errorf("Key or name contain a null byte!")
	}

	query := fmt.Sprintf(copyCommand, pq.QuoteIdentifier(r.Name),
		EscapePGString("s3://"+r.Key), r.Credentials, importOptions)

	_, err := t.Exec(query)
	if err != nil {
		log.Printf("Error on executing copy: %v", err)
		return err
	}

	return nil
}

//TxExec runs the execution of the manifest row copy request in a transaction
func (r ManifestRowCopyRequest) TxExec(t *sql.Tx) error {
	if strings.IndexRune(r.ManifestURL, '\000') != -1 || strings.IndexRune(r.Name, '\000') != -1 {
		return fmt.Errorf("ManifestURL or name contain a null byte!")
	}

	query := fmt.Sprintf(copyCommand, pq.QuoteIdentifier(r.Name),
		EscapePGString(r.ManifestURL), r.Credentials, manifestImportOptions)

	_, err := t.Exec(query)
	if err != nil {
		log.Printf("Error on executing copy: %v", err)
		return err
	}

	return nil
}

//RowQueryer is the interface that has a method that returns a function for the row query
type RowQueryer interface {
	QueryRow(query string, args ...interface{}) *sql.Row
}

//CheckLoadStatus checks the status of a load into redshift
func CheckLoadStatus(t RowQueryer, manifestURL string) (scoop_protocol.LoadStatus, error) {
	var count int
	q := fmt.Sprintf(copyCommandSearch, manifestURL)

	err := t.QueryRow("SELECT count(*) FROM STV_RECENTS WHERE query ILIKE $1 AND status != 'Done'", q).Scan(&count)
	if err != nil {
		return "", err
	}

	if count != 0 {
		log.Printf("CheckLoadStatus: Manifest copy %s is in STV_RECENTS as running", manifestURL)
		return scoop_protocol.LoadInProgress, nil
	}

	var aborted, xid int
	err = t.QueryRow("SELECT xid, aborted FROM STL_QUERY WHERE querytxt ILIKE $1", q).Scan(&xid, &aborted)
	switch {
	case err == sql.ErrNoRows:
		log.Printf("CheckLoadStatus: Manifest copy %s does not have a transaction ID", manifestURL)
		return scoop_protocol.LoadNotFound, nil
	case err != nil:
		return "", err
	default:
	}

	if aborted == 1 {
		log.Printf("CheckLoadStatus: Manifest copy %s was aborted while running", manifestURL)
		return scoop_protocol.LoadFailed, nil
	}

	err = t.QueryRow("SELECT count(*) FROM STL_UTILITYTEXT WHERE xid = $1 AND text = 'COMMIT'", xid).Scan(&count)
	if err != nil {
		return "", err
	}

	if count != 0 {
		log.Printf("CheckLoadStatus: Manifest copy %s was committed", manifestURL)
		return scoop_protocol.LoadComplete, nil
	}

	err = t.QueryRow("SELECT count(*) FROM STL_UNDONE WHERE xact_id_undone = $1", xid).Scan(&count)
	if err != nil {
		return "", err
	}

	if count != 0 {
		log.Printf("CheckLoadStatus: Manifest copy %s was rolled back", manifestURL)
		return scoop_protocol.LoadFailed, nil
	}

	log.Printf("CheckLoadStatus: Manifest copy %s was found, has a transaction, and neither rolled back nor committed, assume still running", manifestURL)
	return scoop_protocol.LoadInProgress, nil
}

//CopyCredentials refreshes the redshift aws auth token aggressively
func CopyCredentials(awsCredentials *aws.Auth) (accessCreds string) {
	// Agressively refresh the token
	if awsCredentials.Expiration().Sub(time.Now()) <= 2*time.Hour {
		*awsCredentials, _ = aws.GetAuth("", "", "", time.Time{})
	}

	tempToken := awsCredentials.Token()
	if tempToken == "" {
		accessCreds = fmt.Sprintf(
			"aws_access_key_id=%s;aws_secret_access_key=%s",
			awsCredentials.AccessKey,
			awsCredentials.SecretKey,
		)
	} else {
		accessCreds = fmt.Sprintf(
			"aws_access_key_id=%s;aws_secret_access_key=%s;token=%s",
			awsCredentials.AccessKey,
			awsCredentials.SecretKey,
			tempToken,
		)
	}
	return
}
