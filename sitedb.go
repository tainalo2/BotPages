package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/dop251/goja"
	_ "modernc.org/sqlite"
)

const siteDatabaseFilename = "data.sqlite"
const siteDatabaseWALReserve int64 = 16 << 20
const siteDatabaseMetadataReserve int64 = 256 << 10
const sqliteWALFrameHeaderBytes int64 = 24
const siteDatabaseAutoCheckpointPages = 256

var errSiteDatabaseQuota = errors.New("site database quota exhausted")

type siteDatabase struct {
	db      *sql.DB
	readDB  *sql.DB
	writeMu *contextMutex
}

type contextMutex struct {
	slot chan struct{}
}

func newContextMutex() *contextMutex {
	return &contextMutex{slot: make(chan struct{}, 1)}
}

func (mutex *contextMutex) Lock() {
	mutex.slot <- struct{}{}
}

func (mutex *contextMutex) LockContext(ctx context.Context) error {
	select {
	case mutex.slot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (mutex *contextMutex) Unlock() {
	<-mutex.slot
}

func (s *Server) ensureSiteDatabase(siteName string) (*siteDatabase, error) {
	if !safeName(siteName) {
		return nil, errors.New("invalid site database scope")
	}

	s.siteDatabasesMu.Lock()
	defer s.siteDatabasesMu.Unlock()
	if store := s.siteDatabases[siteName]; store != nil {
		return store, nil
	}

	databasePath := filepath.Join(s.cfg.Dir, siteName, siteDatabaseFilename)
	if err := os.MkdirAll(filepath.Dir(databasePath), 0700); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf(
		"%s?_pragma=busy_timeout%%285000%%29&_pragma=foreign_keys%%281%%29&_pragma=wal_autocheckpoint%%28%d%%29&_pragma=journal_size_limit%%28%d%%29",
		(&url.URL{Scheme: "file", Path: databasePath}).String(), siteDatabaseAutoCheckpointPages, siteDatabaseWALReserve,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode = WAL").Scan(&journalMode); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	if journalMode != "wal" {
		_ = db.Close()
		return nil, fmt.Errorf("enable WAL: unexpected journal mode %q", journalMode)
	}
	if err := os.Chmod(databasePath, 0600); err != nil {
		_ = db.Close()
		return nil, err
	}

	readDB, err := sql.Open("sqlite", dsn+"&_pragma=query_only%281%29")
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	readDB.SetMaxOpenConns(8)
	if err := readDB.PingContext(context.Background()); err != nil {
		_ = readDB.Close()
		_ = db.Close()
		return nil, err
	}
	store := &siteDatabase{db: db, readDB: readDB, writeMu: newContextMutex()}
	if err := s.configureSiteDatabaseQuota(context.Background(), siteName, store); err != nil {
		_ = readDB.Close()
		_ = db.Close()
		return nil, err
	}
	s.siteDatabases[siteName] = store
	return store, nil
}

func (s *Server) configureSiteDatabaseQuota(ctx context.Context, siteName string, store *siteDatabase) error {
	root := filepath.Join(s.cfg.Dir, siteName)
	databasePath := filepath.Join(root, siteDatabaseFilename)
	excluded := map[string]bool{
		databasePath:          true,
		databasePath + "-wal": true,
		databasePath + "-shm": true,
	}
	var otherBytes int64
	err := filepath.Walk(root, func(current string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && !excluded[current] {
			otherBytes += info.Size()
		}
		return nil
	})
	if err != nil {
		return err
	}
	available := maxSiteBytes - siteDatabaseWALReserve - siteDatabaseMetadataReserve - otherBytes
	if available <= 0 {
		return errSiteDatabaseQuota
	}
	var pageSize, pageCount int64
	if err := store.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return fmt.Errorf("read database page size: %w", err)
	}
	if err := store.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount); err != nil {
		return fmt.Errorf("read database page count: %w", err)
	}
	// One transaction can temporarily duplicate every database page in the WAL.
	// Include the per-frame WAL header rather than assuming each frame is only
	// one SQLite page.
	maxPages := available / (2*pageSize + sqliteWALFrameHeaderBytes)
	if maxPages < pageCount || maxPages < 1 {
		return errSiteDatabaseQuota
	}
	var appliedMaxPages int64
	pragma := fmt.Sprintf("PRAGMA max_page_count = %d", maxPages)
	if err := store.db.QueryRowContext(ctx, pragma).Scan(&appliedMaxPages); err != nil {
		return fmt.Errorf("set database page limit: %w", err)
	}
	if appliedMaxPages > maxPages {
		return fmt.Errorf("%w: SQLite page limit could not be reduced", errSiteDatabaseQuota)
	}
	return nil
}

func (s *Server) prepareSiteDatabaseWrite(ctx context.Context, siteName string, store *siteDatabase) error {
	busy, err := checkpointSiteDatabase(ctx, store)
	if err != nil {
		return err
	}
	walPath := filepath.Join(s.cfg.Dir, siteName, siteDatabaseFilename) + "-wal"
	walBytes := int64(0)
	if info, statErr := os.Stat(walPath); statErr == nil {
		walBytes = info.Size()
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	if busy && walBytes >= siteDatabaseWALReserve {
		return fmt.Errorf("%w: WAL checkpoint is blocked", errSiteDatabaseQuota)
	}
	return s.configureSiteDatabaseQuota(ctx, siteName, store)
}

func checkpointSiteDatabase(ctx context.Context, store *siteDatabase) (bool, error) {
	var busy, logFrames, checkpointedFrames int
	if err := store.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointedFrames); err != nil {
		return false, fmt.Errorf("checkpoint site database: %w", err)
	}
	return busy != 0, nil
}

func (s *Server) closeSiteDatabases() error {
	s.siteDatabasesMu.Lock()
	defer s.siteDatabasesMu.Unlock()
	var closeErr error
	for siteName, store := range s.siteDatabases {
		closeErr = errors.Join(closeErr, store.readDB.Close(), store.db.Close())
		delete(s.siteDatabases, siteName)
	}
	return closeErr
}

func (s *Server) bindSiteDatabase(ctx context.Context, vm *goja.Runtime, siteName string) error {
	storageLock := s.siteStorageLock(siteName)
	if err := storageLock.LockContext(ctx); err != nil {
		return err
	}
	store, err := s.ensureSiteDatabase(siteName)
	storageLock.Unlock()
	if err != nil {
		return err
	}
	object := vm.NewObject()
	if err := object.Set("exec", func(call goja.FunctionCall) goja.Value {
		query := call.Argument(0).String()
		if err := validateRuntimeSQL(query, "INSERT", "UPDATE", "DELETE", "REPLACE"); err != nil {
			panic(vm.NewGoError(err))
		}
		arguments, err := databaseArguments(call.Arguments[1:])
		if err != nil {
			panic(vm.NewGoError(err))
		}
		storageLock := s.siteStorageLock(siteName)
		if err := storageLock.LockContext(ctx); err != nil {
			panic(vm.NewGoError(fmt.Errorf("database write canceled: %w", err)))
		}
		if err := store.writeMu.LockContext(ctx); err != nil {
			storageLock.Unlock()
			panic(vm.NewGoError(fmt.Errorf("database write canceled: %w", err)))
		}
		if err := s.prepareSiteDatabaseWrite(ctx, siteName, store); err != nil {
			store.writeMu.Unlock()
			storageLock.Unlock()
			panic(vm.NewGoError(err))
		}
		result, err := store.db.ExecContext(ctx, query, arguments...)
		_, _ = checkpointSiteDatabase(ctx, store)
		store.writeMu.Unlock()
		storageLock.Unlock()
		if err != nil {
			panic(vm.NewGoError(fmt.Errorf("database exec failed: %w", err)))
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			panic(vm.NewGoError(fmt.Errorf("database exec result: %w", err)))
		}
		lastInsertID, err := result.LastInsertId()
		if err != nil {
			panic(vm.NewGoError(fmt.Errorf("database exec result: %w", err)))
		}
		return vm.ToValue(map[string]int64{"rowsAffected": rowsAffected, "lastInsertId": lastInsertID})
	}); err != nil {
		return err
	}
	if err := object.Set("query", func(call goja.FunctionCall) goja.Value {
		query := call.Argument(0).String()
		if err := validateRuntimeSQL(query, "SELECT"); err != nil {
			panic(vm.NewGoError(err))
		}
		arguments, err := databaseArguments(call.Arguments[1:])
		if err != nil {
			panic(vm.NewGoError(err))
		}
		rows, err := store.readDB.QueryContext(ctx, query, arguments...)
		if err != nil {
			panic(vm.NewGoError(fmt.Errorf("database query failed: %w", err)))
		}
		defer rows.Close()
		columns, err := rows.Columns()
		if err != nil {
			panic(vm.NewGoError(fmt.Errorf("database query columns: %w", err)))
		}
		result := make([]map[string]any, 0)
		for rows.Next() {
			if len(result) >= 1000 {
				panic(vm.NewGoError(errors.New("database query row limit exceeded")))
			}
			values := make([]any, len(columns))
			destinations := make([]any, len(columns))
			for i := range values {
				destinations[i] = &values[i]
			}
			if err := rows.Scan(destinations...); err != nil {
				panic(vm.NewGoError(fmt.Errorf("database query row: %w", err)))
			}
			row := make(map[string]any, len(columns))
			for i, column := range columns {
				if bytes, ok := values[i].([]byte); ok {
					row[column] = string(bytes)
				} else {
					row[column] = values[i]
				}
			}
			result = append(result, row)
		}
		if err := rows.Err(); err != nil {
			panic(vm.NewGoError(fmt.Errorf("database query rows: %w", err)))
		}
		return vm.ToValue(result)
	}); err != nil {
		return err
	}
	if err := vm.Set("db", object); err != nil {
		return err
	}
	return vm.Set("openDatabase", func(call goja.FunctionCall) goja.Value {
		if call.Argument(0).String() != siteName {
			panic(vm.NewGoError(errors.New("cross-site database access denied")))
		}
		return object
	})
}

func validateRuntimeSQL(query string, allowed ...string) error {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 64<<10 {
		return errors.New("database statement is empty or too large")
	}
	if strings.Contains(strings.ToUpper(query), "_BOTPAGES_") {
		return errors.New("database internal tables are not accessible at runtime")
	}
	if hasSQLStatementSeparator(query) {
		return errors.New("database statement must contain exactly one statement")
	}
	end := 0
	for end < len(query) {
		character := query[end]
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') {
			break
		}
		end++
	}
	keyword := strings.ToUpper(query[:end])
	for _, candidate := range allowed {
		if keyword == candidate {
			return nil
		}
	}
	return errors.New("database statement type is not allowed at runtime")
}

func hasSQLStatementSeparator(query string) bool {
	var quote byte
	for i := 0; i < len(query); i++ {
		character := query[i]
		if quote != 0 {
			if character == quote {
				if i+1 < len(query) && query[i+1] == quote {
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		switch character {
		case '\'', '"', '`':
			quote = character
		case ';':
			return true
		}
	}
	return false
}

func databaseArguments(values []goja.Value) ([]any, error) {
	arguments := make([]any, len(values))
	for i, value := range values {
		exported := value.Export()
		switch exported.(type) {
		case nil, bool, int64, float64, string, []byte:
			arguments[i] = exported
		default:
			return nil, fmt.Errorf("database argument %d has unsupported type", i+1)
		}
	}
	return arguments, nil
}
