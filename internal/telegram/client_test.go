package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
)

// testToken is deliberately fake. Tests never print it on failure so that a
// real token could never leak through copied test output.
const testToken = "123456:test-token-not-for-logs"

func newTestClient(t *testing.T, handler http.Handler, opts ...Option) *Client {
	t.Helper()
	client, _ := newTestServerClient(t, handler, opts...)
	return client
}

// newTestServerClient also returns the server URL so sanitization tests can
// prove it never appears in an error.
func newTestServerClient(t *testing.T, handler http.Handler, opts ...Option) (*Client, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(func() {
		// Force-close connections first so a handler that is waiting on a
		// cancelled request context cannot block Server.Close.
		server.CloseClientConnections()
		server.Close()
	})
	all := append([]Option{WithBaseURL(server.URL), WithHTTPClient(server.Client())}, opts...)
	return New(testToken, all...), server.URL
}

// assertSanitizedError pins the redaction contract and rejects an unwrap chain
// that would let a caller recover the token-bearing request URL.
func assertSanitizedError(t *testing.T, err error, method, baseURL string) {
	t.Helper()
	if err == nil {
		t.Fatal("error is nil")
	}
	message := err.Error()
	if strings.Contains(message, testToken) {
		t.Error("error message contains the bot token")
	}
	if strings.Contains(message, baseURL) {
		t.Error("error message contains the raw base URL")
	}
	if !strings.HasPrefix(message, "telegram "+method+": ") {
		t.Errorf("error message %q lacks the method prefix", message)
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		t.Error("error chain exposes *url.Error")
	}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func respond(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func TestGetUpdatesSendsOffsetAndTimeout(t *testing.T) {
	var (
		gotPath   string
		gotMethod string
		gotForm   url.Values
	)

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse getUpdates form: %v", err)
		}
		gotForm = r.PostForm
		respond(w, http.StatusOK, `{"ok":true,"result":[{"update_id":7,"message":{"message_id":11,"from":{"id":4242,"is_bot":false,"first_name":"Owner"},"chat":{"id":4242,"type":"private"},"date":1700000000,"text":"hello"}}]}`)
	}))

	updates, err := client.GetUpdates(testContext(t), 42, 30*time.Second)
	if err != nil {
		t.Fatalf("GetUpdates returned an error: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want %q", gotMethod, http.MethodPost)
	}
	if want := "/bot" + testToken + "/getUpdates"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if got := gotForm.Get("offset"); got != "42" {
		t.Errorf("offset = %q, want %q", got, "42")
	}
	if got := gotForm.Get("timeout"); got != "30" {
		t.Errorf("timeout = %q, want %q", got, "30")
	}
	if got := gotForm.Get("allowed_updates"); got != `["message"]` {
		t.Errorf("allowed_updates = %q, want %q", got, `["message"]`)
	}

	if len(updates) != 1 {
		t.Fatalf("len(updates) = %d, want 1", len(updates))
	}
	update := updates[0]
	if update.UpdateID != 7 {
		t.Errorf("UpdateID = %d, want 7", update.UpdateID)
	}
	if update.Message == nil {
		t.Fatal("Message is nil")
	}
	if update.Message.MessageID != 11 {
		t.Errorf("MessageID = %d, want 11", update.Message.MessageID)
	}
	if update.Message.Date != 1700000000 {
		t.Errorf("Date = %d, want 1700000000", update.Message.Date)
	}
	if update.Message.Text != "hello" {
		t.Errorf("Text = %q, want %q", update.Message.Text, "hello")
	}
	if update.Message.Chat.ID != 4242 || update.Message.Chat.Type != "private" {
		t.Errorf("Chat = %+v, want id 4242 type private", update.Message.Chat)
	}
	if update.Message.From == nil {
		t.Fatal("From is nil")
	}
	if update.Message.From.ID != 4242 || update.Message.From.IsBot || update.Message.From.FirstName != "Owner" {
		t.Errorf("From = %+v, want owner user", update.Message.From)
	}
}

func TestSendMessageUsesHTMLAndReturnsAPIError(t *testing.T) {
	var (
		mu      sync.Mutex
		calls   int
		gotPath string
		gotForm url.Values
	)

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()

		if call == 1 {
			gotPath = r.URL.Path
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse sendMessage form: %v", err)
			}
			gotForm = r.PostForm
			respond(w, http.StatusOK, `{"ok":true,"result":{"message_id":5}}`)
			return
		}
		respond(w, http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`)
	}))

	if err := client.SendMessage(testContext(t), 99, "hello <b>world</b>"); err != nil {
		t.Fatalf("SendMessage returned an error: %v", err)
	}
	if want := "/bot" + testToken + "/sendMessage"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if got := gotForm.Get("chat_id"); got != "99" {
		t.Errorf("chat_id = %q, want %q", got, "99")
	}
	if got := gotForm.Get("text"); got != "hello <b>world</b>" {
		t.Errorf("text = %q, want the HTML body unchanged", got)
	}
	if got := gotForm.Get("parse_mode"); got != "HTML" {
		t.Errorf("parse_mode = %q, want %q", got, "HTML")
	}

	err := client.SendMessage(testContext(t), 99, "hello")
	if err == nil {
		t.Fatal("SendMessage returned nil for an API error")
	}
	message := err.Error()
	for _, want := range []string{"telegram sendMessage:", "400", "chat not found"} {
		if !strings.Contains(message, want) {
			t.Errorf("error %q does not contain %q", message, want)
		}
	}
}

func TestSendDocumentIsMultipart(t *testing.T) {
	var (
		gotPath        string
		gotContentType string
		gotChatID      string
		gotFilename    string
		gotData        []byte
	)

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart form: %v", err)
			respond(w, http.StatusInternalServerError, `{"ok":false,"error_code":500,"description":"bad multipart"}`)
			return
		}
		gotChatID = r.FormValue("chat_id")
		files := r.MultipartForm.File["document"]
		if len(files) != 1 {
			t.Errorf("document parts = %d, want 1", len(files))
			respond(w, http.StatusOK, `{"ok":true,"result":{"message_id":6}}`)
			return
		}
		gotFilename = files[0].Filename
		file, err := files[0].Open()
		if err != nil {
			t.Errorf("open document part: %v", err)
			respond(w, http.StatusInternalServerError, `{"ok":false,"error_code":500,"description":"open failed"}`)
			return
		}
		defer file.Close()
		gotData, err = io.ReadAll(file)
		if err != nil {
			t.Errorf("read document part: %v", err)
		}
		respond(w, http.StatusOK, `{"ok":true,"result":{"message_id":6}}`)
	}))

	data := []byte{0x00, 0x01, 0x02, 'g', 'i', 's', 't', 0xff}
	if err := client.SendDocument(testContext(t), 55, "codemap.md", data); err != nil {
		t.Fatalf("SendDocument returned an error: %v", err)
	}

	if want := "/bot" + testToken + "/sendDocument"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	mediaType, params, err := mime.ParseMediaType(gotContentType)
	if err != nil {
		t.Fatalf("parse content type %q: %v", gotContentType, err)
	}
	if mediaType != "multipart/form-data" {
		t.Errorf("media type = %q, want multipart/form-data", mediaType)
	}
	if params["boundary"] == "" {
		t.Error("multipart boundary is empty")
	}
	if gotChatID != "55" {
		t.Errorf("chat_id = %q, want %q", gotChatID, "55")
	}
	if gotFilename != "codemap.md" {
		t.Errorf("filename = %q, want %q", gotFilename, "codemap.md")
	}
	if !bytes.Equal(gotData, data) {
		t.Errorf("document bytes = %v, want %v", gotData, data)
	}
}

func Test429HonorsRetryAfter(t *testing.T) {
	var (
		mu       sync.Mutex
		requests int
		slept    []time.Duration
	)

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		call := requests
		mu.Unlock()

		if call == 1 {
			respond(w, http.StatusTooManyRequests, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 2","parameters":{"retry_after":2}}`)
			return
		}
		respond(w, http.StatusOK, `{"ok":true,"result":[]}`)
	}), WithSleep(func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		if ctx == nil {
			t.Error("sleep received a nil context")
		}
		slept = append(slept, d)
		return nil
	}))

	updates, err := client.GetUpdates(testContext(t), 0, 30*time.Second)
	if err != nil {
		t.Fatalf("GetUpdates returned an error after retry: %v", err)
	}
	if len(updates) != 0 {
		t.Errorf("len(updates) = %d, want 0", len(updates))
	}

	mu.Lock()
	defer mu.Unlock()
	if requests != 2 {
		t.Errorf("requests = %d, want 2", requests)
	}
	if len(slept) != 1 {
		t.Fatalf("sleep calls = %d, want 1", len(slept))
	}
	if slept[0] != 2*time.Second {
		t.Errorf("slept for %v, want 2s", slept[0])
	}
}

func TestErrorsNeverContainToken(t *testing.T) {
	t.Run("api description is redacted", func(t *testing.T) {
		client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			respond(w, http.StatusUnauthorized, `{"ok":false,"error_code":401,"description":"Unauthorized: bot`+testToken+` rejected"}`)
		}))

		err := client.DeleteWebhook(testContext(t))
		if err == nil {
			t.Fatal("DeleteWebhook returned nil for a 401 response")
		}
		message := err.Error()
		if strings.Contains(message, testToken) {
			t.Fatal("error message contains the bot token")
		}
		if !strings.Contains(message, "<redacted>") {
			t.Error("error message does not mark the redaction")
		}
		if !strings.HasPrefix(message, "telegram deleteWebhook: ") {
			t.Errorf("error message %q lacks the method prefix", message)
		}
	})

	t.Run("transport error omits the raw URL", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		baseURL := server.URL
		client := New(testToken, WithBaseURL(baseURL), WithHTTPClient(server.Client()))
		server.Close()

		err := client.DeleteWebhook(testContext(t))
		if err == nil {
			t.Fatal("DeleteWebhook returned nil for a closed server")
		}
		message := err.Error()
		if strings.Contains(message, testToken) {
			t.Fatal("error message contains the bot token")
		}
		if strings.Contains(message, baseURL) {
			t.Error("error message contains the raw request URL")
		}
		if !strings.HasPrefix(message, "telegram deleteWebhook: ") {
			t.Errorf("error message %q lacks the method prefix", message)
		}
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			t.Error("error chain exposes *url.Error")
		}
	})
}

func TestContextCancellationIsClassified(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	var once sync.Once
	client, baseURL := newTestServerClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan error, 1)
	go func() {
		_, pollErr := client.GetUpdates(ctx, 0, 30*time.Second)
		result <- pollErr
	}()

	<-started
	cancel()

	err := <-result
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want errors.Is(err, context.Canceled)", err)
	}
	assertSanitizedError(t, err, "getUpdates", baseURL)
}

func TestContextDeadlineIsClassified(t *testing.T) {
	release := make(chan struct{})

	client, baseURL := newTestServerClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := client.GetUpdates(ctx, 0, 30*time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want errors.Is(err, context.DeadlineExceeded)", err)
	}
	assertSanitizedError(t, err, "getUpdates", baseURL)
}

func TestHTTPClientTimeoutIsClassified(t *testing.T) {
	release := make(chan struct{})

	client, baseURL := newTestServerClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}), WithHTTPClient(&http.Client{Timeout: 50 * time.Millisecond}))
	t.Cleanup(func() { close(release) })

	_, err := client.GetUpdates(context.Background(), 0, 30*time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want errors.Is(err, context.DeadlineExceeded) for a client timeout", err)
	}
	assertSanitizedError(t, err, "getUpdates", baseURL)
}

func Test429RetrySleepCancellationIsClassified(t *testing.T) {
	sleepStarted := make(chan struct{})
	client, baseURL := newTestServerClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond(w, http.StatusTooManyRequests, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 30","parameters":{"retry_after":30}}`)
	}), WithSleep(func(ctx context.Context, d time.Duration) error {
		close(sleepStarted)
		<-ctx.Done()
		return ctx.Err()
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan error, 1)
	go func() {
		_, pollErr := client.GetUpdates(ctx, 0, 30*time.Second)
		result <- pollErr
	}()

	<-sleepStarted
	cancel()

	err := <-result
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want errors.Is(err, context.Canceled) after retry sleep", err)
	}
	assertSanitizedError(t, err, "getUpdates", baseURL)
}

func TestEscapeAndTruncateCountsUTF16AndKeepsEntitiesIntact(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{
			name: "escapes named entities and leaves quotes",
			in:   `a<b>&'"`,
			max:  100,
			want: `a&lt;b&gt;&amp;&#39;"`,
		},
		{
			name: "exact fit is not truncated",
			in:   "a&",
			max:  6,
			want: "a&amp;",
		},
		{
			name: "truncates only at an entity boundary",
			in:   "a<b>rest",
			max:  6,
			want: "a&lt;…",
		},
		{
			name: "counts UTF-16 code units not runes",
			in:   "😀😀",
			max:  3,
			want: "😀…",
		},
		{
			name: "zero budget",
			in:   "abc",
			max:  0,
			want: "",
		},
		{
			name: "one unit budget holds only the ellipsis",
			in:   "ab",
			max:  1,
			want: "…",
		},
		{
			name: "fits without truncation",
			in:   "short",
			max:  5,
			want: "short",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EscapeAndTruncate(tt.in, tt.max)
			if got != tt.want {
				t.Fatalf("EscapeAndTruncate(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
			units := len(utf16.Encode([]rune(got)))
			if units > tt.max {
				t.Errorf("output uses %d UTF-16 units, over budget %d", units, tt.max)
			}
			prefix := strings.TrimSuffix(got, "…")
			if strings.Contains(prefix, "&") {
				if i := strings.LastIndex(prefix, "&"); !strings.Contains(prefix[i:], ";") {
					t.Fatalf("output %q ends inside an HTML entity", got)
				}
			}
		})
	}
}

func TestDeleteWebhook(t *testing.T) {
	var (
		gotPath   string
		gotMethod string
	)
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		respond(w, http.StatusOK, `{"ok":true,"result":true}`)
	}))

	if err := client.DeleteWebhook(testContext(t)); err != nil {
		t.Fatalf("DeleteWebhook returned an error: %v", err)
	}
	if want := "/bot" + testToken + "/deleteWebhook"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want %q", gotMethod, http.MethodPost)
	}
}

func TestDefaultBaseURL(t *testing.T) {
	client := New(testToken)
	if client.baseURL != "https://api.telegram.org" {
		t.Errorf("baseURL = %q, want https://api.telegram.org", client.baseURL)
	}
}

func TestSendMessageRejectsTooLong(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		respond(w, http.StatusOK, `{"ok":true,"result":{"message_id":1}}`)
	}))

	tooLong := strings.Repeat("a", maxMessageUTF16+1)
	err := client.SendMessage(testContext(t), 1, tooLong)
	if err == nil {
		t.Fatal("SendMessage accepted a message over the UTF-16 limit")
	}
	if !errors.Is(err, ErrMessageTooLong) {
		t.Errorf("error = %v, want ErrMessageTooLong", err)
	}
	if !strings.HasPrefix(err.Error(), "telegram sendMessage: ") {
		t.Errorf("error %q lacks the method prefix", err.Error())
	}

	mu.Lock()
	got := requests
	mu.Unlock()
	if got != 0 {
		t.Errorf("too-long message caused %d HTTP request(s), want 0", got)
	}

	if err := client.SendMessage(testContext(t), 1, strings.Repeat("a", maxMessageUTF16)); err != nil {
		t.Fatalf("SendMessage rejected text at exactly the limit: %v", err)
	}
}

func Test429GivesUpAfterThreeRetries(t *testing.T) {
	var (
		mu       sync.Mutex
		requests int
		slept    []time.Duration
	)
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		respond(w, http.StatusTooManyRequests, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 1","parameters":{"retry_after":1}}`)
	}), WithSleep(func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		slept = append(slept, d)
		return nil
	}))

	err := client.SendMessage(testContext(t), 1, "hello")
	if err == nil {
		t.Fatal("SendMessage returned nil after exhausting retries")
	}
	if !strings.Contains(err.Error(), "telegram sendMessage:") || !strings.Contains(err.Error(), "429") {
		t.Errorf("error %q does not describe the exhausted 429", err.Error())
	}

	mu.Lock()
	defer mu.Unlock()
	if requests != 4 {
		t.Errorf("requests = %d, want 4 (initial + 3 retries)", requests)
	}
	if len(slept) != 3 {
		t.Errorf("sleep calls = %d, want 3", len(slept))
	}
}

func Test429UsesRetryAfterHeaderWhenBodyHasNoParameters(t *testing.T) {
	var (
		mu       sync.Mutex
		requests int
		slept    []time.Duration
	)
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		call := requests
		mu.Unlock()
		if call == 1 {
			w.Header().Set("Retry-After", "4")
			respond(w, http.StatusTooManyRequests, `{"ok":false,"error_code":429,"description":"Too Many Requests"}`)
			return
		}
		respond(w, http.StatusOK, `{"ok":true,"result":true}`)
	}), WithSleep(func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		slept = append(slept, d)
		return nil
	}))

	if err := client.DeleteWebhook(testContext(t)); err != nil {
		t.Fatalf("DeleteWebhook returned an error after retry: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(slept) != 1 || slept[0] != 4*time.Second {
		t.Errorf("sleep calls = %v, want [4s]", slept)
	}
}

func TestSendMessageIDReturnsMessageID(t *testing.T) {
	var (
		gotPath string
		gotForm url.Values
	)
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse sendMessage form: %v", err)
		}
		gotForm = r.PostForm
		respond(w, http.StatusOK, `{"ok":true,"result":{"message_id":77}}`)
	}))

	const html = "card <b>text</b>"
	messageID, err := client.SendMessageID(testContext(t), 1234, html)
	if err != nil {
		t.Fatalf("SendMessageID returned an error: %v", err)
	}
	if messageID != 77 {
		t.Errorf("message ID = %d, want 77", messageID)
	}
	if want := "/bot" + testToken + "/sendMessage"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if got := gotForm.Get("chat_id"); got != "1234" {
		t.Errorf("chat_id = %q, want %q", got, "1234")
	}
	if got := gotForm.Get("text"); got != html {
		t.Errorf("text = %q, want the HTML body unchanged", got)
	}
	if got := gotForm.Get("parse_mode"); got != "HTML" {
		t.Errorf("parse_mode = %q, want %q", got, "HTML")
	}
}

func TestSendMessageStillReturnsErrorOnly(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()

		if call == 1 {
			respond(w, http.StatusOK, `{"ok":true,"result":{"message_id":88}}`)
			return
		}
		respond(w, http.StatusBadRequest, `{"ok":false,"error_code":400,"description":"Bad Request: message is too long"}`)
	}))

	// Pin the existing error-only signature while SendMessage delegates to
	// SendMessageID internally.
	var send func(context.Context, int64, string) error = client.SendMessage
	if err := send(testContext(t), 7, "hello"); err != nil {
		t.Fatalf("SendMessage returned an error for a successful send: %v", err)
	}

	err := send(testContext(t), 7, "hello")
	if err == nil {
		t.Fatal("SendMessage returned nil for an API error")
	}
	message := err.Error()
	if !strings.HasPrefix(message, "telegram sendMessage: ") {
		t.Errorf("error %q lacks the sendMessage prefix", message)
	}
	if !strings.Contains(message, "400") || !strings.Contains(message, "message is too long") {
		t.Errorf("error %q does not describe the API failure", message)
	}
}

func TestEditMessageTextSendsChatMessageAndHTML(t *testing.T) {
	var (
		gotPath string
		gotForm url.Values
	)
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse editMessageText form: %v", err)
		}
		gotForm = r.PostForm
		respond(w, http.StatusOK, `{"ok":true,"result":true}`)
	}))

	if err := client.EditMessageText(testContext(t), -100123, 456, "<b>Running</b>"); err != nil {
		t.Fatalf("EditMessageText returned an error: %v", err)
	}
	if want := "/bot" + testToken + "/editMessageText"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if got := gotForm.Get("chat_id"); got != "-100123" {
		t.Errorf("chat_id = %q, want %q", got, "-100123")
	}
	if got := gotForm.Get("message_id"); got != "456" {
		t.Errorf("message_id = %q, want %q", got, "456")
	}
	if got := gotForm.Get("text"); got != "<b>Running</b>" {
		t.Errorf("text = %q, want the HTML body unchanged", got)
	}
	if got := gotForm.Get("parse_mode"); got != "HTML" {
		t.Errorf("parse_mode = %q, want %q", got, "HTML")
	}
}

func TestEditMessageTextRejectsTooLong(t *testing.T) {
	var (
		mu       sync.Mutex
		requests int
	)
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		respond(w, http.StatusOK, `{"ok":true,"result":true}`)
	}))

	tooLong := strings.Repeat("a", maxMessageUTF16+1)
	err := client.EditMessageText(testContext(t), 1, 2, tooLong)
	if err == nil {
		t.Fatal("EditMessageText accepted a message over the UTF-16 limit")
	}
	if !errors.Is(err, ErrMessageTooLong) {
		t.Errorf("error = %v, want ErrMessageTooLong", err)
	}
	if !strings.HasPrefix(err.Error(), "telegram editMessageText: ") {
		t.Errorf("error %q lacks the editMessageText prefix", err.Error())
	}

	mu.Lock()
	got := requests
	mu.Unlock()
	if got != 0 {
		t.Errorf("too-long edit caused %d HTTP request(s), want 0", got)
	}
}

func TestSendMediaGroupBuildsDocumentsAndFiles(t *testing.T) {
	type mediaEntry struct {
		Type    string `json:"type"`
		Media   string `json:"media"`
		Caption string `json:"caption"`
	}

	docs := []Document{
		{Filename: "codemap.md", Data: []byte("first document"), Caption: "<b>Code map</b>"},
		{Filename: "routing.md", Data: []byte{0x00, 0x01, 0xff, 'x'}},
	}

	var (
		gotPath        string
		gotContentType string
		gotChatID      string
		gotParseMode   string
		gotMedia       []mediaEntry
		gotFilenames   = map[string]string{}
		gotFiles       = map[string][]byte{}
	)

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart form: %v", err)
			respond(w, http.StatusInternalServerError, `{"ok":false,"error_code":500,"description":"bad multipart"}`)
			return
		}
		gotChatID = r.FormValue("chat_id")
		gotParseMode = r.FormValue("parse_mode")
		if err := json.Unmarshal([]byte(r.FormValue("media")), &gotMedia); err != nil {
			t.Errorf("decode media field: %v", err)
		}
		for _, field := range []string{"file0", "file1"} {
			files := r.MultipartForm.File[field]
			if len(files) != 1 {
				t.Errorf("%s parts = %d, want 1", field, len(files))
				continue
			}
			gotFilenames[field] = files[0].Filename
			file, err := files[0].Open()
			if err != nil {
				t.Errorf("open %s: %v", field, err)
				continue
			}
			data, readErr := io.ReadAll(file)
			_ = file.Close()
			if readErr != nil {
				t.Errorf("read %s: %v", field, readErr)
				continue
			}
			gotFiles[field] = data
		}
		respond(w, http.StatusOK, `{"ok":true,"result":[]}`)
	}))

	if err := client.SendMediaGroup(testContext(t), -1001, docs); err != nil {
		t.Fatalf("SendMediaGroup returned an error: %v", err)
	}

	if want := "/bot" + testToken + "/sendMediaGroup"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	mediaType, _, err := mime.ParseMediaType(gotContentType)
	if err != nil {
		t.Fatalf("parse content type %q: %v", gotContentType, err)
	}
	if mediaType != "multipart/form-data" {
		t.Errorf("media type = %q, want multipart/form-data", mediaType)
	}
	if gotChatID != "-1001" {
		t.Errorf("chat_id = %q, want %q", gotChatID, "-1001")
	}
	if gotParseMode != "HTML" {
		t.Errorf("parse_mode = %q, want %q", gotParseMode, "HTML")
	}

	wantMedia := []mediaEntry{
		{Type: "document", Media: "attach://file0", Caption: "<b>Code map</b>"},
		{Type: "document", Media: "attach://file1"},
	}
	if len(gotMedia) != len(wantMedia) {
		t.Fatalf("media entries = %d, want %d", len(gotMedia), len(wantMedia))
	}
	for i, want := range wantMedia {
		if gotMedia[i] != want {
			t.Errorf("media[%d] = %+v, want %+v", i, gotMedia[i], want)
		}
	}

	for i, doc := range docs {
		field := "file" + strconv.Itoa(i)
		if got := gotFilenames[field]; got != doc.Filename {
			t.Errorf("%s filename = %q, want %q", field, got, doc.Filename)
		}
		if !bytes.Equal(gotFiles[field], doc.Data) {
			t.Errorf("%s bytes = %v, want %v", field, gotFiles[field], doc.Data)
		}
	}
}

func TestSendMediaGroupRequiresTwoToTen(t *testing.T) {
	makeDocs := func(n int) []Document {
		docs := make([]Document, n)
		for i := range docs {
			docs[i] = Document{Filename: "doc.md", Data: []byte("x")}
		}
		return docs
	}

	tests := []struct {
		name string
		docs []Document
	}{
		{name: "none", docs: nil},
		{name: "one", docs: makeDocs(1)},
		{name: "eleven", docs: makeDocs(11)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var (
				mu       sync.Mutex
				requests int
			)
			client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests++
				mu.Unlock()
				respond(w, http.StatusOK, `{"ok":true,"result":[]}`)
			}))

			err := client.SendMediaGroup(testContext(t), 1, tt.docs)
			if err == nil {
				t.Fatal("SendMediaGroup accepted an invalid document count")
			}
			if !strings.HasPrefix(err.Error(), "telegram sendMediaGroup: ") {
				t.Errorf("error %q lacks the sendMediaGroup prefix", err.Error())
			}
			if !strings.Contains(err.Error(), "2 to 10 documents") {
				t.Errorf("error %q does not describe the document count bounds", err.Error())
			}

			mu.Lock()
			got := requests
			mu.Unlock()
			if got != 0 {
				t.Errorf("invalid document count caused %d HTTP request(s), want 0", got)
			}
		})
	}
}

func TestSendMediaGroupRetries429(t *testing.T) {
	var (
		mu           sync.Mutex
		bodies       [][]byte
		contentTypes []string
		slept        []time.Duration
	)
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read media group body: %v", err)
		}

		mu.Lock()
		bodies = append(bodies, body)
		contentTypes = append(contentTypes, r.Header.Get("Content-Type"))
		call := len(bodies)
		mu.Unlock()

		if call == 1 {
			respond(w, http.StatusTooManyRequests, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 1","parameters":{"retry_after":1}}`)
			return
		}
		respond(w, http.StatusOK, `{"ok":true,"result":[]}`)
	}), WithSleep(func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		slept = append(slept, d)
		return nil
	}))

	docs := []Document{
		{Filename: "a.md", Data: []byte("alpha")},
		{Filename: "b.md", Data: []byte("beta")},
	}
	if err := client.SendMediaGroup(testContext(t), 1, docs); err != nil {
		t.Fatalf("SendMediaGroup returned an error after retry: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want 2", len(bodies))
	}
	if len(bodies[0]) == 0 {
		t.Fatal("first media group body is empty")
	}
	if !bytes.Equal(bodies[0], bodies[1]) {
		t.Error("429 retry did not replay the exact multipart body")
	}
	if contentTypes[0] != contentTypes[1] {
		t.Errorf("retry content type = %q, want %q", contentTypes[1], contentTypes[0])
	}
	if len(slept) != 1 || slept[0] != time.Second {
		t.Errorf("sleep calls = %v, want [1s]", slept)
	}
}

func TestSetMyCommandsSendsJSONCommands(t *testing.T) {
	var (
		gotPath        string
		gotContentType string
		gotForm        url.Values
	)
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse setMyCommands form: %v", err)
		}
		gotForm = r.PostForm
		respond(w, http.StatusOK, `{"ok":true,"result":true}`)
	}))

	commands := []BotCommand{
		{Command: "gist", Description: "Start a job"},
		{Command: "help", Description: "Show help"},
	}
	if err := client.SetMyCommands(testContext(t), commands); err != nil {
		t.Fatalf("SetMyCommands returned an error: %v", err)
	}

	if want := "/bot" + testToken + "/setMyCommands"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	mediaType, _, err := mime.ParseMediaType(gotContentType)
	if err != nil {
		t.Fatalf("parse content type %q: %v", gotContentType, err)
	}
	if mediaType != formContentType {
		t.Errorf("media type = %q, want %q", mediaType, formContentType)
	}

	raw := gotForm.Get("commands")
	var decoded []struct {
		Command     string `json:"command"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("decode commands value %q: %v", raw, err)
	}
	if len(decoded) != len(commands) {
		t.Fatalf("commands = %d, want %d", len(decoded), len(commands))
	}
	for i, want := range commands {
		if decoded[i].Command != want.Command || decoded[i].Description != want.Description {
			t.Errorf("commands[%d] = %+v, want %+v", i, decoded[i], want)
		}
	}

	t.Run("api error is sanitized", func(t *testing.T) {
		errorClient, baseURL := newTestServerClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			respond(w, http.StatusUnauthorized, `{"ok":false,"error_code":401,"description":"Unauthorized: bot`+testToken+` rejected"}`)
		}))

		err := errorClient.SetMyCommands(testContext(t), commands)
		assertSanitizedError(t, err, "setMyCommands", baseURL)
	})
}
