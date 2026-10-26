// Package telegram provides the subset of the Telegram Bot API used by Zero
// Two (gistbot).
//
// The client is owner-agnostic: callers decide which updates to act on. It
// never logs or returns a raw request URL, and every error is shaped as
// "telegram <method>: <sanitized error>" with the bot token replaced by
// "<redacted>".
//
// A Client is safe for concurrent use after construction.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://api.telegram.org"
	redacted       = "<redacted>"
	redactedURL    = "<base-url>"

	methodDeleteWebhook   = "deleteWebhook"
	methodGetUpdates      = "getUpdates"
	methodSendMessage     = "sendMessage"
	methodSendDocument    = "sendDocument"
	methodEditMessageText = "editMessageText"
	methodSendMediaGroup  = "sendMediaGroup"
	methodSetMyCommands   = "setMyCommands"
	formContentType       = "application/x-www-form-urlencoded"
	defaultPollTimeout    = 30 * time.Second
	longPollGrace         = 10 * time.Second
	sendTimeout           = 15 * time.Second
	maxRetries            = 3
	maxResponseBytes      = 10 << 20
	maxMessageUTF16       = 4096
	truncationEllipsis    = "…"
	minMediaGroupDocs     = 2
	maxMediaGroupDocs     = 10
	fieldAllowedUpdates   = "allowed_updates"
	onlyMessageUpdates    = `["message"]`
	fieldParseMode        = "parse_mode"
	parseModeHTML         = "HTML"
	fieldDocument         = "document"
	fieldMedia            = "media"
	fieldCommands         = "commands"
)

// ErrMessageTooLong reports that an HTML message exceeds Telegram's 4,096
// UTF-16 code unit limit. Callers should trim the text with EscapeAndTruncate
// rather than sending it.
var ErrMessageTooLong = errors.New("message exceeds 4096 UTF-16 code units")

// Client is a minimal Telegram Bot API client.
type Client struct {
	token      string
	baseURL    string
	httpClient *http.Client
	customHTTP bool
	sleep      func(context.Context, time.Duration) error
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL overrides the Telegram API base URL. Tests point this at an
// httptest server.
func WithBaseURL(baseURL string) Option {
	return func(c *Client) {
		trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
		if trimmed != "" {
			c.baseURL = trimmed
		}
	}
}

// WithHTTPClient overrides the HTTP client. Tests use the httptest server's
// client; production uses the default send timeout.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *Client) {
		if httpClient == nil {
			return
		}
		c.httpClient = httpClient
		c.customHTTP = true
	}
}

// WithSleep overrides how the client waits before retrying a 429 response.
func WithSleep(sleep func(context.Context, time.Duration) error) Option {
	return func(c *Client) {
		if sleep == nil {
			return
		}
		c.sleep = sleep
	}
}

// New returns a client for token. The zero configuration targets the public
// Telegram Bot API and uses the process HTTP client with a send timeout.
func New(token string, opts ...Option) *Client {
	client := &Client{
		token:      token,
		baseURL:    defaultBaseURL,
		httpClient: &http.Client{Timeout: sendTimeout},
		sleep:      sleepContext,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(client)
		}
	}
	return client
}

// DeleteWebhook removes any previously configured webhook so that long
// polling can receive updates.
func (c *Client) DeleteWebhook(ctx context.Context) error {
	form := url.Values{}
	_, err := c.do(ctx, methodDeleteWebhook, formContentType, formBody(form), 0)
	return err
}

// GetUpdates long-polls for message updates starting at offset. Only message
// updates are requested; edited messages and other update kinds are filtered
// out by Telegram. A non-positive timeout means the default 30 seconds.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	if timeout <= 0 {
		timeout = defaultPollTimeout
	}

	seconds := int(timeout / time.Second)
	if seconds <= 0 {
		seconds = 1
	}

	form := url.Values{
		"offset":            {strconv.FormatInt(offset, 10)},
		"timeout":           {strconv.Itoa(seconds)},
		fieldAllowedUpdates: {onlyMessageUpdates},
	}

	raw, err := c.do(ctx, methodGetUpdates, formContentType, formBody(form), timeout+longPollGrace)
	if err != nil {
		return nil, err
	}

	var updates []Update
	if err := json.Unmarshal(raw, &updates); err != nil {
		return nil, c.wrap(methodGetUpdates, fmt.Errorf("decode result: %w", err))
	}
	return updates, nil
}

// SendMessage sends an HTML-formatted message and discards its message ID. The
// text is not escaped or truncated here: callers must pass output from
// EscapeAndTruncate and keep it within 4,096 UTF-16 code units.
func (c *Client) SendMessage(ctx context.Context, chatID int64, html string) error {
	_, err := c.SendMessageID(ctx, chatID, html)
	return err
}

// SendMessageID sends an HTML-formatted message and returns its Telegram
// message ID so callers can edit it in place later. The text is not escaped or
// truncated here: callers must pass output from EscapeAndTruncate and keep it
// within 4,096 UTF-16 code units.
func (c *Client) SendMessageID(ctx context.Context, chatID int64, html string) (int64, error) {
	if utf16Len(html) > maxMessageUTF16 {
		return 0, c.wrap(methodSendMessage, ErrMessageTooLong)
	}

	form := url.Values{
		"chat_id":      {strconv.FormatInt(chatID, 10)},
		"text":         {html},
		fieldParseMode: {parseModeHTML},
	}

	raw, err := c.do(ctx, methodSendMessage, formContentType, formBody(form), 0)
	if err != nil {
		return 0, err
	}

	var result struct {
		MessageID int64 `json:"message_id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return 0, c.wrap(methodSendMessage, fmt.Errorf("decode result: %w", err))
	}
	if result.MessageID <= 0 {
		return 0, c.wrap(methodSendMessage, errors.New("decode result: missing message_id"))
	}
	return result.MessageID, nil
}

// EditMessageText replaces the text of a message the bot sent. The text is not
// escaped or truncated here: callers must pass output from EscapeAndTruncate
// and keep it within 4,096 UTF-16 code units.
func (c *Client) EditMessageText(ctx context.Context, chatID, messageID int64, html string) error {
	if utf16Len(html) > maxMessageUTF16 {
		return c.wrap(methodEditMessageText, ErrMessageTooLong)
	}

	form := url.Values{
		"chat_id":      {strconv.FormatInt(chatID, 10)},
		"message_id":   {strconv.FormatInt(messageID, 10)},
		"text":         {html},
		fieldParseMode: {parseModeHTML},
	}

	_, err := c.do(ctx, methodEditMessageText, formContentType, formBody(form), 0)
	return err
}

// SendDocument uploads data as a document using multipart/form-data.
func (c *Client) SendDocument(ctx context.Context, chatID int64, filename string, data []byte) error {
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)

	if err := writer.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return c.wrap(methodSendDocument, fmt.Errorf("build multipart body: %w", err))
	}
	part, err := writer.CreateFormFile(fieldDocument, filename)
	if err != nil {
		_ = writer.Close()
		return c.wrap(methodSendDocument, fmt.Errorf("build multipart document: %w", err))
	}
	if _, err := part.Write(data); err != nil {
		_ = writer.Close()
		return c.wrap(methodSendDocument, fmt.Errorf("write multipart document: %w", err))
	}
	if err := writer.Close(); err != nil {
		return c.wrap(methodSendDocument, fmt.Errorf("close multipart body: %w", err))
	}

	body := buffer.Bytes()
	contentType := writer.FormDataContentType()
	_, err = c.do(ctx, methodSendDocument, contentType, func() io.Reader {
		return bytes.NewReader(body)
	}, 0)
	return err
}

// SendMediaGroup uploads two to ten documents as one album. Captions are not
// escaped here: callers must pass output from EscapeAndTruncate. The same
// HTML parse mode applies to every caption in the group.
func (c *Client) SendMediaGroup(ctx context.Context, chatID int64, docs []Document) error {
	if len(docs) < minMediaGroupDocs || len(docs) > maxMediaGroupDocs {
		return c.wrap(methodSendMediaGroup, fmt.Errorf("requires 2 to 10 documents, got %d", len(docs)))
	}

	items := make([]mediaGroupItem, len(docs))
	for i, doc := range docs {
		items[i] = mediaGroupItem{
			Type:    "document",
			Media:   "attach://file" + strconv.Itoa(i),
			Caption: doc.Caption,
		}
	}
	media, err := json.Marshal(items)
	if err != nil {
		return c.wrap(methodSendMediaGroup, fmt.Errorf("encode media: %w", err))
	}

	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)

	if err := writer.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return c.wrap(methodSendMediaGroup, fmt.Errorf("build multipart body: %w", err))
	}
	if err := writer.WriteField(fieldParseMode, parseModeHTML); err != nil {
		return c.wrap(methodSendMediaGroup, fmt.Errorf("build multipart parse mode: %w", err))
	}
	if err := writer.WriteField(fieldMedia, string(media)); err != nil {
		return c.wrap(methodSendMediaGroup, fmt.Errorf("build multipart media: %w", err))
	}
	for i, doc := range docs {
		part, err := writer.CreateFormFile("file"+strconv.Itoa(i), doc.Filename)
		if err != nil {
			_ = writer.Close()
			return c.wrap(methodSendMediaGroup, fmt.Errorf("build multipart file %d: %w", i, err))
		}
		if _, err := part.Write(doc.Data); err != nil {
			_ = writer.Close()
			return c.wrap(methodSendMediaGroup, fmt.Errorf("write multipart file %d: %w", i, err))
		}
	}
	if err := writer.Close(); err != nil {
		return c.wrap(methodSendMediaGroup, fmt.Errorf("close multipart body: %w", err))
	}

	// Build the multipart body once so a 429 retry replays the exact bytes.
	body := buffer.Bytes()
	contentType := writer.FormDataContentType()
	_, err = c.do(ctx, methodSendMediaGroup, contentType, func() io.Reader {
		return bytes.NewReader(body)
	}, 0)
	return err
}

// SetMyCommands replaces the bot's command menu. An empty list clears it.
func (c *Client) SetMyCommands(ctx context.Context, commands []BotCommand) error {
	if commands == nil {
		commands = []BotCommand{}
	}

	encoded, err := json.Marshal(commands)
	if err != nil {
		return c.wrap(methodSetMyCommands, fmt.Errorf("encode commands: %w", err))
	}

	form := url.Values{fieldCommands: {string(encoded)}}
	_, err = c.do(ctx, methodSetMyCommands, formContentType, formBody(form), 0)
	return err
}

// EscapeAndTruncate HTML-escapes s and truncates it to at most maxUTF16 UTF-16
// code units. Escaping is applied rune by rune, so an output never ends inside
// an entity such as &amp;, &lt;, &gt;, or &#39;. A single ellipsis is appended
// when truncation occurs and it fits in the budget.
func EscapeAndTruncate(s string, maxUTF16 int) string {
	if maxUTF16 <= 0 {
		return ""
	}

	total := 0
	for _, r := range s {
		total += escapedUnits(r)
	}
	if total <= maxUTF16 {
		return escapeHTML(s)
	}

	// Reserve one unit for the ellipsis so truncation is always visible.
	limit := maxUTF16 - 1
	var output strings.Builder
	output.Grow(maxUTF16)
	used := 0
	for _, r := range s {
		units := escapedUnits(r)
		if used+units > limit {
			break
		}
		writeEscapedRune(&output, r)
		used += units
	}
	output.WriteString(truncationEllipsis)
	return output.String()
}

// apiEnvelope is the common Telegram response wrapper.
type apiEnvelope struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  *apiParameters  `json:"parameters"`
}

type apiParameters struct {
	RetryAfter int `json:"retry_after"`
}

// mediaGroupItem is one entry in the sendMediaGroup media JSON array.
type mediaGroupItem struct {
	Type    string `json:"type"`
	Media   string `json:"media"`
	Caption string `json:"caption,omitempty"`
}

// formBody rebuilds an encoded form for every attempt so 429 retries can
// resend the same request.
func formBody(form url.Values) func() io.Reader {
	return func() io.Reader {
		return strings.NewReader(form.Encode())
	}
}

// do performs one Bot API method, retrying up to maxRetries times when
// Telegram answers 429 and the injected sleep succeeds. requestTimeout bounds
// each HTTP attempt, not the retry sleeps.
func (c *Client) do(ctx context.Context, method, contentType string, newBody func() io.Reader, requestTimeout time.Duration) (json.RawMessage, error) {
	httpClient := c.httpClient
	if !c.customHTTP && requestTimeout > 0 {
		clone := *httpClient
		clone.Timeout = requestTimeout
		httpClient = &clone
	}

	endpoint := c.baseURL + "/bot" + c.token + "/" + method

	for attempt := 0; ; attempt++ {
		response, payload, err := c.attempt(ctx, httpClient, endpoint, contentType, newBody, requestTimeout)
		if err != nil {
			return nil, c.wrap(method, err)
		}

		var envelope apiEnvelope
		decodeErr := json.Unmarshal(payload, &envelope)

		if response.StatusCode == http.StatusTooManyRequests {
			if attempt >= maxRetries {
				return nil, c.wrap(method, envelopeFailure(response, envelope, decodeErr))
			}
			if err := c.sleep(ctx, retryAfter(response, envelope.Parameters)); err != nil {
				return nil, c.wrap(method, err)
			}
			continue
		}

		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, c.wrap(method, envelopeFailure(response, envelope, decodeErr))
		}
		if decodeErr != nil {
			return nil, c.wrap(method, fmt.Errorf("decode response: %w", decodeErr))
		}
		if !envelope.OK {
			return nil, c.wrap(method, envelopeFailure(response, envelope, nil))
		}
		return envelope.Result, nil
	}
}

// attempt performs a single HTTP request with its own timeout and returns the
// fully read response body.
func (c *Client) attempt(ctx context.Context, httpClient *http.Client, endpoint, contentType string, newBody func() io.Reader, requestTimeout time.Duration) (*http.Response, []byte, error) {
	if requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, requestTimeout)
		defer cancel()
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, newBody())
	if err != nil {
		return nil, nil, fmt.Errorf("build request: %w", err)
	}
	request.Header.Set("Content-Type", contentType)

	response, err := httpClient.Do(request)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		_ = response.Body.Close()
	}()

	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("read response: %w", err)
	}
	return response, payload, nil
}

// envelopeFailure turns an unsuccessful response into an error that names the
// status and the API description, never the request URL.
func envelopeFailure(response *http.Response, envelope apiEnvelope, decodeErr error) error {
	code := envelope.ErrorCode
	if code == 0 {
		code = response.StatusCode
	}
	description := strings.TrimSpace(envelope.Description)
	if description == "" {
		if decodeErr != nil {
			return fmt.Errorf("status %d: invalid response body", code)
		}
		description = http.StatusText(response.StatusCode)
		if description == "" {
			description = "request failed"
		}
	}
	return fmt.Errorf("status %d: %s", code, description)
}

// retryAfter extracts the wait before retrying a 429 response, preferring the
// API's parameters.retry_after over the Retry-After header.
func retryAfter(response *http.Response, parameters *apiParameters) time.Duration {
	if parameters != nil && parameters.RetryAfter > 0 {
		return time.Duration(parameters.RetryAfter) * time.Second
	}
	if raw := strings.TrimSpace(response.Header.Get("Retry-After")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		if when, err := http.ParseTime(raw); err == nil {
			if wait := time.Until(when); wait > 0 {
				return wait
			}
		}
	}
	return time.Second
}

// methodError is the error type returned by every Client method. Its message
// is fully sanitized, and Unwrap exposes only allow-listed sentinel causes so
// callers can classify cancellation and message-length errors without ever
// recovering a URL-bearing transport error.
type methodError struct {
	method  string
	message string
	cause   error
}

func (e *methodError) Error() string {
	return "telegram " + e.method + ": " + e.message
}

func (e *methodError) Unwrap() error {
	return e.cause
}

// wrap formats an error as "telegram <method>: <sanitized error>".
func (c *Client) wrap(method string, err error) error {
	return &methodError{
		method:  method,
		message: c.sanitize(err),
		cause:   safeCause(err),
	}
}

// safeCause returns the only underlying causes that may cross the error
// boundary. The original *url.Error is deliberately never returned: it carries
// the token-bearing request URL.
func safeCause(err error) error {
	switch {
	case errors.Is(err, ErrMessageTooLong):
		return ErrMessageTooLong
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

// sanitize removes URLs and the bot token from an error before it can be
// returned, logged, or embedded in another error.
func (c *Client) sanitize(err error) string {
	if err == nil {
		return ""
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	return c.redact(err.Error())
}

func (c *Client) redact(message string) string {
	if c.baseURL != "" {
		message = strings.ReplaceAll(message, c.baseURL, redactedURL)
	}
	if c.token != "" {
		message = strings.ReplaceAll(message, c.token, redacted)
		if escaped := url.QueryEscape(c.token); escaped != "" && escaped != c.token {
			message = strings.ReplaceAll(message, escaped, redacted)
		}
	}
	return message
}

// sleepContext waits for d or until ctx is cancelled.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// escapeHTML escapes every rune of s for Telegram's HTML parse mode.
func escapeHTML(s string) string {
	var output strings.Builder
	output.Grow(len(s))
	for _, r := range s {
		writeEscapedRune(&output, r)
	}
	return output.String()
}

// writeEscapedRune escapes the four text characters Telegram's HTML parser
// treats as markup. Quotes are valid in text content and are left alone.
func writeEscapedRune(output *strings.Builder, r rune) {
	switch r {
	case '&':
		output.WriteString("&amp;")
	case '<':
		output.WriteString("&lt;")
	case '>':
		output.WriteString("&gt;")
	case '\'':
		output.WriteString("&#39;")
	default:
		output.WriteRune(r)
	}
}

// escapedUnits reports how many UTF-16 code units r occupies after escaping.
func escapedUnits(r rune) int {
	switch r {
	case '&', '\'':
		return 5 // &amp; and &#39;
	case '<', '>':
		return 4 // &lt; and &gt;
	default:
		if r > 0xFFFF {
			return 2
		}
		return 1
	}
}

// utf16Len counts UTF-16 code units, matching Telegram's message limit.
func utf16Len(s string) int {
	units := 0
	for _, r := range s {
		if r > 0xFFFF {
			units += 2
			continue
		}
		units++
	}
	return units
}
