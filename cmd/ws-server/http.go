package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Sothatsit/work-stream/internal/api"
	"github.com/Sothatsit/work-stream/internal/store"
	"github.com/Sothatsit/work-stream/internal/version"
)

const (
	maxRequestBytes  = int64(1 << 20)
	maxRecordedBytes = 1 << 20
)

func requireAPIVersion(next http.Handler) http.Handler {
	serverVersion := strconv.Itoa(version.API)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(version.APIHeader, serverVersion)
		values := r.Header.Values(version.APIHeader)
		if len(values) == 0 {
			writeBadRequest(w, "missing "+version.APIHeader+" header")
			return
		}
		if len(values) != 1 {
			writeBadRequest(w, version.APIHeader+" must be given once")
			return
		}
		got, err := strconv.Atoi(values[0])
		if err != nil {
			writeBadRequest(w, version.APIHeader+" must be an integer")
			return
		}
		if got != version.API {
			writeBadRequest(w, fmt.Sprintf(
				"unsupported API version %d; server uses %d",
				got, version.API,
			))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func authenticate(secret string, next http.Handler) http.Handler {
	if secret == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(r.Header.Values("Authorization")) != 0 {
				writeBadRequest(w, "server authentication is disabled")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	expected := sha256.Sum256([]byte("Bearer " + secret))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values("Authorization")
		provided := ""
		if len(values) == 1 {
			provided = values[0]
		}
		actual := sha256.Sum256([]byte(provided))
		match := subtle.ConstantTimeCompare(actual[:], expected[:])
		if len(values) != 1 || match != 1 {
			w.Header().Set(
				"WWW-Authenticate", `Bearer realm="work-stream"`,
			)
			writeJSON(w, http.StatusUnauthorized,
				api.ErrorResponse{Error: "authentication failed"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requestTimeout(timeout time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func limitRequestBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		}
		next.ServeHTTP(w, r)
	})
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isWrite := r.Method != http.MethodGet
		rw := &recordingWriter{
			ResponseWriter: w,
			status:         http.StatusOK,
			record:         isWrite,
		}
		var requestBody []byte
		if isWrite && r.Body != nil {
			var err error
			requestBody, err = io.ReadAll(r.Body)
			if err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					writeJSON(rw, http.StatusRequestEntityTooLarge,
						api.ErrorResponse{Error: "request body exceeds 1 MiB"})
				} else {
					writeBadRequest(rw, "reading request body: "+err.Error())
				}
				logRequest(r, requestBody, rw)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(requestBody))
		}
		next.ServeHTTP(rw, r)
		logRequest(r, requestBody, rw)
	})
}

func logRequest(r *http.Request, requestBody []byte, rw *recordingWriter) {
	if rw.record {
		log.Printf("%s %s %s -> %d %s", r.Method, r.URL.RequestURI(),
			logBody(requestBody, false), rw.status,
			logBody(rw.body.Bytes(), rw.truncated))
		return
	}
	log.Printf("%s %s -> %d", r.Method, r.URL.RequestURI(), rw.status)
}

type recordingWriter struct {
	http.ResponseWriter
	status    int
	record    bool
	wroteHead bool
	truncated bool
	body      bytes.Buffer
}

func (w *recordingWriter) WriteHeader(status int) {
	if w.wroteHead {
		return
	}
	w.wroteHead = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *recordingWriter) Write(body []byte) (int, error) {
	if !w.wroteHead {
		w.WriteHeader(http.StatusOK)
	}
	if w.record {
		remaining := maxRecordedBytes - w.body.Len()
		if remaining > 0 {
			kept := body
			if len(kept) > remaining {
				kept = kept[:remaining]
			}
			_, _ = w.body.Write(kept)
		}
		if len(body) > remaining {
			w.truncated = true
		}
	}
	return w.ResponseWriter.Write(body)
}

func logBody(body []byte, truncated bool) string {
	if len(body) == 0 {
		return "-"
	}
	if truncated {
		return strconv.Quote(string(body)) + " [truncated]"
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		return strconv.Quote(string(body))
	}
	return compact.String()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		log.Printf("encoding response: %v", err)
		status = http.StatusInternalServerError
		body = []byte(`{"error":"encoding response"}`)
	}
	if len(body) > api.MaxResponseBytes {
		log.Printf("response exceeds %d bytes", api.MaxResponseBytes)
		status = http.StatusInternalServerError
		body = []byte(`{"error":"response is too large"}`)
	}
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeStoreError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "internal server error"
	switch {
	case errors.Is(err, store.ErrInvalidFilter):
		status = http.StatusBadRequest
		message = err.Error()
	case errors.Is(err, store.ErrEntryNotFound),
		errors.Is(err, store.ErrMetaNotFound):
		status = http.StatusNotFound
		message = err.Error()
	case errors.Is(err, store.ErrMetaExists):
		status = http.StatusConflict
		message = err.Error()
	case errors.Is(err, store.ErrMetadataLimit):
		status = http.StatusConflict
		message = err.Error()
	case errors.Is(err, context.DeadlineExceeded), store.IsBusy(err):
		status = http.StatusServiceUnavailable
		message = "server is busy or the request timed out"
	case errors.Is(err, context.Canceled):
		status = http.StatusServiceUnavailable
		message = "request canceled"
	default:
		log.Printf("store request failed: %v", err)
	}
	writeJSON(w, status, api.ErrorResponse{Error: message})
}

func writeBadRequest(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusBadRequest, api.ErrorResponse{Error: message})
}

func decodeBody(w http.ResponseWriter, r *http.Request, value any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeBadRequest(w, "invalid request body: "+err.Error())
		return false
	}
	var trailing any
	err := decoder.Decode(&trailing)
	if !errors.Is(err, io.EOF) {
		if err == nil {
			writeBadRequest(w, "request body must contain one JSON value")
		} else {
			writeBadRequest(w, "invalid request body: "+err.Error())
		}
		return false
	}
	return true
}

func parseQuery(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeBadRequest(w, "invalid query: "+err.Error())
		return nil, false
	}
	return query, true
}

func requireNoQuery(w http.ResponseWriter, r *http.Request) bool {
	query, ok := parseQuery(w, r)
	if !ok {
		return false
	}
	for key := range query {
		writeBadRequest(w, fmt.Sprintf("unknown query parameter %q", key))
		return false
	}
	return true
}

func splitMeta(value string) (string, string, error) {
	key, metadataValue, found := strings.Cut(value, "=")
	if !found {
		return "", "", fmt.Errorf("meta filter %q must be key=value", value)
	}
	return key, metadataValue, nil
}
