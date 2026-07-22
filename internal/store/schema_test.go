package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenCreatesAndReopensVersionOne(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "stream.db")
	store := openStoreAt(t, path, testBusyTimeout)
	if stats := store.db.Stats(); stats.MaxOpenConnections != 1 {
		t.Fatalf("max open connections = %d, want 1",
			stats.MaxOpenConnections)
	}
	var application, version int
	if err := store.db.QueryRowContext(
		ctx, `PRAGMA application_id`,
	).Scan(&application); err != nil {
		t.Fatalf("reading application_id: %v", err)
	}
	if err := store.db.QueryRowContext(
		ctx, `PRAGMA user_version`,
	).Scan(&version); err != nil {
		t.Fatalf("reading user_version: %v", err)
	}
	if application != applicationID || version != databaseVersion {
		t.Fatalf("database identity = %x/v%d, want %x/v%d",
			application, version, applicationID, databaseVersion)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("closing first store: %v", err)
	}
	reopened, err := Open(ctx, path, testBusyTimeout)
	if err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("closing reopened store: %v", err)
	}
}

func TestOpenEscapesSQLitePath(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "duck?pond")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("creating data directory: %v", err)
	}
	path := filepath.Join(dir, "stream#1%.db")
	store, err := Open(ctx, path, testBusyTimeout)
	if err != nil {
		t.Fatalf("opening store: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("closing store: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database was not created at %q: %v", path, err)
	}
}

func TestConcurrentFirstOpenCreatesOneSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stream.db")
	start := make(chan struct{})
	results := make(chan struct {
		store *Store
		err   error
	}, 2)
	for range 2 {
		go func() {
			<-start
			ctx, cancel := context.WithTimeout(
				context.Background(), 5*time.Second,
			)
			defer cancel()
			store, err := Open(ctx, path, testBusyTimeout)
			results <- struct {
				store *Store
				err   error
			}{store, err}
		}()
	}
	close(start)
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent open: %v", result.err)
		}
		if err := result.store.Close(); err != nil {
			t.Fatalf("closing store: %v", err)
		}
	}
}

func TestOpenRejectsUnrecognizedDatabases(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		setup func(*testing.T, string)
		want  string
	}{
		{
			"non-empty unversioned",
			func(t *testing.T, path string) {
				rawExec(t, path, `CREATE TABLE alien(value TEXT)`)
			},
			"unversioned",
		},
		{
			"empty with header",
			func(t *testing.T, path string) {
				rawExec(t, path, `PRAGMA user_version = 1`)
			},
			"empty database",
		},
		{
			"wrong application",
			func(t *testing.T, path string) {
				rawExec(t, path, `
					CREATE TABLE alien(value TEXT);
					PRAGMA application_id = 1234;
					PRAGMA user_version = 1`)
			},
			"application_id",
		},
		{
			"newer version",
			func(t *testing.T, path string) {
				createValidDatabase(t, path)
				rawExec(t, path, `PRAGMA user_version = 2`)
			},
			"newer",
		},
		{
			"changed schema",
			func(t *testing.T, path string) {
				createValidDatabase(t, path)
				rawExec(t, path, `DROP INDEX entries_type_search`)
			},
			"schema does not match",
		},
		{
			"extra object",
			func(t *testing.T, path string) {
				createValidDatabase(t, path)
				rawExec(t, path, `CREATE TABLE alien(value TEXT)`)
			},
			"schema does not match",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.db")
			test.setup(t, path)
			store, err := Open(ctx, path, testBusyTimeout)
			if store != nil {
				store.Close()
				t.Fatal("Open returned a store for invalid database")
			}
			if !errors.Is(err, ErrInvalidDatabase) {
				t.Fatalf("Open error = %v, want ErrInvalidDatabase", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Open error = %q, want text %q", err, test.want)
			}
		})
	}
}

func rawExec(t *testing.T, path, statement string) {
	t.Helper()
	dsn, err := sqliteDSN(path, testBusyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("opening raw database: %v", err)
	}
	if _, err := db.Exec(statement); err != nil {
		db.Close()
		t.Fatalf("setting up raw database: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing raw database: %v", err)
	}
}

func createValidDatabase(t *testing.T, path string) {
	t.Helper()
	store, err := Open(context.Background(), path, testBusyTimeout)
	if err != nil {
		t.Fatalf("creating valid database: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("closing valid database: %v", err)
	}
}
