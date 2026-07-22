package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Sothatsit/work-stream/internal/api"
)

type searchEntries struct {
	store  *Store
	first  api.Entry
	second api.Entry
	third  api.Entry
	fourth api.Entry
	fifth  api.Entry
}

func newSearchEntries(t *testing.T) searchEntries {
	t.Helper()
	store := openTestStore(t)
	return searchEntries{
		store: store,
		first: mustAdd(t, store, "TODO", "Count the Ducklings", "",
			map[string]string{"project": "duck-pond", "jira": "QUACK-1"}),
		second: mustAdd(t, store, "todo", "Fix Nest", "Ducklings are safe",
			map[string]string{"project": "duck-nest"}),
		third: mustAdd(t, store, "note", "Ducklings restless", "",
			map[string]string{"project": "duck-pond", "jira": "QUACK-2"}),
		fourth: mustAdd(t, store, "idea", "Cache feed", "", nil),
		fifth:  mustAdd(t, store, "note", "Æther", "", nil),
	}
}

func runSearch(t *testing.T, store *Store, filter Filter) api.SearchResult {
	t.Helper()
	if filter.Limit == 0 {
		filter.Limit = 50
	}
	result, err := store.Search(context.Background(), filter)
	if err != nil {
		t.Fatalf("searching: %v", err)
	}
	return result
}

func assertEntryIDs(t *testing.T, entries []api.Entry, want ...int64) {
	t.Helper()
	if len(entries) != len(want) {
		t.Fatalf("entry count = %d, want %d: %v", len(entries), len(want), entries)
	}
	for index, entry := range entries {
		if entry.ID != want[index] {
			t.Fatalf("entry %d has id %d, want %d", index, entry.ID, want[index])
		}
	}
}

func TestSearchUsesRawCaseFoldedGlobPatterns(t *testing.T) {
	entries := newSearchEntries(t)
	tests := []struct {
		name    string
		pattern string
		want    []int64
	}{
		{"whole string", "COUNT THE DUCKLINGS", []int64{entries.first.ID}},
		{"wildcard", "*DUCKLINGS*", []int64{
			entries.third.ID, entries.first.ID,
		}},
		{"single character", "fix nes?", []int64{entries.second.ID}},
		{"character class", "[cf]*", []int64{
			entries.fourth.ID, entries.second.ID, entries.first.ID,
		}},
		{"non-ASCII is unchanged", "æther", nil},
		{"matching non-ASCII", "ÆTHER", []int64{entries.fifth.ID}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := runSearch(t, entries.store, Filter{
				Subject: []FieldCond{{Value: test.pattern}},
			})
			assertEntryIDs(t, result.Entries, test.want...)
		})
	}
}

func TestSearchSetAndPairNegationUsesNotExists(t *testing.T) {
	entries := newSearchEntries(t)
	result := runSearch(t, entries.store, Filter{
		Key: []FieldCond{{Value: "JIRA"}},
	})
	assertEntryIDs(t, result.Entries, entries.third.ID, entries.first.ID)

	result = runSearch(t, entries.store, Filter{
		Key: []FieldCond{{Negate: true, Value: "JIRA"}},
	})
	assertEntryIDs(t, result.Entries,
		entries.fifth.ID, entries.fourth.ID, entries.second.ID)

	result = runSearch(t, entries.store, Filter{
		Meta: []MetaCond{{Key: "project", Value: "DUCK-*"}},
	})
	assertEntryIDs(t, result.Entries,
		entries.third.ID, entries.second.ID, entries.first.ID)

	result = runSearch(t, entries.store, Filter{
		Meta: []MetaCond{{Key: "project", Value: "DUCK-1"}},
	})
	assertEntryIDs(t, result.Entries)

	result = runSearch(t, entries.store, Filter{
		Meta: []MetaCond{{Negate: true, Key: "project", Value: "DUCK-POND"}},
	})
	assertEntryIDs(t, result.Entries,
		entries.fifth.ID, entries.fourth.ID, entries.second.ID)
}

func TestSearchContentPagingAndMetadata(t *testing.T) {
	entries := newSearchEntries(t)
	result := runSearch(t, entries.store, Filter{
		Content: []FieldCond{{Value: "*DUCKLINGS*"}},
		Limit:   2,
		Offset:  1,
	})
	if result.Total != 3 {
		t.Fatalf("total = %d, want 3", result.Total)
	}
	assertEntryIDs(t, result.Entries, entries.second.ID, entries.first.ID)
	if got := result.Entries[1].Metadata["jira"]; got != "QUACK-1" {
		t.Fatalf("paged metadata jira = %q, want QUACK-1", got)
	}
	if result.Entries[0].Metadata == nil {
		t.Fatal("entry metadata map is nil")
	}
}

func TestSearchOrderByModifiedIncludesMetadataChanges(t *testing.T) {
	entries := newSearchEntries(t)
	oldTime := "2000-01-01T00:00:00.000000000Z"
	if _, err := entries.store.db.Exec(
		`UPDATE entries SET modified_utc = ?`, oldTime,
	); err != nil {
		t.Fatalf("setting modification times: %v", err)
	}
	if _, err := entries.store.EditMeta(
		context.Background(), entries.first.ID, "jira", "QUACK-9",
	); err != nil {
		t.Fatalf("editing metadata: %v", err)
	}
	result := runSearch(t, entries.store, Filter{OrderByModified: true})
	if result.Entries[0].ID != entries.first.ID {
		t.Fatalf("first modified entry = %d, want %d",
			result.Entries[0].ID, entries.first.ID)
	}
}

func TestSearchRejectsInvalidBoundsAndNUL(t *testing.T) {
	store := openTestStore(t)
	tests := []struct {
		name   string
		filter Filter
		want   string
	}{
		{"zero limit", Filter{}, "limit"},
		{"large limit", Filter{Limit: MaxSearchLimit + 1}, "limit"},
		{"negative offset", Filter{Limit: 1, Offset: -1}, "offset"},
		{"NUL pattern", Filter{
			Limit: 1,
			Body:  []FieldCond{{Value: "bad\x00pattern"}},
		}, "NUL"},
		{"long pattern", Filter{
			Limit: 1,
			Body: []FieldCond{{
				Value: strings.Repeat("x", MaxSearchPatternLength+1),
			}},
		}, "exceeds"},
		{"too many conditions", Filter{
			Limit: 1,
			Subject: make(
				[]FieldCond, MaxSearchConditions+1,
			),
		}, "conditions"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := store.Search(context.Background(), test.filter)
			if !errors.Is(err, ErrInvalidFilter) ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("Search error = %v, want text %q", err, test.want)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := store.Search(ctx, Filter{Limit: 1})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Search error = %v, want context.Canceled", err)
	}
}
