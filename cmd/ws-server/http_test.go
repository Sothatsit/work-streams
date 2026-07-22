package main

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Sothatsit/work-stream/internal/version"
)

func TestAPIAuthenticationDoesNotLogSecret(t *testing.T) {
	secret := "correct-horse-battery-staple"
	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(oldOutput) })

	endpoint := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h := requireAPIVersion(authenticate(secret, logRequests(endpoint)))

	request := httptest.NewRequest(http.MethodPost, "/api/entries",
		strings.NewReader(`{"type":"note"}`))
	request.Header.Set(version.APIHeader, strconv.Itoa(version.API))
	request.Header.Set("Authorization", "Bearer "+secret)
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want %d", response.Code,
			http.StatusNoContent)
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatal("request log contains the authentication secret")
	}
	if got := response.Header().Get(version.APIHeader); got != "1" {
		t.Fatalf("response API version: got %q, want 1", got)
	}
}

func TestDisabledAuthenticationRejectsCredential(t *testing.T) {
	endpoint := http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	request.Header.Set("Authorization", "Bearer stale-secret")
	response := httptest.NewRecorder()
	authenticate("", endpoint).ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want %d",
			response.Code, http.StatusBadRequest)
	}
}

func TestStrictJSONBody(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"unknown field", `{"value":"x","extra":true}`},
		{"second value", `{"value":"x"} {"value":"y"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, "/",
				strings.NewReader(test.body))
			response := httptest.NewRecorder()
			var value metaValueRequest
			if decodeBody(response, request, &value) {
				t.Fatal("decodeBody accepted invalid JSON")
			}
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d, want %d", response.Code,
					http.StatusBadRequest)
			}
		})
	}
}
