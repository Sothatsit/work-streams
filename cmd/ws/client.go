package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/Sothatsit/work-stream/internal/api"
	"github.com/Sothatsit/work-stream/internal/config"
	"github.com/Sothatsit/work-stream/internal/version"
)

const (
	defaultAddress    = "localhost"
	defaultPort       = "7139"
	secretEnvironment = "WORK_STREAM_SECRET"
)

type requestOperation int

const (
	readOperation requestOperation = iota
	addOperation
	editOperation
	deleteOperation
	addMetaOperation
	editMetaOperation
	removeMetaOperation
)

func (op requestOperation) advice(id int64) string {
	switch op {
	case addOperation:
		return "check 'ws recent' before retrying"
	case editOperation, deleteOperation, addMetaOperation,
		editMetaOperation, removeMetaOperation:
		return fmt.Sprintf("check 'ws entry e%d' before retrying", id)
	default:
		return ""
	}
}

type serverResponseError struct {
	StatusCode int
	Message    string
}

func (e *serverResponseError) Error() string {
	return e.Message
}

type client struct {
	baseURL string
	secret  string
	timeout time.Duration
	http    *http.Client
}

func newClient(address, port, timeoutFlag string) (*client, error) {
	if address == "" {
		address = os.Getenv("WORK_STREAM_ADDRESS")
	}
	if address == "" {
		address = defaultAddress
	}
	if port == "" {
		port = os.Getenv("WORK_STREAM_PORT")
	}
	if port == "" {
		port = defaultPort
	}
	timeout, err := config.Timeout(timeoutFlag)
	if err != nil {
		return nil, err
	}
	secret := os.Getenv(secretEnvironment)
	if err := validateSecret(secret); err != nil {
		return nil, err
	}
	return &client{
		baseURL: "http://" + net.JoinHostPort(address, port),
		secret:  secret,
		timeout: timeout,
		http: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(
				_ *http.Request, _ []*http.Request,
			) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func validateSecret(secret string) error {
	for i := 0; i < len(secret); i++ {
		if secret[i] <= ' ' || secret[i] == 0x7f {
			return fmt.Errorf(
				"%s cannot contain ASCII whitespace or control characters",
				secretEnvironment,
			)
		}
	}
	return nil
}

func (c *client) addAdvice(
	op requestOperation, id int64, err error,
) error {
	advice := op.advice(id)
	if advice == "" {
		return err
	}
	return fmt.Errorf("%w; it may have succeeded\n%s", err, advice)
}

func (c *client) requestError(
	op requestOperation, id int64, err error,
) error {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &netErr) && netErr.Timeout()) {
		failure := fmt.Errorf("request timed out after %s", c.timeout)
		return c.addAdvice(op, id, failure)
	}
	failure := fmt.Errorf("request to %s failed: %w", c.baseURL, err)
	return c.addAdvice(op, id, failure)
}

func validateAPIVersion(resp *http.Response) error {
	expected := strconv.Itoa(version.API)
	values := resp.Header.Values(version.APIHeader)
	if len(values) == 1 && values[0] == expected {
		return nil
	}
	if len(values) == 0 {
		return fmt.Errorf(
			"server omitted %s; ws requires API %s",
			version.APIHeader, expected,
		)
	}
	return fmt.Errorf(
		"server returned %s %q; ws requires API %s",
		version.APIHeader, values, expected,
	)
}

func decodeResponse(body io.Reader, out any) error {
	data, err := readResponse(body)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("response contains more than one JSON value")
		}
		return err
	}
	return nil
}

func readResponse(body io.Reader) ([]byte, error) {
	reader := io.LimitReader(body, int64(api.MaxResponseBytes)+1)
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if len(data) > api.MaxResponseBytes {
		return nil, fmt.Errorf("server response exceeds %d bytes",
			api.MaxResponseBytes)
	}
	return data, nil
}

func (c *client) do(
	op requestOperation, id int64, method, path string, body any, out any,
) error {
	var reqBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reqBody = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, c.baseURL+path, reqBody)
	if err != nil {
		return err
	}
	req.Header.Set(version.APIHeader, strconv.Itoa(version.API))
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.requestError(op, id, err)
	}
	defer resp.Body.Close()

	if err := validateAPIVersion(resp); err != nil {
		return c.addAdvice(op, id, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, err := readResponse(resp.Body)
		if err != nil {
			return c.requestError(op, id, err)
		}
		var errResp api.ErrorResponse
		message := ""
		if json.Unmarshal(data, &errResp) == nil {
			message = errResp.Error
		}
		if resp.StatusCode == http.StatusUnauthorized {
			if c.secret == "" {
				message = "server requires WORK_STREAM_SECRET"
			} else {
				message = "WORK_STREAM_SECRET was rejected"
			}
		}
		if message == "" {
			message = fmt.Sprintf("server returned %s", resp.Status)
			if len(data) > 0 {
				message += ": " + string(data)
			}
		}
		responseErr := &serverResponseError{
			StatusCode: resp.StatusCode,
			Message:    message,
		}
		if resp.StatusCode >= http.StatusInternalServerError {
			return c.addAdvice(op, id, responseErr)
		}
		return responseErr
	}
	if out == nil {
		return nil
	}
	if err := decodeResponse(resp.Body, out); err != nil {
		return c.requestError(
			op, id, fmt.Errorf("reading server response: %w", err),
		)
	}
	return nil
}

func (c *client) addEntry(req api.AddEntryRequest) (api.Entry, error) {
	var entry api.Entry
	err := c.do(addOperation, 0, "POST", "/api/entries", req, &entry)
	return entry, err
}

func (c *client) getEntry(id int64) (api.Entry, error) {
	var entry api.Entry
	path := fmt.Sprintf("/api/entries/%d", id)
	err := c.do(readOperation, id, "GET", path, nil, &entry)
	return entry, err
}

func (c *client) editEntry(
	id int64, req api.EditEntryRequest,
) (api.Entry, error) {
	var entry api.Entry
	path := fmt.Sprintf("/api/entries/%d", id)
	err := c.do(editOperation, id, "PATCH", path, req, &entry)
	return entry, err
}

func (c *client) deleteEntry(id int64) error {
	path := fmt.Sprintf("/api/entries/%d", id)
	return c.do(deleteOperation, id, "DELETE", path, nil, nil)
}

func (c *client) search(params url.Values) (api.SearchResult, error) {
	var result api.SearchResult
	path := "/api/entries?" + params.Encode()
	err := c.do(readOperation, 0, "GET", path, nil, &result)
	return result, err
}

func (c *client) addMeta(
	id int64, key, value string,
) (api.Entry, error) {
	var entry api.Entry
	req := api.MetaRequest{Key: key, Value: value}
	path := fmt.Sprintf("/api/entries/%d/metadata", id)
	err := c.do(addMetaOperation, id, "POST", path, req, &entry)
	return entry, err
}

func (c *client) editMeta(
	id int64, key, value string,
) (api.Entry, error) {
	var entry api.Entry
	req := api.MetaRequest{Value: value}
	path := fmt.Sprintf(
		"/api/entries/%d/metadata/%s", id, url.PathEscape(key),
	)
	err := c.do(editMetaOperation, id, "PUT", path, req, &entry)
	return entry, err
}

func (c *client) removeMeta(id int64, key string) (api.Entry, error) {
	var entry api.Entry
	path := fmt.Sprintf(
		"/api/entries/%d/metadata/%s", id, url.PathEscape(key),
	)
	err := c.do(removeMetaOperation, id, "DELETE", path, nil, &entry)
	return entry, err
}

func (c *client) status() (api.StatusResponse, error) {
	var status api.StatusResponse
	err := c.do(readOperation, 0, "GET", "/api/status", nil, &status)
	return status, err
}
