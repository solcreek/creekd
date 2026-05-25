package adminclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/solcreek/creekd/internal/apitypes"
)

// DefaultServer is used when Config.Server is empty.
const DefaultServer = "http://127.0.0.1:9080"

// DefaultTimeout caps each request. Logs in follow mode are
// streaming and bypass this limit.
const DefaultTimeout = 30 * time.Second

// Config bundles the runtime knobs for a Client. All fields are
// optional; New() fills in defaults.
type Config struct {
	Server     string
	Token      string
	HTTPClient *http.Client
}

// Client is the typed admin API client. Construct with New. Safe for
// concurrent use across goroutines.
type Client struct {
	server     string
	token      string
	httpClient *http.Client
}

// New returns a Client built from cfg. Missing fields fall back to
// the package defaults.
func New(cfg Config) *Client {
	server := strings.TrimRight(cfg.Server, "/")
	if server == "" {
		server = DefaultServer
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{server: server, token: cfg.Token, httpClient: hc}
}

// Server returns the resolved base URL.
func (c *Client) Server() string { return c.server }

// APIError is returned for non-2xx responses, surfacing the server's
// JSON error payload alongside the HTTP status.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("admin api: %s (%d): %s", e.Code, e.Status, e.Message)
	}
	return fmt.Sprintf("admin api: HTTP %d: %s", e.Status, e.Message)
}

// IsNotFound reports whether err is a 404 from the API.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// IsAlreadyRunning reports whether err indicates the app already exists.
func IsAlreadyRunning(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Code == string(apitypes.ErrorCodeAlreadyRunning)
}

// List fetches all registered apps.
func (c *Client) List(ctx context.Context) ([]apitypes.AppView, error) {
	var resp apitypes.ListAppsResponse
	if err := c.do(ctx, http.MethodGet, "/v1/apps", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Apps, nil
}

// Get fetches one app by ID.
func (c *Client) Get(ctx context.Context, id string) (*apitypes.AppView, error) {
	var v apitypes.AppView
	if err := c.do(ctx, http.MethodGet, "/v1/apps/"+url.PathEscape(id), nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Spawn creates a new app. Returns the AppView from the 201 response.
func (c *Client) Spawn(ctx context.Context, req apitypes.SpawnRequest) (*apitypes.AppView, error) {
	var v apitypes.AppView
	if err := c.do(ctx, http.MethodPost, "/v1/apps", req, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Stop deletes an app.
func (c *Client) Stop(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/apps/"+url.PathEscape(id), nil, nil)
}

// Deploy issues a blue-green deployment for an existing app.
func (c *Client) Deploy(ctx context.Context, id string, req apitypes.DeployRequest) (*apitypes.AppView, error) {
	var v apitypes.AppView
	if err := c.do(ctx, http.MethodPost,
		"/v1/apps/"+url.PathEscape(id)+"/deploy", req, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Restart cycles an app's process in place.
func (c *Client) Restart(ctx context.Context, id string, req apitypes.RestartRequest) (*apitypes.AppView, error) {
	var v apitypes.AppView
	if err := c.do(ctx, http.MethodPost,
		"/v1/apps/"+url.PathEscape(id)+"/restart", req, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Reset clears the crash-loop state of a suspended app.
func (c *Client) Reset(ctx context.Context, id string) (*apitypes.AppView, error) {
	var v apitypes.AppView
	if err := c.do(ctx, http.MethodPost,
		"/v1/apps/"+url.PathEscape(id)+"/reset", struct{}{}, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Stats fetches the per-app resource snapshot.
func (c *Client) Stats(ctx context.Context, id string) (*apitypes.StatsView, error) {
	var v apitypes.StatsView
	if err := c.do(ctx, http.MethodGet,
		"/v1/apps/"+url.PathEscape(id)+"/stats", nil, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// LogsTail fetches the last n lines of an app's log as plain text.
func (c *Client) LogsTail(ctx context.Context, id string, n int) (string, error) {
	q := url.Values{}
	if n > 0 {
		q.Set("tail", strconv.Itoa(n))
	}
	path := "/v1/apps/" + url.PathEscape(id) + "/logs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", parseAPIError(resp.StatusCode, body)
	}
	return string(body), nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("admin api: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("admin api: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseAPIError(resp.StatusCode, respBody)
	}
	if resp.StatusCode == http.StatusNoContent || out == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("admin api: decode response: %w", err)
	}
	return nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("admin api: encode body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.server+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return req, nil
}

// Events opens an SSE connection to /v1/apps/{id}/events.
func (c *Client) Events(ctx context.Context, id string, fn func(line []byte) error) error {
	req, err := c.newRequest(ctx, "GET", "/v1/apps/"+url.PathEscape(id)+"/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("admin api: events: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return parseAPIError(resp.StatusCode, body)
	}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		const prefix = "data: "
		if len(line) > len(prefix) && string(line[:len(prefix)]) == prefix {
			if err := fn(line[len(prefix):]); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("admin api: events stream: %w", err)
	}
	return nil
}

func parseAPIError(status int, body []byte) error {
	var er apitypes.ErrorResponse
	if json.Unmarshal(body, &er) == nil && (er.Code != "" || er.Error != "") {
		return &APIError{Status: status, Code: string(er.Code), Message: er.Error}
	}
	return &APIError{Status: status, Message: strings.TrimSpace(string(body))}
}
