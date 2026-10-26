package telegram

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	all := append([]Option{WithBaseURL(server.URL), WithHTTPClient(server.Client())}, opts...)
	return New(testToken, all...)
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
	})
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
