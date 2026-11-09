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
	"sync"

	"github.com/dop251/goja"
	_ "modernc.org/sqlite"
)

const siteDatabaseFilename = "data.sqlite"

type siteDatabase struct {
	db      *sql.DB
	writeMu sync.Mutex
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
	dsn := (&url.URL{Scheme: "file", Path: databasePath}).String() + "?_pragma=busy_timeout%285000%29&_pragma=foreign_keys%281%29"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
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

	store := &siteDatabase{db: db}
	s.siteDatabases[siteName] = store
	return store, nil
}

func (s *Server) closeSiteDatabases() error {
	s.siteDatabasesMu.Lock()
	defer s.siteDatabasesMu.Unlock()
	var closeErr error
	for siteName, store := range s.siteDatabases {
		closeErr = errors.Join(closeErr, store.db.Close())
		delete(s.siteDatabases, siteName)
	}
	return closeErr
}

func (s *Server) bindSiteDatabase(ctx context.Context, vm *goja.Runtime, siteName string) error {
	store, err := s.ensureSiteDatabase(siteName)
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
		store.writeMu.Lock()
		result, err := store.db.ExecContext(ctx, query, arguments...)
		store.writeMu.Unlock()
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
		rows, err := store.db.QueryContext(ctx, query, arguments...)
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
