package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Sothatsit/work-stream/internal/api"
)

func localDate(t time.Time) string {
	return t.Local().Format("2006-01-02")
}

func localClock(t time.Time) string {
	return t.Local().Format("15:04")
}

func localTimeDetail(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04:05 MST")
}

func sortedMetaKeys(md map[string]string) []string {
	keys := make([]string, 0, len(md))
	for key := range md {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// listTime is the timestamp a listing shows: the same one it is
// ordered by, so the date headings and times line up with the sort.
func listTime(e api.Entry, orderByModified bool) time.Time {
	if orderByModified {
		return e.Modified
	}
	return e.Created
}

// printEntryLine prints one log-style line per entry: id, type, and the
// listed time, then the subject. Newlines in the subject are escaped to
// keep the line a line. The date lives in a heading above each day's
// entries (see printSearchResult).
func printEntryLine(e api.Entry, orderByModified bool) {
	subject := strings.ReplaceAll(e.Subject, "\n", "\\n")
	fmt.Printf("[e%d] (%s) %s: %s\n", e.ID, e.Type,
		localClock(listTime(e, orderByModified)), subject)
}

func printOrigin(o api.Origin) {
	if o == (api.Origin{}) {
		return
	}
	location := o.Host
	if o.User != "" {
		location = o.User + "@" + o.Host
	}
	if o.Dir != "" {
		location += ":" + o.Dir
	}
	if location != "" {
		fmt.Println("From: " + location)
	}
	if o.ClaudeSession != "" {
		fmt.Println("  claude session: " + o.ClaudeSession)
	}
}

func printEntryDetail(e api.Entry) {
	fmt.Printf("[e%d] (%s) %s\n\n", e.ID, e.Type, e.Subject)
	fmt.Println("Created: " + localTimeDetail(e.Created))
	fmt.Println("Modified: " + localTimeDetail(e.Modified))
	printOrigin(e.Origin)
	if len(e.Metadata) > 0 {
		fmt.Println("Metadata:")
		for _, key := range sortedMetaKeys(e.Metadata) {
			fmt.Printf("  %s: %s\n", key, e.Metadata[key])
		}
	}
	if e.Body != "" {
		fmt.Println()
		fmt.Println(e.Body)
	}
}

func printSearchResult(
	result api.SearchResult, offset, limit int,
	orderByModified, idOnly bool,
) {
	if idOnly {
		for _, e := range result.Entries {
			fmt.Printf("e%d\n", e.ID)
		}
		return
	}
	if len(result.Entries) == 0 {
		if result.Total > 0 {
			fmt.Printf("No entries at offset %d (%d matched).\n",
				offset, result.Total)
		} else {
			fmt.Println("No matching entries.")
		}
		return
	}
	// A date heading precedes each run of entries sharing a day, so the
	// entry lines themselves only carry the time.
	lastDate := ""
	for _, e := range result.Entries {
		date := localDate(listTime(e, orderByModified))
		if date != lastDate {
			if lastDate != "" {
				fmt.Println()
			}
			fmt.Println(date)
			lastDate = date
		}
		printEntryLine(e, orderByModified)
	}
	remaining := result.Total - offset - len(result.Entries)
	if remaining > 0 {
		fmt.Printf("--- %d more entries (use --offset %d to view older) ---\n",
			remaining, offset+limit)
	}
}
