// Package store owns the SQLite database behind ws-server.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"

	"github.com/Sothatsit/work-stream/internal/api"
)

var (
	ErrEntryNotFound   = errors.New("entry not found")
	ErrMetaNotFound    = errors.New("metadata key not found")
	ErrMetaExists      = errors.New("metadata key already exists")
	ErrMetadataLimit   = errors.New("metadata pair limit reached")
	ErrInvalidDatabase = errors.New("invalid work-stream database")
)

const (
	applicationID       = 0x57535452 // "WSTR"
	databaseVersion     = 1
	maxBusyTimeoutMsecs = int64(1<<31 - 1)
	sqliteBusy          = 5
	sqliteLocked        = 6
)

const createEntries = `
CREATE TABLE entries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	created_utc TEXT NOT NULL,
	modified_utc TEXT NOT NULL,
	type TEXT NOT NULL,
	type_folded TEXT COLLATE BINARY
		GENERATED ALWAYS AS (lower(type)) STORED,
	subject TEXT NOT NULL,
	subject_folded TEXT COLLATE BINARY
		GENERATED ALWAYS AS (lower(subject)) STORED,
	body TEXT NOT NULL DEFAULT '',
	body_folded TEXT COLLATE BINARY
		GENERATED ALWAYS AS (lower(body)) STORED
)`

const createMetadata = `
CREATE TABLE entry_metadata (
	entry_id INTEGER NOT NULL REFERENCES entries(id) ON DELETE CASCADE,
	key TEXT NOT NULL,
	value TEXT NOT NULL,
	value_folded TEXT COLLATE BINARY
		GENERATED ALWAYS AS (lower(value)) STORED,
	PRIMARY KEY (entry_id, key)
) WITHOUT ROWID`

const createCreatedIndex = `
CREATE INDEX entries_created_order
ON entries(created_utc DESC, id DESC)`

const createModifiedIndex = `
CREATE INDEX entries_modified_order
ON entries(modified_utc DESC, id DESC)`

const createTypeIndex = `
CREATE INDEX entries_type_search ON entries(type_folded)`

const createSubjectIndex = `
CREATE INDEX entries_subject_search ON entries(subject_folded)`

var schemaStatements = []string{
	createEntries,
	createMetadata,
	createCreatedIndex,
	createModifiedIndex,
	createTypeIndex,
	createSubjectIndex,
}

type schemaObject struct {
	typ   string
	name  string
	table string
	defn  string
}

var expectedSchema = []schemaObject{
	{"index", "entries_created_order", "entries",
		normalizeSchemaSQL(createCreatedIndex)},
	{"index", "entries_modified_order", "entries",
		normalizeSchemaSQL(createModifiedIndex)},
	{"index", "entries_subject_search", "entries",
		normalizeSchemaSQL(createSubjectIndex)},
	{"index", "entries_type_search", "entries",
		normalizeSchemaSQL(createTypeIndex)},
	{"table", "entries", "entries", normalizeSchemaSQL(createEntries)},
	{"table", "entry_metadata", "entry_metadata",
		normalizeSchemaSQL(createMetadata)},
}

type Store struct {
	db *sql.DB
}

func Open(
	ctx context.Context,
	path string,
	busyTimeout time.Duration,
) (_ *Store, retErr error) {
	dsn, err := sqliteDSN(path, busyTimeout)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer func() {
		if retErr != nil {
			db.Close()
		}
	}()

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	if err := prepareDatabase(ctx, db); err != nil {
		return nil, err
	}
	var journalMode string
	if err := db.QueryRowContext(
		ctx, `PRAGMA journal_mode = WAL`,
	).Scan(&journalMode); err != nil {
		return nil, fmt.Errorf("enabling WAL: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		return nil, fmt.Errorf("enabling WAL: got mode %q", journalMode)
	}
	var foreignKeys int
	if err := db.QueryRowContext(
		ctx, `PRAGMA foreign_keys`,
	).Scan(&foreignKeys); err != nil {
		return nil, fmt.Errorf("checking foreign keys: %w", err)
	}
	if foreignKeys != 1 {
		return nil, errors.New("SQLite foreign keys are disabled")
	}
	stream := &Store{db: db}
	if err := stream.Ping(ctx); err != nil {
		return nil, fmt.Errorf("checking database health: %w", err)
	}
	return stream, nil
}

func sqliteDSN(path string, busyTimeout time.Duration) (string, error) {
	if busyTimeout < 0 {
		return "", errors.New("busy timeout must not be negative")
	}
	msecs := busyTimeout / time.Millisecond
	if busyTimeout%time.Millisecond != 0 {
		msecs++
	}
	if int64(msecs) > maxBusyTimeoutMsecs {
		return "", errors.New("busy timeout is too large")
	}

	fileURL := url.URL{Scheme: "file", Path: path}
	query := url.Values{}
	query.Set("mode", "rwc")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", msecs))
	fileURL.RawQuery = query.Encode()
	return fileURL.String(), nil
}

func prepareDatabase(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("checking database: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("locking database schema: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			conn.ExecContext(context.Background(), `ROLLBACK`)
		}
	}()

	var application, version, objectCount int
	if err := conn.QueryRowContext(
		ctx, `PRAGMA application_id`,
	).Scan(&application); err != nil {
		return fmt.Errorf("reading application_id: %w", err)
	}
	if err := conn.QueryRowContext(
		ctx, `PRAGMA user_version`,
	).Scan(&version); err != nil {
		return fmt.Errorf("reading user_version: %w", err)
	}
	if err := conn.QueryRowContext(
		ctx, `SELECT count(*) FROM sqlite_schema`,
	).Scan(&objectCount); err != nil {
		return fmt.Errorf("reading database schema: %w", err)
	}

	if objectCount == 0 && application == 0 && version == 0 {
		if err := createDatabase(ctx, conn); err != nil {
			return err
		}
	} else if err := validateDatabase(
		ctx, conn, application, version, objectCount,
	); err != nil {
		return err
	}

	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("committing database schema: %w", err)
	}
	committed = true
	return nil
}

func createDatabase(ctx context.Context, conn *sql.Conn) error {
	for _, statement := range schemaStatements {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("creating database schema: %w", err)
		}
	}
	if _, err := conn.ExecContext(
		ctx, fmt.Sprintf(`PRAGMA application_id = %d`, applicationID),
	); err != nil {
		return fmt.Errorf("setting application_id: %w", err)
	}
	if _, err := conn.ExecContext(
		ctx, fmt.Sprintf(`PRAGMA user_version = %d`, databaseVersion),
	); err != nil {
		return fmt.Errorf("setting user_version: %w", err)
	}
	objects, err := readSchema(ctx, conn)
	if err != nil {
		return err
	}
	if !sameSchema(objects, expectedSchema) {
		return fmt.Errorf("%w: newly-created schema does not match v1",
			ErrInvalidDatabase)
	}
	return nil
}

func validateDatabase(
	ctx context.Context,
	conn *sql.Conn,
	application int,
	version int,
	objectCount int,
) error {
	if objectCount == 0 {
		return fmt.Errorf(
			"%w: empty database has application_id %d and version %d",
			ErrInvalidDatabase, application, version,
		)
	}
	if application == 0 && version == 0 {
		return fmt.Errorf("%w: non-empty database is unversioned",
			ErrInvalidDatabase)
	}
	if application != applicationID {
		return fmt.Errorf("%w: application_id is %d, want %d",
			ErrInvalidDatabase, application, applicationID)
	}
	if version == 0 {
		return fmt.Errorf("%w: database is unversioned", ErrInvalidDatabase)
	}
	if version > databaseVersion {
		return fmt.Errorf("%w: database version %d is newer than %d",
			ErrInvalidDatabase, version, databaseVersion)
	}
	if version != databaseVersion {
		return fmt.Errorf("%w: database version %d is unsupported",
			ErrInvalidDatabase, version)
	}
	objects, err := readSchema(ctx, conn)
	if err != nil {
		return err
	}
	if !sameSchema(objects, expectedSchema) {
		return fmt.Errorf("%w: schema does not match database v%d",
			ErrInvalidDatabase, databaseVersion)
	}
	return nil
}

func readSchema(ctx context.Context, conn *sql.Conn) ([]schemaObject, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT type, name, tbl_name, sql
		FROM sqlite_schema
		WHERE substr(name, 1, 7) <> 'sqlite_'
		ORDER BY type, name`)
	if err != nil {
		return nil, fmt.Errorf("reading database schema: %w", err)
	}
	defer rows.Close()
	var objects []schemaObject
	for rows.Next() {
		var object schemaObject
		var definition sql.NullString
		if err := rows.Scan(
			&object.typ, &object.name, &object.table, &definition,
		); err != nil {
			return nil, fmt.Errorf("reading database schema: %w", err)
		}
		object.defn = normalizeSchemaSQL(definition.String)
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading database schema: %w", err)
	}
	return objects, nil
}

func normalizeSchemaSQL(definition string) string {
	definition = strings.TrimSpace(definition)
	definition = strings.TrimSuffix(definition, ";")
	return strings.Join(strings.Fields(definition), " ")
}

func sameSchema(got, want []schemaObject) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("starting database health check: %w", err)
	}
	defer tx.Rollback()

	var result string
	if err := tx.QueryRowContext(
		ctx, `PRAGMA quick_check(1)`,
	).Scan(&result); err != nil {
		return fmt.Errorf("checking database integrity: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("database integrity check failed: %s", result)
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("checking database foreign keys: %w", err)
	}
	if rows.Next() {
		rows.Close()
		return errors.New("database foreign key check failed")
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("checking database foreign keys: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("closing database health check: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("finishing database health check: %w", err)
	}
	return nil
}

func IsBusy(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() & 0xff {
	case sqliteBusy, sqliteLocked:
		return true
	default:
		return false
	}
}

const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func nowUTC() string {
	return time.Now().UTC().Format(timeLayout)
}

func (s *Store) Add(
	ctx context.Context,
	req api.AddEntryRequest,
) (api.Entry, error) {
	if len(req.Metadata) > api.MaxMetadata {
		return api.Entry{}, ErrMetadataLimit
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Entry{}, fmt.Errorf("adding entry: %w", err)
	}
	defer tx.Rollback()

	now := nowUTC()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO entries (
			created_utc, modified_utc, type, subject, body
		) VALUES (?, ?, ?, ?, ?)`,
		now, now, req.Type, req.Subject, req.Body,
	)
	if err != nil {
		return api.Entry{}, fmt.Errorf("adding entry: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return api.Entry{}, fmt.Errorf("reading new entry id: %w", err)
	}
	for key, value := range req.Metadata {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO entry_metadata (entry_id, key, value)
			VALUES (?, ?, ?)`, id, key, value,
		); err != nil {
			return api.Entry{}, fmt.Errorf("adding entry metadata: %w", err)
		}
	}
	entry, err := getEntry(ctx, tx, id)
	if err != nil {
		return api.Entry{}, fmt.Errorf("reading new entry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return api.Entry{}, fmt.Errorf("committing new entry: %w", err)
	}
	return entry, nil
}

func (s *Store) Get(ctx context.Context, id int64) (api.Entry, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return api.Entry{}, fmt.Errorf("getting entry: %w", err)
	}
	defer tx.Rollback()
	entry, err := getEntry(ctx, tx, id)
	if err != nil {
		return api.Entry{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.Entry{}, fmt.Errorf("finishing entry read: %w", err)
	}
	return entry, nil
}

func (s *Store) Edit(
	ctx context.Context,
	id int64,
	req api.EditEntryRequest,
) (api.Entry, error) {
	if req.Subject == nil && req.Body == nil {
		return api.Entry{}, errors.New("nothing to edit")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Entry{}, fmt.Errorf("editing entry: %w", err)
	}
	defer tx.Rollback()

	query := `UPDATE entries SET modified_utc = ?`
	args := []any{nowUTC()}
	if req.Subject != nil {
		query += `, subject = ?`
		args = append(args, *req.Subject)
	}
	if req.Body != nil {
		query += `, body = ?`
		args = append(args, *req.Body)
	}
	query += ` WHERE id = ?`
	args = append(args, id)
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return api.Entry{}, fmt.Errorf("editing entry: %w", err)
	}
	matched, err := result.RowsAffected()
	if err != nil {
		return api.Entry{}, fmt.Errorf("checking edited entry: %w", err)
	}
	if matched == 0 {
		return api.Entry{}, ErrEntryNotFound
	}
	entry, err := getEntry(ctx, tx, id)
	if err != nil {
		return api.Entry{}, fmt.Errorf("reading edited entry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return api.Entry{}, fmt.Errorf("committing edited entry: %w", err)
	}
	return entry, nil
}

func (s *Store) Delete(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("deleting entry: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(
		ctx, `DELETE FROM entries WHERE id = ?`, id,
	)
	if err != nil {
		return fmt.Errorf("deleting entry: %w", err)
	}
	matched, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking deleted entry: %w", err)
	}
	if matched == 0 {
		return ErrEntryNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing deleted entry: %w", err)
	}
	return nil
}

func touchEntry(
	ctx context.Context,
	tx *sql.Tx,
	id int64,
) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE entries SET modified_utc = ? WHERE id = ?`, nowUTC(), id,
	)
	if err != nil {
		return err
	}
	matched, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if matched == 0 {
		return ErrEntryNotFound
	}
	return nil
}

func entryExists(ctx context.Context, tx *sql.Tx, id int64) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM entries WHERE id = ?)`, id,
	).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return ErrEntryNotFound
	}
	return nil
}

func (s *Store) AddMeta(
	ctx context.Context,
	id int64,
	key string,
	value string,
) (api.Entry, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Entry{}, fmt.Errorf("adding metadata: %w", err)
	}
	defer tx.Rollback()
	if err := touchEntry(ctx, tx, id); err != nil {
		return api.Entry{}, err
	}
	var count int
	var exists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*), coalesce(max(key = ?), 0)
		FROM entry_metadata WHERE entry_id = ?`, key, id,
	).Scan(&count, &exists); err != nil {
		return api.Entry{}, fmt.Errorf("checking metadata: %w", err)
	}
	if exists {
		return api.Entry{}, ErrMetaExists
	}
	if count >= api.MaxMetadata {
		return api.Entry{}, ErrMetadataLimit
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO entry_metadata (entry_id, key, value)
		VALUES (?, ?, ?)`, id, key, value,
	); err != nil {
		return api.Entry{}, fmt.Errorf("adding metadata: %w", err)
	}
	entry, err := getEntry(ctx, tx, id)
	if err != nil {
		return api.Entry{}, fmt.Errorf("reading changed entry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return api.Entry{}, fmt.Errorf("committing metadata: %w", err)
	}
	return entry, nil
}

func (s *Store) EditMeta(
	ctx context.Context,
	id int64,
	key string,
	value string,
) (api.Entry, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Entry{}, fmt.Errorf("editing metadata: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		UPDATE entry_metadata SET value = ?
		WHERE entry_id = ? AND key = ?`, value, id, key,
	)
	if err != nil {
		return api.Entry{}, fmt.Errorf("editing metadata: %w", err)
	}
	matched, err := result.RowsAffected()
	if err != nil {
		return api.Entry{}, fmt.Errorf("checking edited metadata: %w", err)
	}
	if matched == 0 {
		if err := entryExists(ctx, tx, id); err != nil {
			return api.Entry{}, err
		}
		return api.Entry{}, ErrMetaNotFound
	}
	if err := touchEntry(ctx, tx, id); err != nil {
		return api.Entry{}, err
	}
	entry, err := getEntry(ctx, tx, id)
	if err != nil {
		return api.Entry{}, fmt.Errorf("reading changed entry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return api.Entry{}, fmt.Errorf("committing metadata: %w", err)
	}
	return entry, nil
}

func (s *Store) RemoveMeta(
	ctx context.Context,
	id int64,
	key string,
) (api.Entry, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return api.Entry{}, fmt.Errorf("removing metadata: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		DELETE FROM entry_metadata WHERE entry_id = ? AND key = ?`,
		id, key,
	)
	if err != nil {
		return api.Entry{}, fmt.Errorf("removing metadata: %w", err)
	}
	matched, err := result.RowsAffected()
	if err != nil {
		return api.Entry{}, fmt.Errorf("checking removed metadata: %w", err)
	}
	if matched == 0 {
		if err := entryExists(ctx, tx, id); err != nil {
			return api.Entry{}, err
		}
		return api.Entry{}, ErrMetaNotFound
	}
	if err := touchEntry(ctx, tx, id); err != nil {
		return api.Entry{}, err
	}
	entry, err := getEntry(ctx, tx, id)
	if err != nil {
		return api.Entry{}, fmt.Errorf("reading changed entry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return api.Entry{}, fmt.Errorf("committing metadata: %w", err)
	}
	return entry, nil
}

type databaseReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getEntry(
	ctx context.Context,
	db databaseReader,
	id int64,
) (api.Entry, error) {
	row := db.QueryRowContext(ctx, `
		SELECT id, created_utc, modified_utc, type, subject, body
		FROM entries WHERE id = ?`, id,
	)
	entry, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return api.Entry{}, ErrEntryNotFound
	}
	if err != nil {
		return api.Entry{}, err
	}
	metadata, err := metadataFor(ctx, db, id)
	if err != nil {
		return api.Entry{}, err
	}
	entry.Metadata = metadata
	return entry, nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanEntry(row rowScanner) (api.Entry, error) {
	var entry api.Entry
	var created, modified string
	err := row.Scan(
		&entry.ID, &created, &modified,
		&entry.Type, &entry.Subject, &entry.Body,
	)
	if err != nil {
		return api.Entry{}, err
	}
	if err := setEntryTimes(&entry, created, modified); err != nil {
		return api.Entry{}, err
	}
	return entry, nil
}

func setEntryTimes(entry *api.Entry, created, modified string) error {
	var err error
	entry.Created, err = time.Parse(time.RFC3339, created)
	if err != nil {
		return fmt.Errorf("parsing created_utc: %w", err)
	}
	entry.Modified, err = time.Parse(time.RFC3339, modified)
	if err != nil {
		return fmt.Errorf("parsing modified_utc: %w", err)
	}
	return nil
}

func metadataFor(
	ctx context.Context,
	db databaseReader,
	id int64,
) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT key, value FROM entry_metadata
		WHERE entry_id = ? ORDER BY key`, id,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	metadata := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		metadata[key] = value
	}
	return metadata, rows.Err()
}
