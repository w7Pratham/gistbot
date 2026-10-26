package bot

import (
	"context"
	"errors"
	"testing"

	"gistbot/internal/config"
	"gistbot/internal/jobs"
	"gistbot/internal/state"
)

// snapshotState returns the in-memory state after a command.
func (f *botFixture) snapshotState(t *testing.T) state.Data {
	t.Helper()
	return f.store.Snapshot()
}

// reopenState reloads the state file to prove a selection was persisted rather
// than only updated in memory.
func (f *botFixture) reopenState(t *testing.T) state.Data {
	t.Helper()
	store, err := state.Open(f.cfg.StateFile, state.Data{})
	if err != nil {
		t.Fatalf("reopen state: %v", err)
	}
	return store.Snapshot()
}

// lastMessage returns the most recent Telegram message.
func (f *botFixture) lastMessage(t *testing.T) string {
	t.Helper()
	texts := f.sender.messageTexts()
	if len(texts) == 0 {
		t.Fatal("no messages sent")
	}
	return texts[len(texts)-1]
}

func TestModelOutputIsCompact(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		f := newBotFixture(t)

		f.handle(privateUpdate("/model"))

		text := f.lastMessage(t)
		for _, want := range []string{
			"Model", "flash (test/flash)", "/model", "free", "pro",
		} {
			requireContains(t, text, want)
		}
		requireContains(t, text, "<code>")
		for _, unwanted := range []string{
			"Free default:", "Free override:", "Aliases:",
		} {
			requireNotContains(t, text, unwanted)
		}
		if got := utf16Count(text); got > 400 {
			t.Errorf("model output = %d UTF-16 units, want compact", got)
		}
	})

	t.Run("free override", func(t *testing.T) {
		f := newBotFixture(t)
		f.setModel(t, "free")
		if err := f.store.SetFreeModel("test/pro#high"); err != nil {
			t.Fatalf("SetFreeModel: %v", err)
		}

		f.handle(privateUpdate("/model"))

		text := f.lastMessage(t)
		requireContains(t, text, "free (test/pro#high)")
		requireNotContains(t, text, "Free default:")
	})
}

func TestModelSetsAliasAndPersists(t *testing.T) {
	f := newBotFixture(t)

	f.handle(privateUpdate("/model pro"))
	data := f.reopenState(t)
	if data.Model != "pro" {
		t.Errorf("persisted Model = %q, want pro", data.Model)
	}
	requireContains(t, f.lastMessage(t), "pro (test/pro#high)")

	f.handle(privateUpdate("/model free"))
	data = f.reopenState(t)
	if data.Model != "free" {
		t.Errorf("persisted Model = %q, want free", data.Model)
	}

	f.handle(privateUpdate("/model bogus"))
	data = f.reopenState(t)
	if data.Model != "free" {
		t.Errorf("persisted Model after unknown alias = %q, want free", data.Model)
	}
	requireContains(t, f.lastMessage(t), "Unknown model")

	f.handle(privateUpdate("/model pro extra"))
	data = f.reopenState(t)
	if data.Model != "free" {
		t.Errorf("persisted Model after extra args = %q, want free", data.Model)
	}
	requireContains(t, f.lastMessage(t), "Usage")
}

func TestModelFreeOverrideValidatesInstalledID(t *testing.T) {
	f := newBotFixture(t)

	f.handle(privateUpdate("/model free test/pro"))
	data := f.reopenState(t)
	if data.Model != "free" {
		t.Errorf("persisted Model = %q, want free", data.Model)
	}
	if data.FreeModel != "test/pro" {
		t.Errorf("persisted FreeModel = %q, want test/pro", data.FreeModel)
	}
	text := f.lastMessage(t)
	requireContains(t, text, "free (test/pro)")

	f.handle(privateUpdate("/model free test/flash"))
	data = f.reopenState(t)
	if data.FreeModel != "test/flash" {
		t.Errorf("persisted FreeModel = %q, want test/flash", data.FreeModel)
	}

	f.bot.listModels = func(context.Context) ([]string, error) {
		return nil, errors.New("opencode models: unavailable")
	}
	f.handle(privateUpdate("/model free test/pro"))
	data = f.reopenState(t)
	if data.FreeModel != "test/flash" {
		t.Errorf("persisted FreeModel after list failure = %q, want test/flash", data.FreeModel)
	}
	requireContains(t, f.lastMessage(t), "Could not list installed models")
}

func TestModelFreeOverrideAcceptsVariantSuffix(t *testing.T) {
	f := newBotFixture(t)

	f.handle(privateUpdate("/model free test/pro#high"))
	data := f.reopenState(t)
	if data.Model != "free" {
		t.Errorf("persisted Model = %q, want free", data.Model)
	}
	if data.FreeModel != "test/pro#high" {
		t.Errorf("persisted FreeModel = %q, want test/pro#high", data.FreeModel)
	}
	requireContains(t, f.lastMessage(t), "free (test/pro#high)")

	f.handle(privateUpdate("/gist https://github.com/example/project"))
	requests := f.jobs.requestList()
	if len(requests) != 1 {
		t.Fatalf("enqueued requests = %d, want 1", len(requests))
	}
	want := config.ModelRef{ProviderID: "test", ModelID: "pro", Variant: "high"}
	if requests[0].Model != want {
		t.Errorf("job Model = %v, want %v", requests[0].Model, want)
	}
	if requests[0].ModelAlias != "free" {
		t.Errorf("job ModelAlias = %q, want free", requests[0].ModelAlias)
	}
}

func TestModelFreeOverrideRejectsUnknownID(t *testing.T) {
	f := newBotFixture(t)
	f.setModel(t, "free")
	if err := f.store.SetFreeModel("test/flash"); err != nil {
		t.Fatalf("SetFreeModel: %v", err)
	}

	cases := []struct {
		name string
		text string
		want string
	}{
		{
			name: "unknown id",
			text: "/model free vendor/missing",
			want: "Unknown model",
		},
		{
			name: "case sensitive",
			text: "/model free Test/Flash",
			want: "Unknown model",
		},
		{
			name: "malformed reference",
			text: "/model free not-a-reference",
			want: "provider/model",
		},
		{
			name: "multiple variants",
			text: "/model free test/pro#high#again",
			want: "invalid",
		},
		{
			name: "empty variant",
			text: "/model free test/pro#",
			want: "invalid",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.handle(privateUpdate(tc.text))

			data := f.reopenState(t)
			if data.Model != "free" {
				t.Errorf("persisted Model = %q, want free", data.Model)
			}
			if data.FreeModel != "test/flash" {
				t.Errorf("persisted FreeModel = %q, want test/flash", data.FreeModel)
			}
			requireContains(t, f.lastMessage(t), tc.want)
		})
	}
}

func TestModelFreeResetReturnsToEnvDefault(t *testing.T) {
	f := newBotFixture(t)
	f.setModel(t, "free")
	if err := f.store.SetFreeModel("test/pro#high"); err != nil {
		t.Fatalf("SetFreeModel: %v", err)
	}

	f.handle(privateUpdate("/model free reset"))

	data := f.reopenState(t)
	if data.Model != "free" {
		t.Errorf("persisted Model = %q, want free", data.Model)
	}
	if data.FreeModel != "" {
		t.Errorf("persisted FreeModel = %q, want empty", data.FreeModel)
	}

	f.handle(privateUpdate("/model"))
	text := f.lastMessage(t)
	requireContains(t, text, "free (test/free)")
	requireNotContains(t, text, "Free override")

	f.handle(privateUpdate("/gist https://github.com/example/project"))
	requests := f.jobs.requestList()
	if len(requests) != 1 {
		t.Fatalf("enqueued requests = %d, want 1", len(requests))
	}
	if requests[0].Model != f.cfg.Models["free"] {
		t.Errorf("job Model = %v, want %v", requests[0].Model, f.cfg.Models["free"])
	}
	if requests[0].ModelAlias != "free" {
		t.Errorf("job ModelAlias = %q, want free", requests[0].ModelAlias)
	}
}

func TestRunnerOutputIsCompact(t *testing.T) {
	f := newBotFixture(t)

	f.handle(privateUpdate("/runner"))
	text := f.lastMessage(t)
	requireContains(t, text, "Runner")
	requireContains(t, text, "cli")
	requireContains(t, text, "/runner cli|api")
	requireNotContains(t, text, "Runner: ")

	f.handle(privateUpdate("/runner api"))
	if got := f.reopenState(t).Runner; got != "api" {
		t.Errorf("persisted Runner = %q, want api", got)
	}
	text = f.lastMessage(t)
	requireContains(t, text, "Runner")
	requireContains(t, text, "api")
	requireNotContains(t, text, "Runner: ")

	f.handle(privateUpdate("/runner"))
	requireContains(t, f.lastMessage(t), "api")

	f.handle(privateUpdate("/runner CLI"))
	if got := f.reopenState(t).Runner; got != "cli" {
		t.Errorf("persisted Runner = %q, want cli", got)
	}
	requireContains(t, f.lastMessage(t), "cli")
	if got := utf16Count(f.lastMessage(t)); got > 200 {
		t.Errorf("runner output = %d UTF-16 units, want compact", got)
	}
}

func TestRunnerRejectsUnknownValue(t *testing.T) {
	f := newBotFixture(t)
	f.setRunner(t, "api")

	f.handle(privateUpdate("/runner docker"))

	if got := f.reopenState(t).Runner; got != "api" {
		t.Errorf("persisted Runner = %q, want api", got)
	}
	requireContains(t, f.lastMessage(t), "Unknown runner")
	requireNotContains(t, f.lastMessage(t), "docker")

	f.handle(privateUpdate("/runner cli extra"))
	if got := f.reopenState(t).Runner; got != "api" {
		t.Errorf("persisted Runner after extra args = %q, want api", got)
	}
	requireContains(t, f.lastMessage(t), "Usage")
}

func TestRunnerChangeDoesNotAlterRunningJob(t *testing.T) {
	f := newBotFixture(t)

	f.handle(privateUpdate("/gist https://github.com/example/alpha"))
	requests := f.jobs.requestList()
	if len(requests) != 1 {
		t.Fatalf("enqueued requests = %d, want 1", len(requests))
	}
	if requests[0].Runner != jobs.RunnerCLI {
		t.Fatalf("first job Runner = %q, want cli", requests[0].Runner)
	}

	f.jobs.status = jobs.Status{Running: &jobs.JobView{
		ID:        requests[0].ID,
		RepoID:    "github__example__alpha",
		Model:     "flash",
		Runner:    "cli",
		StartedAt: fixedNow,
		Elapsed:   testJobTime,
	}}

	f.handle(privateUpdate("/runner api"))

	if got := f.jobs.Status().Running.Runner; got != "cli" {
		t.Errorf("running job Runner = %q, want cli", got)
	}
	requests = f.jobs.requestList()
	if requests[0].Runner != jobs.RunnerCLI {
		t.Errorf("first job Runner after change = %q, want cli", requests[0].Runner)
	}

	f.handle(privateUpdate("/gist https://github.com/example/beta"))
	requests = f.jobs.requestList()
	if len(requests) != 2 {
		t.Fatalf("enqueued requests = %d, want 2", len(requests))
	}
	if requests[0].Runner != jobs.RunnerCLI {
		t.Errorf("first job Runner = %q, want cli", requests[0].Runner)
	}
	if requests[1].Runner != jobs.RunnerAPI {
		t.Errorf("second job Runner = %q, want api", requests[1].Runner)
	}
}

func TestNextJobUsesNewRunnerAndModel(t *testing.T) {
	f := newBotFixture(t)

	f.handle(privateUpdate("/gist https://github.com/example/first"))
	f.handle(privateUpdate("/runner api"))
	f.handle(privateUpdate("/model pro"))
	f.handle(privateUpdate("/gist https://github.com/example/second"))

	requests := f.jobs.requestList()
	if len(requests) != 2 {
		t.Fatalf("enqueued requests = %d, want 2", len(requests))
	}

	first := requests[0]
	if first.Runner != jobs.RunnerCLI {
		t.Errorf("first Runner = %q, want cli", first.Runner)
	}
	if first.Model != f.cfg.Models["flash"] || first.ModelAlias != "flash" {
		t.Errorf("first model = %v/%q, want %v/flash", first.Model, first.ModelAlias, f.cfg.Models["flash"])
	}

	second := requests[1]
	if second.Runner != jobs.RunnerAPI {
		t.Errorf("second Runner = %q, want api", second.Runner)
	}
	if second.Model != f.cfg.Models["pro"] || second.ModelAlias != "pro" {
		t.Errorf("second model = %v/%q, want %v/pro", second.Model, second.ModelAlias, f.cfg.Models["pro"])
	}

	f.handle(privateUpdate("/model free test/pro#high"))
	f.handle(privateUpdate("/gist https://github.com/example/third"))

	requests = f.jobs.requestList()
	if len(requests) != 3 {
		t.Fatalf("enqueued requests = %d, want 3", len(requests))
	}
	third := requests[2]
	if third.Runner != jobs.RunnerAPI {
		t.Errorf("third Runner = %q, want api", third.Runner)
	}
	want := config.ModelRef{ProviderID: "test", ModelID: "pro", Variant: "high"}
	if third.Model != want || third.ModelAlias != "free" {
		t.Errorf("third model = %v/%q, want %v/free", third.Model, third.ModelAlias, want)
	}
}
