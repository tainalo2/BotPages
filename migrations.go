package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const maxAppMigrations = 128
const maxMigrationBytes = 1 << 20

var migrationNamePattern = regexp.MustCompile(`^([0-9]{3})_[a-z0-9][a-z0-9_-]*\.sql$`)

type AppMigration struct {
	Name string `json:"name"`
	SQL  string `json:"sql"`
}

func validateAppMigrations(migrations []AppMigration) error {
	if len(migrations) > maxAppMigrations {
		return fmt.Errorf("at most %d migrations are allowed", maxAppMigrations)
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Name < migrations[j].Name })
	for i, migration := range migrations {
		matches := migrationNamePattern.FindStringSubmatch(migration.Name)
		if matches == nil {
			return errors.New("migration names must use NNN_name.sql")
		}
		sequence, _ := strconv.Atoi(matches[1])
		if sequence != i+1 {
			return errors.New("migration numbers must be sequential starting at 001")
		}
		if strings.TrimSpace(migration.SQL) == "" || len(migration.SQL) > maxMigrationBytes {
			return fmt.Errorf("migration %s is empty or exceeds 1 MiB", migration.Name)
		}
		if unsafeMigrationSQL(migration.SQL) {
			return fmt.Errorf("migration %s contains a forbidden SQLite operation", migration.Name)
		}
	}
	return nil
}

func unsafeMigrationSQL(script string) bool {
	upper := strings.ToUpper(script)
	if strings.Contains(upper, "_BOTPAGES_") {
		return true
	}
	for _, keyword := range []string{
		"ATTACH", "DETACH", "VACUUM", "PRAGMA", "LOAD_EXTENSION",
		"BEGIN", "COMMIT", "END", "ROLLBACK", "SAVEPOINT", "RELEASE",
	} {
		if containsSQLKeyword(script, keyword) {
			return true
		}
	}
	return false
}

func containsSQLKeyword(script, wanted string) bool {
	for i := 0; i < len(script); {
		switch {
		case script[i] == '\'' || script[i] == '"' || script[i] == '`':
			quote := script[i]
			i++
			for i < len(script) {
				if script[i] == quote {
					if i+1 < len(script) && script[i+1] == quote {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
		case i+1 < len(script) && script[i] == '-' && script[i+1] == '-':
			i += 2
			for i < len(script) && script[i] != '\n' {
				i++
			}
		case i+1 < len(script) && script[i] == '/' && script[i+1] == '*':
			i += 2
			for i+1 < len(script) && !(script[i] == '*' && script[i+1] == '/') {
				i++
			}
			if i+1 < len(script) {
				i += 2
			}
		case isSQLIdentifierCharacter(script[i]):
			start := i
			for i < len(script) && isSQLIdentifierCharacter(script[i]) {
				i++
			}
			if strings.EqualFold(script[start:i], wanted) {
				return true
			}
		default:
			i++
		}
	}
	return false
}

func isSQLIdentifierCharacter(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_'
}

func writeAppMigrationFiles(root string, migrations []AppMigration) error {
	if len(migrations) == 0 {
		return nil
	}
	directory := filepath.Join(root, "migrations")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	for _, migration := range migrations {
		if err := writeSyncedFile(filepath.Join(directory, migration.Name), []byte(migration.SQL)); err != nil {
			return err
		}
	}
	return syncDir(directory)
}

func migrationFilesSize(migrations []AppMigration) int64 {
	var size int64
	for _, migration := range migrations {
		size += int64(len(migration.SQL))
	}
	return size
}

type migrationRun struct {
	store *siteDatabase
	tx    *sql.Tx
	ctx   context.Context
}

func prepareMigrations(ctx context.Context, server *Server, store *siteDatabase, siteName, appName, versionID string, migrations []AppMigration) (*migrationRun, error) {
	if err := store.writeMu.LockContext(ctx); err != nil {
		return nil, err
	}
	if err := server.prepareSiteDatabaseWrite(ctx, siteName, store); err != nil {
		store.writeMu.Unlock()
		return nil, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		store.writeMu.Unlock()
		return nil, err
	}
	run := &migrationRun{store: store, tx: tx, ctx: ctx}
	fail := func(err error) (*migrationRun, error) {
		_ = run.Rollback()
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS _botpages_migrations (
		app_name TEXT NOT NULL,
		migration_name TEXT NOT NULL,
		checksum TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (app_name, migration_name)
	)`); err != nil {
		return fail(fmt.Errorf("create migration history: %w", err))
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS _botpages_app_versions (
		app_name TEXT PRIMARY KEY,
		version_id TEXT NOT NULL,
		published_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return fail(fmt.Errorf("create app version history: %w", err))
	}
	for _, migration := range migrations {
		digest := sha256.Sum256([]byte(migration.SQL))
		checksum := hex.EncodeToString(digest[:])
		var appliedChecksum string
		err = tx.QueryRowContext(ctx,
			"SELECT checksum FROM _botpages_migrations WHERE app_name = ? AND migration_name = ?",
			appName, migration.Name,
		).Scan(&appliedChecksum)
		switch {
		case err == nil && appliedChecksum == checksum:
			continue
		case err == nil:
			return fail(fmt.Errorf("migration %s changed after it was applied", migration.Name))
		case !errors.Is(err, sql.ErrNoRows):
			return fail(fmt.Errorf("read migration history: %w", err))
		}
		if _, err = tx.ExecContext(ctx, migration.SQL); err != nil {
			return fail(fmt.Errorf("migration %s: %w", migration.Name, err))
		}
		if _, err = tx.ExecContext(ctx,
			"INSERT INTO _botpages_migrations(app_name, migration_name, checksum) VALUES (?, ?, ?)",
			appName, migration.Name, checksum,
		); err != nil {
			return fail(fmt.Errorf("record migration %s: %w", migration.Name, err))
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO _botpages_app_versions(app_name, version_id)
		VALUES (?, ?)
		ON CONFLICT(app_name) DO UPDATE SET version_id = excluded.version_id, published_at = CURRENT_TIMESTAMP`, appName, versionID); err != nil {
		return fail(fmt.Errorf("record app version: %w", err))
	}
	return run, nil
}

func (run *migrationRun) Commit() error {
	if run.tx == nil {
		return nil
	}
	err := run.tx.Commit()
	_, _ = checkpointSiteDatabase(run.ctx, run.store)
	run.store.writeMu.Unlock()
	run.tx = nil
	if err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	return nil
}

func (run *migrationRun) Rollback() error {
	if run.tx == nil {
		return nil
	}
	err := run.tx.Rollback()
	run.store.writeMu.Unlock()
	run.tx = nil
	return err
}
