// Package opencode provides the typed subset of the OpenCode HTTP API used by
// Gistbot and model listing from the OpenCode CLI.
//
// Every request authenticates with Basic auth username "opencode" and the
// password supplied to NewClient. Errors name the operation, the HTTP status,
// and the response body, but never the password or the Authorization header.
//
// A Client is safe for concurrent use after construction.
package opencode

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// defaultBaseURL is the always-on API server address used by production
	// when no OPENCODE_API_URL override is configured.
	defaultBaseURL = "http://127.0.0.1:4096"
	// authUsername is the fixed Basic auth username required by the dedicated
	// OpenCode API server.
	authUsername = "opencode"
	// requestTimeout bounds every call except Wait, which is owned by the
	// caller's job context and may legitimately run for many minutes.
	requestTimeout = 30 * time.Second
	// maxResponseBytes caps how much of a response body is read.
	maxResponseBytes = 10 << 20
	// maxErrorBytes caps how much response body is copied into an error.
	maxErrorBytes = 4 << 10

	redacted = "<redacted>"
)

// Message is one projected session message. Assistant messages carry text in
// Content; idle messages carry the terminal outcome.
type Message struct {
	Type    string           `json:"type"`
	Outcome string           `json:"outcome,omitempty"`
	Content []MessageContent `json:"content,omitempty"`
}

// MessageContent is one content part of a message.
type MessageContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Client is a minimal OpenCode API client.
type Client struct {
	baseURL   string
	password  string
	authToken string
	http      *http.Client
}

// NewClient returns a client for baseURL. An empty baseURL falls back to the
// production address, and trailing slashes are removed so paths never double
// up. The password is used only for Basic auth and is redacted from every
// error.
func NewClient(baseURL, password string) *Client {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = defaultBaseURL
	}
	return &Client{
		baseURL:   base,
		password:  password,
		authToken: base64.StdEncoding.EncodeToString([]byte(authUsername + ":" + password)),
		http:      &http.Client{Timeout: requestTimeout},
	}
}

// Info reports whether the API server is reachable and authenticated. It is
// the health check used before a job starts.
func (c *Client) Info(ctx context.Context) error {
	_, err := c.do(ctx, c.http, "info", http.MethodGet, "/api/info", nil)
	return err
}

// CreateSession starts a session in location with the given agent and model.
// The variant is sent only when non-empty. It returns the server's session
// identifier.
func (c *Client) CreateSession(ctx context.Context, location, agent, providerID, modelID, variant string) (string, error) {
	payload := createSessionRequest{
		Agent: agent,
		Model: sessionModel{
			ProviderID: providerID,
			ID:         modelID,
			Variant:    variant,
		},
		Location: sessionLocation{Directory: location},
	}

	raw, err := c.do(ctx, c.http, "create session", http.MethodPost, "/api/session", payload)
	if err != nil {
		return "", err
	}

	var response sessionEnvelope
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", c.wrap("create session", fmt.Errorf("decode response: %w", err))
	}
	if response.Data.ID == "" {
		return "", c.wrap("create session", errors.New("response is missing the session id"))
	}
	return response.Data.ID, nil
}

// Prompt sends text to a session.
func (c *Client) Prompt(ctx context.Context, sessionID, text string) error {
	payload := promptRequest{Text: text}
	_, err := c.do(ctx, c.http, "prompt", http.MethodPost, sessionPath(sessionID, "/prompt"), payload)
	return err
}

// Wait blocks until the session's agent loop becomes idle. It deliberately
// uses the caller's context without the shared client timeout: a job may run
// for as long as its own deadline allows.
func (c *Client) Wait(ctx context.Context, sessionID string) error {
	client := *c.http
	client.Timeout = 0

	_, err := c.do(ctx, &client, "wait", http.MethodPost, experimentalSessionPath(sessionID, "/wait"), nil)
	return err
}

// Interrupt stops the session's active execution without resuming pending
// input. It reports whether an execution was actually interrupted; false means
// the session was already idle.
func (c *Client) Interrupt(ctx context.Context, sessionID string) (bool, error) {
	path := sessionPath(sessionID, "/interrupt") + "?resume=false"

	raw, err := c.do(ctx, c.http, "interrupt", http.MethodPost, path, nil)
	if err != nil {
		return false, err
	}

	var response interruptResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return false, c.wrap("interrupt", fmt.Errorf("decode response: %w", err))
	}
	return response.Interrupted, nil
}

// Messages returns the newest 50 session messages, newest first.
func (c *Client) Messages(ctx context.Context, sessionID string) ([]Message, error) {
	path := sessionPath(sessionID, "/message") + "?order=desc&limit=50"

	raw, err := c.do(ctx, c.http, "messages", http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}

	var response messagesEnvelope
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, c.wrap("messages", fmt.Errorf("decode response: %w", err))
	}
	return response.Data, nil
}

// SessionLog returns the experimental session event log. The API answers with
// an envelope containing either a string or an array of event objects; an
// array is rendered as one compact JSON object per line. A non-JSON body is
// returned unchanged.
func (c *Client) SessionLog(ctx context.Context, sessionID string) (string, error) {
	raw, err := c.do(ctx, c.http, "session log", http.MethodGet, experimentalSessionPath(sessionID, "/log"), nil)
	if err != nil {
		return "", err
	}

	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope.Data) == 0 {
		return string(raw), nil
	}

	var text string
	if err := json.Unmarshal(envelope.Data, &text); err == nil {
		return text, nil
	}

	var events []json.RawMessage
	if err := json.Unmarshal(envelope.Data, &events); err != nil {
		return string(raw), nil
	}

	lines := make([]string, 0, len(events))
	for _, event := range events {
		lines = append(lines, string(event))
	}
	return strings.Join(lines, "\n"), nil
}

type createSessionRequest struct {
	Agent    string          `json:"agent"`
	Model    sessionModel    `json:"model"`
	Location sessionLocation `json:"location"`
}

type sessionModel struct {
	ProviderID string `json:"providerID"`
	ID         string `json:"id"`
	Variant    string `json:"variant,omitempty"`
}

type sessionLocation struct {
	Directory string `json:"directory"`
}

type sessionEnvelope struct {
	Data struct {
		ID string `json:"id"`
	} `json:"data"`
}

type promptRequest struct {
	Text string `json:"text"`
}

type interruptResponse struct {
	Interrupted bool `json:"interrupted"`
}

type messagesEnvelope struct {
	Data []Message `json:"data"`
}

func sessionPath(sessionID, suffix string) string {
	return "/api/session/" + url.PathEscape(sessionID) + suffix
}

func experimentalSessionPath(sessionID, suffix string) string {
	return "/api/experimental/session/" + url.PathEscape(sessionID) + suffix
}

// do performs one API call and returns the raw success body. A nil body sends
// no payload. The password only ever travels in the Authorization header.
func (c *Client) do(ctx context.Context, client *http.Client, operation, method, path string, body any) ([]byte, error) {
	if ctx == nil {
		return nil, c.wrap(operation, errors.New("context is nil"))
	}

	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, c.wrap(operation, fmt.Errorf("encode request: %w", err))
		}
		reader = bytes.NewReader(payload)
	}

	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, c.wrap(operation, fmt.Errorf("build request: %w", err))
	}
	request.SetBasicAuth(authUsername, c.password)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := client.Do(request)
	if err != nil {
		return nil, c.wrap(operation, err)
	}
	defer func() {
		_ = response.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return nil, c.wrap(operation, fmt.Errorf("read response: %w", err))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, c.statusError(operation, response.StatusCode, raw)
	}
	return raw, nil
}

// statusError reports an HTTP failure with its status and a sanitized,
// bounded copy of the response body.
func (c *Client) statusError(operation string, status int, raw []byte) error {
	detail := c.sanitize(strings.TrimSpace(string(raw)))
	if detail == "" {
		detail = http.StatusText(status)
		if detail == "" {
			detail = "request failed"
		}
	}
	return &clientError{
		operation: operation,
		message:   fmt.Sprintf("status %d: %s", status, truncate(detail, maxErrorBytes)),
	}
}

// clientError is the error type returned by every Client method. Its message
// is sanitized, and Unwrap exposes only allow-listed causes so callers can
// classify cancellation without recovering a credential-bearing error.
type clientError struct {
	operation string
	message   string
	cause     error
}

func (e *clientError) Error() string {
	return "opencode " + e.operation + ": " + e.message
}

func (e *clientError) Unwrap() error {
	return e.cause
}

func (c *Client) wrap(operation string, err error) error {
	return &clientError{
		operation: operation,
		message:   c.sanitize(err.Error()),
		cause:     safeCause(err),
	}
}

// safeCause returns only the underlying causes that may cross the error
// boundary. The original transport error can carry a URL and is never
// returned.
func safeCause(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	}

	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return context.DeadlineExceeded
	}
	return nil
}

// sanitize removes the password and its Basic auth encoding from a message
// before it can be returned or logged.
func (c *Client) sanitize(message string) string {
	if c.authToken != "" {
		message = strings.ReplaceAll(message, c.authToken, redacted)
	}
	if c.password != "" {
		message = strings.ReplaceAll(message, c.password, redacted)
		if escaped := url.QueryEscape(c.password); escaped != "" && escaped != c.password {
			message = strings.ReplaceAll(message, escaped, redacted)
		}
	}
	return message
}

func truncate(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	return strings.ToValidUTF8(text[:limit], "") + "…"
}
