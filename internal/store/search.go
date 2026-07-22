package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Sothatsit/work-stream/internal/api"
)

const (
	MaxSearchConditions    = 32
	MaxSearchLimit         = 500
	MaxSearchPatternLength = 4096
)

var ErrInvalidFilter = errors.New("invalid search filter")

type FieldCond struct {
	Negate bool
	Value  string
}

type MetaCond struct {
	Negate bool
	Key    string
	Value  string
}

type Filter struct {
	Subject []FieldCond
	Body    []FieldCond
	Type    []FieldCond
	Key     []FieldCond
	Content []FieldCond
	Meta    []MetaCond

	OrderByModified bool
	Limit           int
	Offset          int
}

type whereBuilder struct {
	clauses []string
	args    []any
}

func conditionCount(filter Filter) int {
	return len(filter.Subject) + len(filter.Body) + len(filter.Type) +
		len(filter.Key) + len(filter.Content) + len(filter.Meta)
}

func (builder *whereBuilder) addField(
	name string,
	column string,
	conditions []FieldCond,
) error {
	for _, condition := range conditions {
		pattern, err := foldedPattern(name, condition.Value)
		if err != nil {
			return err
		}
		clause := column + ` GLOB ?`
		if condition.Negate {
			clause = `NOT (` + clause + `)`
		}
		builder.clauses = append(builder.clauses, clause)
		builder.args = append(builder.args, pattern)
	}
	return nil
}

func (builder *whereBuilder) addKeys(conditions []FieldCond) error {
	for _, condition := range conditions {
		pattern, err := foldedPattern("key", condition.Value)
		if err != nil {
			return err
		}
		clause := `EXISTS (
			SELECT 1 FROM entry_metadata AS key_meta
			WHERE key_meta.entry_id = e.id AND key_meta.key GLOB ?
		)`
		if condition.Negate {
			clause = `NOT ` + clause
		}
		builder.clauses = append(builder.clauses, clause)
		builder.args = append(builder.args, pattern)
	}
	return nil
}

func (builder *whereBuilder) addContent(
	conditions []FieldCond,
) error {
	for _, condition := range conditions {
		pattern, err := foldedPattern("content", condition.Value)
		if err != nil {
			return err
		}
		clause := `(e.subject_folded GLOB ? OR e.body_folded GLOB ?)`
		if condition.Negate {
			clause = `NOT ` + clause
		}
		builder.clauses = append(builder.clauses, clause)
		builder.args = append(builder.args, pattern, pattern)
	}
	return nil
}

func (builder *whereBuilder) addMetadata(conditions []MetaCond) error {
	for _, condition := range conditions {
		keyPattern, err := foldedPattern("metadata key", condition.Key)
		if err != nil {
			return err
		}
		valuePattern, err := foldedPattern(
			"metadata value", condition.Value,
		)
		if err != nil {
			return err
		}
		clause := `EXISTS (
			SELECT 1 FROM entry_metadata AS pair_meta
			WHERE pair_meta.entry_id = e.id
				AND pair_meta.key GLOB ?
				AND pair_meta.value_folded GLOB ?
		)`
		if condition.Negate {
			clause = `NOT ` + clause
		}
		builder.clauses = append(builder.clauses, clause)
		builder.args = append(
			builder.args, keyPattern, valuePattern,
		)
	}
	return nil
}

func buildWhere(filter Filter) (whereBuilder, error) {
	var builder whereBuilder
	fields := []struct {
		name       string
		column     string
		conditions []FieldCond
	}{
		{"subject", "e.subject_folded", filter.Subject},
		{"body", "e.body_folded", filter.Body},
		{"type", "e.type_folded", filter.Type},
	}
	for _, field := range fields {
		if err := builder.addField(
			field.name, field.column, field.conditions,
		); err != nil {
			return whereBuilder{}, err
		}
	}
	if err := builder.addKeys(filter.Key); err != nil {
		return whereBuilder{}, err
	}
	if err := builder.addContent(filter.Content); err != nil {
		return whereBuilder{}, err
	}
	if err := builder.addMetadata(filter.Meta); err != nil {
		return whereBuilder{}, err
	}
	return builder, nil
}

func (builder whereBuilder) sql() string {
	if len(builder.clauses) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(builder.clauses, " AND ")
}

func foldedPattern(name, pattern string) (string, error) {
	if strings.IndexByte(pattern, 0) >= 0 {
		return "", fmt.Errorf("%w: %s pattern contains NUL",
			ErrInvalidFilter, name)
	}
	if utf8.RuneCountInString(pattern) > MaxSearchPatternLength {
		return "", fmt.Errorf(
			"%w: %s pattern exceeds %d characters",
			ErrInvalidFilter, name, MaxSearchPatternLength,
		)
	}
	return foldASCII(pattern), nil
}

func foldASCII(value string) string {
	var folded []byte
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character < 'A' || character > 'Z' {
			continue
		}
		if folded == nil {
			folded = []byte(value)
		}
		folded[index] = character + ('a' - 'A')
	}
	if folded == nil {
		return value
	}
	return string(folded)
}

func (s *Store) Search(
	ctx context.Context,
	filter Filter,
) (api.SearchResult, error) {
	if conditionCount(filter) > MaxSearchConditions {
		return api.SearchResult{}, fmt.Errorf(
			"%w: at most %d search conditions are allowed",
			ErrInvalidFilter, MaxSearchConditions,
		)
	}
	if filter.Limit < 1 || filter.Limit > MaxSearchLimit {
		return api.SearchResult{}, fmt.Errorf(
			"%w: limit must be between 1 and %d",
			ErrInvalidFilter, MaxSearchLimit,
		)
	}
	if filter.Offset < 0 {
		return api.SearchResult{}, fmt.Errorf(
			"%w: offset must not be negative", ErrInvalidFilter,
		)
	}
	where, err := buildWhere(filter)
	if err != nil {
		return api.SearchResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return api.SearchResult{}, fmt.Errorf("starting search: %w", err)
	}
	defer tx.Rollback()

	var total int
	countQuery := `SELECT count(*) FROM entries AS e` + where.sql()
	if err := tx.QueryRowContext(
		ctx, countQuery, where.args...,
	).Scan(&total); err != nil {
		return api.SearchResult{}, fmt.Errorf("counting search results: %w", err)
	}

	orderColumn := "created_utc"
	if filter.OrderByModified {
		orderColumn = "modified_utc"
	}
	pageQuery := fmt.Sprintf(`
		WITH page AS (
			SELECT e.id, e.created_utc, e.modified_utc,
				e.type, e.subject, e.body
			FROM entries AS e%s
			ORDER BY e.%s DESC, e.id DESC
			LIMIT ? OFFSET ?
		)
		SELECT page.id, page.created_utc, page.modified_utc,
			page.type, page.subject, page.body,
			metadata.key, metadata.value
		FROM page
		LEFT JOIN entry_metadata AS metadata
			ON metadata.entry_id = page.id
		ORDER BY page.%s DESC, page.id DESC, metadata.key`,
		where.sql(), orderColumn, orderColumn,
	)
	pageArgs := append([]any{}, where.args...)
	pageArgs = append(pageArgs, filter.Limit, filter.Offset)
	rows, err := tx.QueryContext(ctx, pageQuery, pageArgs...)
	if err != nil {
		return api.SearchResult{}, fmt.Errorf("reading search results: %w", err)
	}
	entries, err := scanSearchRows(rows)
	if err != nil {
		return api.SearchResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return api.SearchResult{}, fmt.Errorf("finishing search: %w", err)
	}
	return api.SearchResult{Entries: entries, Total: total}, nil
}

func scanSearchRows(rows *sql.Rows) ([]api.Entry, error) {
	defer rows.Close()
	var entries []api.Entry
	for rows.Next() {
		var entry api.Entry
		var created, modified string
		var key, value sql.NullString
		if err := rows.Scan(
			&entry.ID, &created, &modified,
			&entry.Type, &entry.Subject, &entry.Body,
			&key, &value,
		); err != nil {
			return nil, fmt.Errorf("reading search result: %w", err)
		}
		if err := setEntryTimes(&entry, created, modified); err != nil {
			return nil, err
		}
		if len(entries) == 0 || entries[len(entries)-1].ID != entry.ID {
			entry.Metadata = map[string]string{}
			entries = append(entries, entry)
		}
		if key.Valid != value.Valid {
			return nil, errors.New("metadata key/value NULL mismatch")
		}
		if key.Valid {
			entries[len(entries)-1].Metadata[key.String] = value.String
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading search results: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("closing search results: %w", err)
	}
	return entries, nil
}
