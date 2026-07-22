package main

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/Sothatsit/work-stream/internal/api"
	"github.com/Sothatsit/work-stream/internal/version"
)

func TestClientSendsVersionAndSecret(t *testing.T) {
	t.Setenv(secretEnvironment, "duck-secret")
	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/status" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		wantVersion := strconv.Itoa(version.API)
		if got := r.Header.Values(version.APIHeader); len(got) != 1 ||
			got[0] != wantVersion {
			t.Errorf("%s = %q", version.APIHeader, got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer duck-secret" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set(version.APIHeader, wantVersion)
		_ = json.NewEncoder(w).Encode(api.StatusResponse{
			Version:  version.Software,
			Database: "ok",
		})
	}))
	defer server.Close()

	client := testClient(t, server.URL)
	status, err := client.status()
	if err != nil {
		t.Fatal(err)
	}
	if status.Version != version.Software || status.Database != "ok" {
		t.Fatalf("status = %#v", status)
	}
}

func TestClientRejectsInvalidResponseVersion(t *testing.T) {
	want := strconv.Itoa(version.API)
	tests := []struct {
		name   string
		values []string
	}{
		{name: "missing"},
		{name: "wrong", values: []string{"999"}},
		{name: "duplicate", values: []string{want, want}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header := http.Header{}
			for _, value := range test.values {
				header.Add(version.APIHeader, value)
			}
			resp := &http.Response{Header: header}
			if err := validateAPIVersion(resp); err == nil {
				t.Fatal("accepted invalid API version response")
			}
		})
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	t.Setenv(secretEnvironment, "duck-secret")
	followed := false
	target := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		followed = true
		w.Header().Set(version.APIHeader, strconv.Itoa(version.API))
	}))
	defer target.Close()

	server := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		w.Header().Set(version.APIHeader, strconv.Itoa(version.API))
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	client := testClient(t, server.URL)
	_, err := client.status()
	var responseErr *serverResponseError
	if !errors.As(err, &responseErr) ||
		responseErr.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("error = %v", err)
	}
	if followed {
		t.Fatal("client followed redirect")
	}
}

func TestEscapeGlobLiteral(t *testing.T) {
	if got, want := escapeGlobLiteral("duck[*?]"), "duck[[][*][?]]"; got != want {
		t.Fatalf("escapeGlobLiteral() = %q, want %q", got, want)
	}
}

func testClient(t *testing.T, serverURL string) *client {
	t.Helper()
	parsed, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	address, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	client, err := newClient(address, port, time.Second.String())
	if err != nil {
		t.Fatal(err)
	}
	return client
}
