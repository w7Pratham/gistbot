package opencode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testPassword is deliberately fake. Tests never print it on failure so a real
// password could not leak through copied test output.
const (
	testPassword  = "test-api-password"
	testSessionID = "ses_test"
)

func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(func() {
		// Force-close connections first so a handler waiting on request
		// cancellation cannot block Server.Close.
		server.CloseClientConnections()
		server.Close()
	})
	return NewClient(server.URL, testPassword)
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

func requestBody(t *testing.T, r *http.Request) string {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read request body: %v", err)
		return ""
	}
	return string(body)
}

func TestInfoUsesBasicAuth(t *testing.T) {
	var (
		gotMethod   string
		gotPath     string
		gotUsername string
		gotPassword string
		gotOK       bool
	)

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotUsername, gotPassword, gotOK = r.BasicAuth()
		respond(w, http.StatusOK, `{"version":"test"}`)
	}))

	if err := client.Info(testContext(t)); err != nil {
		t.Fatalf("Info returned an error: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want %q", gotMethod, http.MethodGet)
	}
	if gotPath != "/api/info" {
		t.Errorf("path = %q, want %q", gotPath, "/api/info")
	}
	if !gotOK {
		t.Fatal("request did not carry Basic auth")
	}
	if gotUsername != "opencode" {
		t.Errorf("basic auth username = %q, want %q", gotUsername, "opencode")
	}
	if gotPassword != testPassword {
		t.Error("basic auth password did not match the configured password")
	}
}

func TestNewClientTrimsTrailingSlash(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		respond(w, http.StatusOK, `{}`)
	}))
	t.Cleanup(server.Close)

	client := NewClient(server.URL+"/", testPassword)
	if err := client.Info(testContext(t)); err != nil {
		t.Fatalf("Info returned an error: %v", err)
	}
	if gotPath != "/api/info" {
		t.Errorf("path = %q, want /api/info with no duplicate slash", gotPath)
	}
}

func TestCreateSessionBody(t *testing.T) {
	const (
		location  = "/work/repo"
		directory = "/work/repo"
	)

	tests := []struct {
		name    string
		variant string
		want    string
	}{
		{
			name:    "variant omitted when empty",
			variant: "",
			want:    `{"agent":"repo-scout","model":{"providerID":"deepseek","id":"deepseek-flash"},"location":{"directory":"` + directory + `"}}`,
		},
		{
			name:    "variant included when set",
			variant: "repo-scout",
			want:    `{"agent":"repo-scout","model":{"providerID":"deepseek","id":"deepseek-flash","variant":"repo-scout"},"location":{"directory":"` + directory + `"}}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var (
				gotMethod string
				gotPath   string
				gotBody   string
			)

			client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				gotPath = r.URL.Path
				gotBody = requestBody(t, r)
				respond(w, http.StatusOK, `{"data":{"id":"ses_created"}}`)
			}))

			id, err := client.CreateSession(testContext(t), location, "repo-scout", "deepseek", "deepseek-flash", test.variant)
			if err != nil {
				t.Fatalf("CreateSession returned an error: %v", err)
			}
			if id != "ses_created" {
				t.Errorf("session id = %q, want %q", id, "ses_created")
			}
			if gotMethod != http.MethodPost {
				t.Errorf("method = %q, want %q", gotMethod, http.MethodPost)
			}
			if gotPath != "/api/session" {
				t.Errorf("path = %q, want %q", gotPath, "/api/session")
			}
			if gotBody != test.want {
				t.Errorf("body = %s, want %s", gotBody, test.want)
			}
		})
	}
}

func TestCreateSessionRequiresID(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond(w, http.StatusOK, `{"data":{}}`)
	}))

	if _, err := client.CreateSession(testContext(t), "/work/repo", "repo-scout", "deepseek", "deepseek-flash", ""); err == nil {
		t.Fatal("CreateSession accepted a response without a session id")
	}
}

func TestPromptBody(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotBody   string
	)

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotBody = requestBody(t, r)
		respond(w, http.StatusOK, `{"data":{}}`)
	}))

	if err := client.Prompt(testContext(t), testSessionID, "scan the repo"); err != nil {
		t.Fatalf("Prompt returned an error: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want %q", gotMethod, http.MethodPost)
	}
	if want := "/api/session/" + testSessionID + "/prompt"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if want := `{"text":"scan the repo"}`; gotBody != want {
		t.Errorf("body = %s, want %s", gotBody, want)
	}
}

func TestWait(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
	)

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		respond(w, http.StatusOK, `{"data":{}}`)
	}))

	if err := client.Wait(testContext(t), testSessionID); err != nil {
		t.Fatalf("Wait returned an error: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want %q", gotMethod, http.MethodPost)
	}
	if want := "/api/experimental/session/" + testSessionID + "/wait"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

func TestWaitUsesCallerContext(t *testing.T) {
	started := make(chan struct{})
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan error, 1)
	go func() {
		result <- client.Wait(ctx, testSessionID)
	}()

	<-started
	cancel()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Wait returned nil after its context was cancelled")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Wait error does not preserve context.Canceled: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait ignored the caller's context cancellation")
	}
}

func TestInterrupt(t *testing.T) {
	tests := []struct {
		name        string
		interrupted bool
		body        string
	}{
		{name: "interrupted", interrupted: true, body: `{"interrupted":true}`},
		{name: "already idle", interrupted: false, body: `{"interrupted":false}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var (
				gotMethod string
				gotPath   string
				gotQuery  string
			)

			client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				gotPath = r.URL.Path
				gotQuery = r.URL.RawQuery
				respond(w, http.StatusOK, test.body)
			}))

			interrupted, err := client.Interrupt(testContext(t), testSessionID)
			if err != nil {
				t.Fatalf("Interrupt returned an error: %v", err)
			}
			if interrupted != test.interrupted {
				t.Errorf("interrupted = %v, want %v", interrupted, test.interrupted)
			}
			if gotMethod != http.MethodPost {
				t.Errorf("method = %q, want %q", gotMethod, http.MethodPost)
			}
			if want := "/api/session/" + testSessionID + "/interrupt"; gotPath != want {
				t.Errorf("path = %q, want %q", gotPath, want)
			}
			if gotQuery != "resume=false" {
				t.Errorf("query = %q, want %q", gotQuery, "resume=false")
			}
		})
	}
}

func TestMessagesParsesAssistantTextAndIdleOutcome(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotQuery  string
	)

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		respond(w, http.StatusOK, `{"data":[
			{"type":"assistant","content":[
				{"type":"text","text":"first"},
				{"type":"reasoning","text":"hidden"},
				{"type":"text","text":"second"}
			]},
			{"type":"idle","outcome":"succeeded"}
		]}`)
	}))

	messages, err := client.Messages(testContext(t), testSessionID)
	if err != nil {
		t.Fatalf("Messages returned an error: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want %q", gotMethod, http.MethodGet)
	}
	if want := "/api/session/" + testSessionID + "/message"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotQuery != "order=desc&limit=50" {
		t.Errorf("query = %q, want %q", gotQuery, "order=desc&limit=50")
	}

	if len(messages) != 2 {
		t.Fatalf("len(messages) = %d, want 2", len(messages))
	}
	if messages[0].Type != "assistant" {
		t.Errorf("first message type = %q, want assistant", messages[0].Type)
	}
	if len(messages[0].Content) != 3 {
		t.Fatalf("first message content = %d, want 3", len(messages[0].Content))
	}
	if got := messages[0].Content[0].Text; got != "first" {
		t.Errorf("first text = %q, want first", got)
	}
	if got := messages[0].Content[2].Text; got != "second" {
		t.Errorf("last text = %q, want second", got)
	}
	if messages[1].Type != "idle" {
		t.Errorf("second message type = %q, want idle", messages[1].Type)
	}
	if messages[1].Outcome != "succeeded" {
		t.Errorf("outcome = %q, want succeeded", messages[1].Outcome)
	}
}

func TestSessionLog(t *testing.T) {
	var gotPath string

	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		respond(w, http.StatusOK, `{"data":[
			{"type":"message","text":"hello"},
			{"type":"idle","outcome":"succeeded"}
		]}`)
	}))

	log, err := client.SessionLog(testContext(t), testSessionID)
	if err != nil {
		t.Fatalf("SessionLog returned an error: %v", err)
	}
	if want := "/api/experimental/session/" + testSessionID + "/log"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if !strings.Contains(log, "hello") || !strings.Contains(log, "succeeded") {
		t.Errorf("log = %q, want both log items", log)
	}
	if lines := strings.Split(log, "\n"); len(lines) != 2 {
		t.Errorf("log has %d lines, want 2", len(lines))
	}
}

func TestSessionLogPlainText(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond(w, http.StatusOK, "line one\nline two\n")
	}))

	log, err := client.SessionLog(testContext(t), testSessionID)
	if err != nil {
		t.Fatalf("SessionLog returned an error: %v", err)
	}
	if log != "line one\nline two\n" {
		t.Errorf("log = %q, want the raw body", log)
	}
}

func TestAPIErrorIncludesStatusAndBody(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond(w, http.StatusInternalServerError, `{"error":"model unavailable"}`)
	}))

	err := client.Info(testContext(t))
	if err == nil {
		t.Fatal("Info returned nil for an HTTP 500")
	}
	message := err.Error()
	if !strings.Contains(message, "500") {
		t.Errorf("error %q does not contain the HTTP status", message)
	}
	if !strings.Contains(message, "model unavailable") {
		t.Errorf("error %q does not contain the response body", message)
	}
}

func TestAPIErrorsNeverContainPassword(t *testing.T) {
	authToken := base64.StdEncoding.EncodeToString([]byte("opencode:" + testPassword))

	t.Run("status body", func(t *testing.T) {
		client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			respond(w, http.StatusUnauthorized, "denied "+testPassword+" "+r.Header.Get("Authorization"))
		}))

		err := client.Info(testContext(t))
		if err == nil {
			t.Fatal("Info returned nil for an HTTP 401")
		}
		message := err.Error()
		if strings.Contains(message, testPassword) {
			t.Error("error message contains the API password")
		}
		if strings.Contains(message, authToken) {
			t.Error("error message contains the Basic auth header value")
		}
	})

	t.Run("transport error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		client := NewClient(server.URL, testPassword)
		server.Close()

		err := client.Info(testContext(t))
		if err == nil {
			t.Fatal("Info returned nil after the server closed")
		}
		if strings.Contains(err.Error(), testPassword) {
			t.Error("transport error contains the API password")
		}
	})
}

func TestListModelsParsesLines(t *testing.T) {
	bin := writeFakeModels(t, `#!/bin/sh
set -eu
if [ "$#" -ne 1 ] || [ "$1" != "models" ]; then
  echo "unexpected argv" >&2
  exit 3
fi
printf '  opencode/deepseek-flash-free  \n\nopencode/deepseek-flash\r\n\t\n'
`)

	models, err := ListModels(testContext(t), bin)
	if err != nil {
		t.Fatalf("ListModels returned an error: %v", err)
	}
	want := []string{"opencode/deepseek-flash-free", "opencode/deepseek-flash"}
	if len(models) != len(want) {
		t.Fatalf("models = %#v, want %#v", models, want)
	}
	for i := range want {
		if models[i] != want[i] {
			t.Errorf("models[%d] = %q, want %q", i, models[i], want[i])
		}
	}
}

func TestListModelsFiltersBotSecretsFromChildEnvironment(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "child-env.txt")
	home := t.TempDir()
	xdg := t.TempDir()

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("FAKE_MODELS_ENV", envFile)
	t.Setenv("TELEGRAM_BOT_TOKEN", "forbidden-telegram-token")
	t.Setenv("TELEGRAM_API_URL", "https://forbidden.invalid")
	t.Setenv("OWNER_USER_ID", "424242")
	t.Setenv("OPENCODE_PASSWORD", "forbidden-cli-password")
	t.Setenv("OPENCODE_SERVER_PASSWORD", "forbidden-server-password")
	t.Setenv("OPENCODE_API_PASSWORD", "forbidden-api-password")

	bin := writeFakeModels(t, `#!/bin/sh
set -eu
env > "$FAKE_MODELS_ENV"
printf 'opencode/deepseek-flash\n'
`)

	models, err := ListModels(testContext(t), bin)
	if err != nil {
		t.Fatalf("ListModels returned an error: %v", err)
	}
	if len(models) != 1 || models[0] != "opencode/deepseek-flash" {
		t.Fatalf("models = %#v, want one model", models)
	}

	raw, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read child environment: %v", err)
	}

	env := make(map[string]string)
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		env[key] = value
	}

	forbiddenKeys := []string{
		"TELEGRAM_BOT_TOKEN",
		"TELEGRAM_API_URL",
		"OWNER_USER_ID",
		"OPENCODE_PASSWORD",
		"OPENCODE_SERVER_PASSWORD",
		"OPENCODE_API_PASSWORD",
	}
	for _, key := range forbiddenKeys {
		if _, ok := env[key]; ok {
			t.Errorf("child environment contains forbidden key %s", key)
		}
	}
	for key := range env {
		if strings.HasPrefix(key, "TELEGRAM_") {
			t.Errorf("child environment contains forbidden TELEGRAM_ key %s", key)
		}
	}
	for _, secret := range []string{
		"forbidden-telegram-token",
		"forbidden-cli-password",
		"forbidden-server-password",
		"forbidden-api-password",
	} {
		if strings.Contains(string(raw), secret) {
			t.Error("child environment contains a forbidden secret value")
		}
	}

	if got := env["HOME"]; got != home {
		t.Errorf("HOME = %q, want the preserved value", got)
	}
	if got := env["XDG_CONFIG_HOME"]; got != xdg {
		t.Errorf("XDG_CONFIG_HOME = %q, want the preserved value", got)
	}
	if got := env["FAKE_MODELS_ENV"]; got != envFile {
		t.Errorf("FAKE_MODELS_ENV = %q, want %q", got, envFile)
	}
	if env["PATH"] == "" {
		t.Error("PATH was removed from the child environment")
	}
}

func TestListModelsPropagatesFailure(t *testing.T) {
	bin := writeFakeModels(t, `#!/bin/sh
exit 2
`)

	if _, err := ListModels(testContext(t), bin); err == nil {
		t.Fatal("ListModels returned nil for a failing OpenCode binary")
	}
}

func TestListModelsHonorsContext(t *testing.T) {
	bin := writeFakeModels(t, `#!/bin/sh
while :; do :; done
`)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := ListModels(ctx, bin)
	if err == nil {
		t.Fatal("ListModels returned nil after its context expired")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("ListModels error does not preserve context.DeadlineExceeded: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("ListModels took %v, want prompt cancellation", elapsed)
	}
}

func writeFakeModels(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake opencode: %v", err)
	}
	return path
}

// TestMessageJSONShape pins the wire tags Task 10 relies on.
func TestMessageJSONShape(t *testing.T) {
	raw := `{"type":"assistant","outcome":"","content":[{"type":"text","text":"hi"},{"type":"reasoning"}]}`

	var message Message
	if err := json.Unmarshal([]byte(raw), &message); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	if message.Type != "assistant" {
		t.Errorf("type = %q, want assistant", message.Type)
	}
	if message.Outcome != "" {
		t.Errorf("outcome = %q, want empty", message.Outcome)
	}
	if len(message.Content) != 2 {
		t.Fatalf("len(content) = %d, want 2", len(message.Content))
	}
	if message.Content[0].Type != "text" || message.Content[0].Text != "hi" {
		t.Errorf("content[0] = %+v, want text hi", message.Content[0])
	}
	if message.Content[1].Type != "reasoning" || message.Content[1].Text != "" {
		t.Errorf("content[1] = %+v, want reasoning with no text", message.Content[1])
	}
}
