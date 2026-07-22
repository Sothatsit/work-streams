// ws-server hosts one work-stream database for remote ws clients.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"
	"time"

	"github.com/Sothatsit/work-stream/internal/api"
	"github.com/Sothatsit/work-stream/internal/store"
	"github.com/Sothatsit/work-stream/internal/version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	cfg, err := parseConfig(args, os.Getenv)
	if err != nil {
		return err
	}
	address := ":" + cfg.port
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", address, err)
	}
	defer listener.Close()

	if err := os.MkdirAll(cfg.data, 0o700); err != nil {
		return fmt.Errorf("creating data directory: %w", err)
	}
	databasePath := filepath.Join(cfg.data, "ws.db")
	workTimeout := serverWorkTimeout(cfg.timeout)
	openContext, cancel := context.WithTimeout(
		context.Background(), cfg.timeout,
	)
	stream, err := store.Open(openContext, databasePath, workTimeout)
	cancel()
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer stream.Close()

	h := &handler{
		store:          stream,
		data:           cfg.data,
		timeout:        cfg.timeout,
		authentication: cfg.secret != "",
	}
	mux := routes(h)
	root := requireAPIVersion(authenticate(cfg.secret,
		requestTimeout(workTimeout,
			limitRequestBody(logRequests(mux)))))
	server := &http.Server{
		Handler:           root,
		ReadTimeout:       cfg.timeout,
		ReadHeaderTimeout: cfg.timeout,
		WriteTimeout:      cfg.timeout,
		IdleTimeout:       cfg.timeout,
		MaxHeaderBytes:    1 << 20,
	}
	shutdownSignal, stopSignals := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
	defer stopSignals()
	go func() {
		<-shutdownSignal.Done()
		shutdownContext, cancel := context.WithTimeout(
			context.Background(), cfg.timeout,
		)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			log.Printf("server shutdown failed: %v", err)
		}
	}()
	log.Printf("ws-server %s listening on %s (data: %s)",
		version.Software, listener.Addr(), cfg.data)
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func serverWorkTimeout(total time.Duration) time.Duration {
	margin := total / 10
	if margin > 500*time.Millisecond {
		margin = 500 * time.Millisecond
	}
	return total - margin
}

func routes(h *handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", h.status)
	mux.HandleFunc("POST /api/entries", h.addEntry)
	mux.HandleFunc("GET /api/entries", h.search)
	mux.HandleFunc("GET /api/entries/{id}", h.getEntry)
	mux.HandleFunc("PATCH /api/entries/{id}", h.editEntry)
	mux.HandleFunc("DELETE /api/entries/{id}", h.deleteEntry)
	mux.HandleFunc("POST /api/entries/{id}/metadata", h.addMeta)
	mux.HandleFunc("PUT /api/entries/{id}/metadata/{key}", h.editMeta)
	mux.HandleFunc("DELETE /api/entries/{id}/metadata/{key}", h.removeMeta)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound,
			api.ErrorResponse{Error: "route not found"})
	})
	return mux
}

type handler struct {
	store          *store.Store
	data           string
	timeout        time.Duration
	authentication bool
}

func (h *handler) status(w http.ResponseWriter, r *http.Request) {
	if !requireNoQuery(w, r) {
		return
	}
	if err := h.store.Ping(r.Context()); err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.StatusResponse{
		Version:        version.Software,
		Database:       "ok",
		Data:           h.data,
		Timeout:        h.timeout.String(),
		Authentication: h.authentication,
	})
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeBadRequest(w, "invalid entry id: "+r.PathValue("id"))
		return 0, false
	}
	return id, true
}

func (h *handler) addEntry(w http.ResponseWriter, r *http.Request) {
	if !requireNoQuery(w, r) {
		return
	}
	var request api.AddEntryRequest
	if !decodeBody(w, r, &request) {
		return
	}
	if err := request.Validate(); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	entry, err := h.store.Add(r.Context(), request)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

func (h *handler) getEntry(w http.ResponseWriter, r *http.Request) {
	if !requireNoQuery(w, r) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	entry, err := h.store.Get(r.Context(), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (h *handler) editEntry(w http.ResponseWriter, r *http.Request) {
	if !requireNoQuery(w, r) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var request api.EditEntryRequest
	if !decodeBody(w, r, &request) {
		return
	}
	if err := request.Validate(); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	entry, err := h.store.Edit(r.Context(), id, request)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (h *handler) deleteEntry(w http.ResponseWriter, r *http.Request) {
	if !requireNoQuery(w, r) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := h.store.Delete(r.Context(), id); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) addMeta(w http.ResponseWriter, r *http.Request) {
	if !requireNoQuery(w, r) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var request api.MetaRequest
	if !decodeBody(w, r, &request) {
		return
	}
	if err := api.ValidateMeta(request.Key, request.Value); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	entry, err := h.store.AddMeta(
		r.Context(), id, request.Key, request.Value,
	)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

type metaValueRequest struct {
	Value string `json:"value"`
}

func (h *handler) editMeta(w http.ResponseWriter, r *http.Request) {
	if !requireNoQuery(w, r) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var request metaValueRequest
	if !decodeBody(w, r, &request) {
		return
	}
	key := r.PathValue("key")
	if err := api.ValidateMeta(key, request.Value); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	entry, err := h.store.EditMeta(r.Context(), id, key, request.Value)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (h *handler) removeMeta(w http.ResponseWriter, r *http.Request) {
	if !requireNoQuery(w, r) {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	if err := api.ValidateKey(key); err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	entry, err := h.store.RemoveMeta(r.Context(), id, key)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

var repeatedQueryParameters = map[string]bool{
	"subject":    true,
	"no-subject": true,
	"body":       true,
	"no-body":    true,
	"type":       true,
	"no-type":    true,
	"key":        true,
	"no-key":     true,
	"content":    true,
	"no-content": true,
	"meta":       true,
	"no-meta":    true,
}

var singleQueryParameters = map[string]bool{
	"limit":    true,
	"offset":   true,
	"order-by": true,
}

func validateSearchQuery(
	w http.ResponseWriter, query map[string][]string,
) bool {
	keys := make([]string, 0, len(query))
	conditionCount := 0
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		values := query[key]
		switch {
		case repeatedQueryParameters[key]:
			conditionCount += len(values)
		case singleQueryParameters[key]:
			if len(values) != 1 {
				writeBadRequest(w, key+" must be given once")
				return false
			}
		default:
			writeBadRequest(w,
				fmt.Sprintf("unknown query parameter %q", key))
			return false
		}
	}
	if conditionCount > store.MaxSearchConditions {
		message := fmt.Sprintf(
			"at most %d search conditions are allowed",
			store.MaxSearchConditions,
		)
		writeBadRequest(w, message)
		return false
	}
	return true
}

func fieldConditions(
	query map[string][]string, field string,
) []store.FieldCond {
	conditions := make([]store.FieldCond, 0,
		len(query[field])+len(query["no-"+field]))
	for _, value := range query[field] {
		conditions = append(conditions, store.FieldCond{Value: value})
	}
	for _, value := range query["no-"+field] {
		conditions = append(conditions,
			store.FieldCond{Negate: true, Value: value})
	}
	return conditions
}

func metaConditions(query map[string][]string) ([]store.MetaCond, error) {
	conditions := make([]store.MetaCond, 0,
		len(query["meta"])+len(query["no-meta"]))
	for _, parameter := range []struct {
		name   string
		negate bool
	}{
		{"meta", false},
		{"no-meta", true},
	} {
		for _, value := range query[parameter.name] {
			key, metadataValue, err := splitMeta(value)
			if err != nil {
				return nil, err
			}
			conditions = append(conditions, store.MetaCond{
				Negate: parameter.negate,
				Key:    key,
				Value:  metadataValue,
			})
		}
	}
	return conditions, nil
}

func searchInteger(
	query map[string][]string, key string, defaultValue int,
) (int, error) {
	values := query[key]
	if len(values) == 0 {
		return defaultValue, nil
	}
	if values[0] == "" {
		return 0, fmt.Errorf("%s requires a value", key)
	}
	value, err := strconv.Atoi(values[0])
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return value, nil
}

func (h *handler) search(w http.ResponseWriter, r *http.Request) {
	query, ok := parseQuery(w, r)
	if !ok || !validateSearchQuery(w, query) {
		return
	}
	metadata, err := metaConditions(query)
	if err != nil {
		writeBadRequest(w, err.Error())
		return
	}
	limit, err := searchInteger(query, "limit", 50)
	if err != nil || limit < 1 || limit > 500 {
		if err != nil {
			writeBadRequest(w, err.Error())
		} else {
			writeBadRequest(w, "limit must be between 1 and 500")
		}
		return
	}
	offset, err := searchInteger(query, "offset", 0)
	if err != nil || offset < 0 {
		if err != nil {
			writeBadRequest(w, err.Error())
		} else {
			writeBadRequest(w, "offset must be non-negative")
		}
		return
	}
	filter := store.Filter{
		Subject: fieldConditions(query, "subject"),
		Body:    fieldConditions(query, "body"),
		Type:    fieldConditions(query, "type"),
		Key:     fieldConditions(query, "key"),
		Content: fieldConditions(query, "content"),
		Meta:    metadata,
		Limit:   limit,
		Offset:  offset,
	}
	if values := query["order-by"]; len(values) != 0 {
		switch values[0] {
		case "created":
		case "modified":
			filter.OrderByModified = true
		default:
			writeBadRequest(w, "order-by must be 'created' or 'modified'")
			return
		}
	}
	result, err := h.store.Search(r.Context(), filter)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
