package migrator

import (
	"fmt"
	"sync"
	"time"

	"github.com/twitchscience/aws_utils/logger"
	"github.com/twitchscience/rs_ingester/backend"
	"github.com/twitchscience/rs_ingester/blueprint"
	"github.com/twitchscience/rs_ingester/metadata"
	"github.com/twitchscience/rs_ingester/versions"
)

type tableVersion struct {
	table   string
	version int
}

// VersionIncrement is used to send a request to increment a table's version w/o running a migration.
type VersionIncrement struct {
	Table    string
	Version  int
	Response chan error
}

// Migrator manages the migration of Ace as new versioned tsvs come in.
type Migrator struct {
	versions                  versions.GetterSetter
	aceBackend                backend.Backend
	metaBackend               metadata.Reader
	bpClient                  blueprint.Client
	closer                    chan bool
	oldVersionWaitClose       chan bool
	versionIncrement          chan VersionIncrement
	wg                        sync.WaitGroup
	pollPeriod                time.Duration
	waitProcessorPeriod       time.Duration
	migrationStarted          map[tableVersion]time.Time
	offpeakStartHour          int
	offpeakDurationHours      int
	onpeakMigrationTimeoutMs  int
	offpeakMigrationTimeoutMs int
}

// New returns a new Migrator for migrating schemas
func New(aceBack backend.Backend,
	metaBack metadata.Reader,
	blueprintClient blueprint.Client,
	versions versions.GetterSetter,
	pollPeriod time.Duration,
	waitProcessorPeriod time.Duration,
	offpeakStartHour int,
	offpeakDurationHours int,
	versionIncrement chan VersionIncrement,
	onpeakMigrationTimeoutMs int,
	offpeakMigrationTimeoutMs int) *Migrator {
	m := Migrator{
		versions:                  versions,
		aceBackend:                aceBack,
		metaBackend:               metaBack,
		bpClient:                  blueprintClient,
		closer:                    make(chan bool),
		oldVersionWaitClose:       make(chan bool),
		versionIncrement:          versionIncrement,
		pollPeriod:                pollPeriod,
		waitProcessorPeriod:       waitProcessorPeriod,
		migrationStarted:          make(map[tableVersion]time.Time),
		offpeakStartHour:          offpeakStartHour,
		offpeakDurationHours:      offpeakDurationHours,
		onpeakMigrationTimeoutMs:  onpeakMigrationTimeoutMs,
		offpeakMigrationTimeoutMs: offpeakMigrationTimeoutMs,
	}

	m.wg.Add(1)
	logger.Go(func() {
		defer m.wg.Done()
		m.loop()
	})
	return &m
}

// findTablesToMigrate inspects tsvs waiting to be loaded and compares their versions
// with the current versions, returning table names to be migrated up
func (m *Migrator) findTablesToMigrate() ([]string, error) {
	tsvVersions, err := m.metaBackend.Versions()
	if err != nil {
		return nil, fmt.Errorf("Error finding versions from unloaded tsvs: %v", err)
	}
	var tables []string
	for tsvTable, tsvVersion := range tsvVersions {
		aceVersion, existant := m.versions.Get(tsvTable)
		if !existant || tsvVersion > aceVersion {
			tables = append(tables, tsvTable)
		}
	}
	return tables, nil
}

//isOldVersionCleared checks to see if there are any tsvs for the given table and
//version still in queue to be loaded. If there are, it prioritizes those tsvs
//to be loaded.
func (m *Migrator) isOldVersionCleared(table string, version int) (bool, error) {
	exists, err := m.metaBackend.TSVVersionExists(table, version)
	if err != nil {
		return false, err
	}
	if !exists {
		return true, nil
	}
	return false, m.metaBackend.ForceLoad(table, "migrator")
}

func (m *Migrator) migrate(table string, to int, isForced bool) error {
	ops, err := m.bpClient.GetMigration(table, to)
	if err != nil {
		return err
	}
	exists, err := m.aceBackend.TableExists(table)
	if err != nil {
		return err
	}
	if !exists {
		err = m.aceBackend.CreateTable(table, ops, to)
		if err != nil {
			return err
		}
	} else {
		// to migrate, first we wait until processor finishes the old version...
		timeMigrationStarted, started := m.migrationStarted[tableVersion{table, to}]
		if !started {
			now := time.Now()
			m.migrationStarted[tableVersion{table, to}] = now
			logger.WithField("table", table).
				WithField("version", to).
				WithField("until", now.Add(m.waitProcessorPeriod)).
				Info("Starting to wait for processor before migrating")
			return nil
		}
		// don't do anything if we haven't waited long enough for processor
		if time.Since(timeMigrationStarted) < m.waitProcessorPeriod {
			logger.WithField("table", table).
				WithField("version", to).
				WithField("until", timeMigrationStarted.Add(m.waitProcessorPeriod)).
				Info("Waiting for processor before migrating")
			return nil
		}

		// wait for all the old version TSVs to ingest before proceeding
		cleared, err := m.isOldVersionCleared(table, to-1)
		if err != nil {
			return fmt.Errorf("Error waiting for old version to clear: %v", err)
		}
		if !cleared {
			logger.WithField("table", table).WithField("version", to).Info("Waiting for old version to clear.")
			return nil
		}

		// everything is ready, now actually do the migration
		logger.WithField("table", table).WithField("version", to).Info("Beginning to migrate")
		timeoutMs := m.offpeakMigrationTimeoutMs
		if isForced {
			timeoutMs = m.onpeakMigrationTimeoutMs
		}
		err = m.aceBackend.ApplyOperations(table, ops, to, timeoutMs)
		if err != nil {
			return fmt.Errorf("Error applying operations to %s: %v", table, err)
		}
	}
	m.versions.Set(table, to)
	logger.WithField("table", table).WithField("version", to).Info("Migrated table successfully")

	return nil
}

func (m *Migrator) isOffPeakHours() bool {
	currentHour := time.Now().Hour()
	if m.offpeakStartHour+m.offpeakDurationHours <= 24 {
		if (m.offpeakStartHour <= currentHour) &&
			(currentHour < m.offpeakStartHour+m.offpeakDurationHours) {
			return true
		}
		return false
	}
	// if duration bleeds into the new day, check the two segments before and after midnight
	if (m.offpeakStartHour <= currentHour) &&
		(currentHour < 24) {
		return true
	}
	if (0 <= currentHour) &&
		(currentHour < (m.offpeakStartHour+m.offpeakDurationHours)%24) {
		return true
	}
	return false
}

func (m *Migrator) incrementVersion(verInc VersionIncrement) {
	exists, err := m.aceBackend.TableExists(verInc.Table)
	switch {
	case err != nil:
		verInc.Response <- fmt.Errorf(
			"error determining if table %s exists: %v", verInc.Table, err)
	case exists:
		verInc.Response <- fmt.Errorf(
			"attempted to increment version of table that exists: %s", verInc.Table)
	default:
		err = m.aceBackend.ApplyOperations(verInc.Table, nil, verInc.Version, m.offpeakMigrationTimeoutMs)
		if err == nil {
			logger.Infof("Incremented table %s to version %d",
				verInc.Table, verInc.Version)
			m.versions.Set(verInc.Table, verInc.Version)
		}
		verInc.Response <- err
	}
}

func (m *Migrator) findAndApplyMigrations() {
	outdatedTables, err := m.findTablesToMigrate()
	if err != nil {
		logger.WithError(err).Error("Error finding migrations to apply")
	}
	if len(outdatedTables) == 0 {
		logger.Infof("Migrator didn't find any tables to migrate.")
	} else {
		logger.WithField("numTables", len(outdatedTables)).Infof("Migrator found tables to migrate.")
	}
	for _, table := range outdatedTables {
		var newVersion int
		currentVersion, exists := m.versions.Get(table)
		if !exists { // table doesn't exist yet, create it by 'migrating' to version 0
			newVersion = 0
		} else {
			newVersion = currentVersion + 1
		}

		// We allow table creation no matter what.
		// Migrate table only if A) currently offpeak hours OR B) force load on the table has been requested.
		// We cannot on-peak migrate a table if it is locked
		var forceLoadRequested bool
		if newVersion > 0 {
			if !m.isOffPeakHours() {
				forceLoadRequested, err = m.metaBackend.IsForceLoadRequested(table)
				if err != nil {
					logger.WithError(err).WithField("table", table).WithField("version", newVersion).Error("Error checking for pending force load")
					continue
				}
				if !forceLoadRequested {
					logger.WithField("table", table).WithField("version", newVersion).Infof("Not migrating; waiting until offpeak at %dh UTC", m.offpeakStartHour)
					continue
				}

				tableLocked, err := m.aceBackend.TableLocked(table)
				if err != nil {
					logger.WithError(err).WithField("table", table).Error("Error checking for table lock")
					continue
				}
				if tableLocked {
					logger.WithField("table", table).WithField("version", newVersion).Infof("Not migrating; on-peak and table is locked")
					continue
				}
			}
		}
		err = m.migrate(table, newVersion, forceLoadRequested)
		if err != nil {
			logger.WithError(err).WithField("table", table).WithField("version", newVersion).Error("Error migrating table")
		}
	}
}

func (m *Migrator) loop() {
	logger.Info("Migrator started.")
	defer logger.Info("Migrator stopped.")
	tick := time.NewTicker(m.pollPeriod)
	for {
		select {
		case verInc := <-m.versionIncrement:
			m.incrementVersion(verInc)
		case <-tick.C:
			m.findAndApplyMigrations()
		case <-m.closer:
			return
		}
	}
}

// Close signals the migrator to stop looking for new migrations and waits until
// it's finished any migrations.
func (m *Migrator) Close() {
	m.closer <- true
	m.wg.Wait()
}
