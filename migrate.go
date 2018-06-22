package migrate

import (
	"database/sql"
	"fmt"
	"sort"
	"time"
)

const (
	// NilVersion is a Claytons version
	// "the version you are at when you are not at a version"
	NilVersion = -1
)

// Migration interface
type Migration interface {
	// The version of this migration
	Version() int64
	// Run the migration
	Run(*sql.Tx) error
}

// ResultFunc is the callback signature
type ResultFunc func(int64, int64, error)

// A Migrator collates and runs migrations
type Migrator struct {
	db           *sql.DB
	migrations   []Migration
	versionTable *string
	stmts        map[string]*sql.Stmt
	prepared     bool
	callback     ResultFunc
}

// Sort those migrations
type sorted []Migration

func (s sorted) Len() int           { return len(s) }
func (s sorted) Less(i, j int) bool { return s[i].Version() < s[j].Version() }
func (s sorted) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// Version reports the current version of the database
func (m *Migrator) Version() (int64, error) {
	err := m.prepareForMigration()
	if err != nil {
		return NilVersion, err
	}

	var version int64
	err = m.stmts["getVersion"].QueryRow().Scan(&version)
	if err != nil {
		if err == sql.ErrNoRows {
			return NilVersion, nil
		}
		return NilVersion, err
	}
	return version, nil
}

// Migrate migrates the database to the highest possible version
func (m *Migrator) Migrate() error {
	err := m.prepareForMigration()
	if err != nil {
		return err
	}

	// Get the last available migration
	v := m.migrations[len(m.migrations)-1].Version()
	return m.MigrateTo(v)
}

// MigrateTo migrates the database to the specified version
func (m *Migrator) MigrateTo(toVersion int64) error {
	err := m.prepareForMigration()
	if err != nil {
		return err
	}

	maxVersion := m.migrations[len(m.migrations)-1].Version()

	currVersion, err := m.Version()
	if err != nil {
		return fmt.Errorf("migration %d failed: %s", currVersion, err)
	}

	if currVersion >= toVersion {
		go m.callback(maxVersion, currVersion, nil)
		return nil
	}

	for _, mig := range m.migrations {
		nextVersion := mig.Version()

		// Skip old migrations
		if nextVersion <= currVersion {
			continue
		}

		// Ensure contiguous
		if currVersion != NilVersion && nextVersion != currVersion+1 {
			return fmt.Errorf("non-contiguous migration: %v -> %v", currVersion, nextVersion)
		}

		if currVersion < nextVersion && nextVersion <= toVersion {
			err = func() error {
				// Start a transaction
				tx, err := m.db.Begin()
				if err != nil {
					return fmt.Errorf("migration %d failed: %s", currVersion, err)
				}
				defer tx.Commit()

				// Run the migration
				if err = mig.Run(tx); err != nil {
					tx.Rollback()
					return fmt.Errorf("migration %d failed: %s", currVersion, err)
				}
				// Update the version entry
				if err = m.setVersion(tx, nextVersion); err != nil {
					tx.Rollback()
					return fmt.Errorf("migration %d failed: %s", currVersion, err)
				}
				return tx.Commit()
			}()

			if m.callback != nil {
				go m.callback(maxVersion, currVersion, err)
			}

			if err != nil {
				return err
			}
		}
		currVersion = nextVersion
	}

	return nil
}

func (m *Migrator) setVersion(tx *sql.Tx, version int64) (err error) {
	if version >= 0 {
		_, err = tx.Stmt(m.stmts["insertVersion"]).Exec(version, time.Now().Unix())
	}
	return err
}

func (m *Migrator) prepareForMigration() error {
	if m.prepared {
		return nil
	}

	if m.versionTable == nil {
		vt := "current_schema_version"
		m.versionTable = &vt
	}

	if _, err := m.db.Exec(fmt.Sprintf(createTableSQL, *m.versionTable)); err != nil {
		return err
	}

	if err := m.prepareStmts(); err != nil {
		return err
	}

	sort.Sort(sorted(m.migrations))

	m.prepared = true
	return nil
}

func (m *Migrator) prepareStmts() error {
	m.stmts = make(map[string]*sql.Stmt)
	s, err := m.db.Prepare(fmt.Sprintf(getVersionSQL, NilVersion, *m.versionTable))
	if err != nil {
		return err
	}
	m.stmts["getVersion"] = s

	s, err = m.db.Prepare(fmt.Sprintf(insertVersionSQL, *m.versionTable))
	if err != nil {
		return err
	}
	m.stmts["insertVersion"] = s

	return nil
}

const (
	getVersionSQL    = `select coalesce(max(version), %d) from %s`
	insertVersionSQL = `insert into %s (version, applied) values ($1, $2)`

	// Use Unix timestamp for time so it works for SQLite and PostgreSQL
	createTableSQL = `create table if not exists %s (
		version bigint not null primary key,
		applied int)`
)
