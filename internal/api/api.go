// Package api defines the JSON types shared between the ws CLI and
// the ws-server HTTP API.
package api

import "time"

const MaxResponseBytes = 32 << 20

type Entry struct {
	ID       int64             `json:"id"`
	Created  time.Time         `json:"created"`
	Modified time.Time         `json:"modified"`
	Type     string            `json:"type"`
	Subject  string            `json:"subject"`
	Body     string            `json:"body,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

type AddEntryRequest struct {
	Type     string            `json:"type"`
	Subject  string            `json:"subject"`
	Body     string            `json:"body,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// EditEntryRequest uses pointers so a field can be updated to the
// empty string (e.g. clearing the body) while absent fields are left
// untouched. Metadata is edited through the dedicated meta endpoints.
type EditEntryRequest struct {
	Subject *string `json:"subject,omitempty"`
	Body    *string `json:"body,omitempty"`
}

// MetaRequest carries a metadata pair. The key is omitted on PUT,
// where it comes from the URL path.
type MetaRequest struct {
	Key   string `json:"key,omitempty"`
	Value string `json:"value"`
}

type SearchResult struct {
	Entries []Entry `json:"entries"`
	Total   int     `json:"total"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

type StatusResponse struct {
	Version        string `json:"version"`
	Database       string `json:"database"`
	Data           string `json:"data"`
	Timeout        string `json:"timeout"`
	Authentication bool   `json:"authentication"`
}
