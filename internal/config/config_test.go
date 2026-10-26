package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// baseEnv returns a complete, valid environment. Optional variables are
// intentionally absent so that every test exercises the documented defaults.
func baseEnv() map[string]string {
	return map[string]string{
		"TELEGRAM_BOT_TOKEN":    "123:secret",
		"OWNER_USER_ID":         "42",
		"WORK_DIR":              "/home/gistbot/gists",
		"OPENCODE_BIN":          "/home/gistbot/.opencode/bin/opencode",
		"OPENCODE_API_PASSWORD": "api-secret",
		"MODEL_FREE":            "opencode/deepseek-v4.1-flash-free",
		"MODEL_FLASH":           "deepseek/deepseek-flash#repo-scout",
		"MODEL_PRO":             "deepseek/deepseek-v4-pro",
		"STATE_FILE":            "/home/gistbot/.local/state/gistbot/state.json",
		"LOG_DIR":               "/home/gistbot/.local/state/gistbot/logs",
	}
}

func getenv(env map[string]string) func(string) string {
	return func(key string) string {
		return env[key]
	}
}

func TestLoadValid(t *testing.T) {
	cfg, err := Load(getenv(baseEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.TelegramToken != "123:secret" {
		t.Errorf("TelegramToken = %q", cfg.TelegramToken)
	}
	if cfg.TelegramAPI != "https://api.telegram.org" {
		t.Errorf("TelegramAPI = %q", cfg.TelegramAPI)
	}
	if cfg.OwnerUserID != 42 {
		t.Errorf("OwnerUserID = %d", cfg.OwnerUserID)
	}
	if cfg.WorkDir != "/home/gistbot/gists" {
		t.Errorf("WorkDir = %q", cfg.WorkDir)
	}
	if cfg.OpenCodeBin != "/home/gistbot/.opencode/bin/opencode" {
		t.Errorf("OpenCodeBin = %q", cfg.OpenCodeBin)
	}
	if cfg.OpenCodeAPI != "http://127.0.0.1:4096" {
		t.Errorf("OpenCodeAPI = %q", cfg.OpenCodeAPI)
	}
	if cfg.OpenCodeAPIPassword != "api-secret" {
		t.Errorf("OpenCodeAPIPassword did not match")
	}
	if cfg.OpenCodeUnit != "opencode-gist.service" {
		t.Errorf("OpenCodeUnit = %q", cfg.OpenCodeUnit)
	}
	if got, want := cfg.Models["flash"].String(), "deepseek/deepseek-flash#repo-scout"; got != want {
		t.Errorf("Models[flash] = %q, want %q", got, want)
	}
	if got, want := cfg.Models["free"].String(), "opencode/deepseek-v4.1-flash-free"; got != want {
		t.Errorf("Models[free] = %q, want %q", got, want)
	}
	if got, want := cfg.Models["pro"].String(), "deepseek/deepseek-v4-pro"; got != want {
		t.Errorf("Models[pro] = %q, want %q", got, want)
	}
	if len(cfg.Models) != 3 {
		t.Errorf("len(Models) = %d, want 3", len(cfg.Models))
	}
	if cfg.DefaultModel != "flash" {
		t.Errorf("DefaultModel = %q", cfg.DefaultModel)
	}
	if cfg.DefaultRunner != "cli" {
		t.Errorf("DefaultRunner = %q", cfg.DefaultRunner)
	}
	if cfg.JobTimeout != 20*time.Minute {
		t.Errorf("JobTimeout = %v", cfg.JobTimeout)
	}
	if cfg.CloneTimeout != 5*time.Minute {
		t.Errorf("CloneTimeout = %v", cfg.CloneTimeout)
	}
	if cfg.MaxQueue != 3 {
		t.Errorf("MaxQueue = %d", cfg.MaxQueue)
	}
	if !reflect.DeepEqual(cfg.AllowedHosts, []string{"github.com"}) {
		t.Errorf("AllowedHosts = %#v", cfg.AllowedHosts)
	}
	if cfg.StateFile != "/home/gistbot/.local/state/gistbot/state.json" {
		t.Errorf("StateFile = %q", cfg.StateFile)
	}
	if cfg.LogDir != "/home/gistbot/.local/state/gistbot/logs" {
		t.Errorf("LogDir = %q", cfg.LogDir)
	}
}

func TestLoadTrimsSecrets(t *testing.T) {
	env := baseEnv()
	env["TELEGRAM_BOT_TOKEN"] = "\n\t 123:secret \t\r\n"
	env["OPENCODE_API_PASSWORD"] = "  api-secret\n"

	cfg, err := Load(getenv(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TelegramToken != "123:secret" {
		t.Errorf("TelegramToken = %q, want trimmed value", cfg.TelegramToken)
	}
	if cfg.OpenCodeAPIPassword != "api-secret" {
		t.Errorf("OpenCodeAPIPassword is not trimmed")
	}
}

func TestLoadSecretErrorsDoNotLeakValues(t *testing.T) {
	env := baseEnv()
	env["TELEGRAM_BOT_TOKEN"] = "\n 123:secret \t"
	env["OPENCODE_API_PASSWORD"] = "  api-secret\n"
	// Force a failure in an unrelated variable so the error path is exercised
	// with padded secrets still present in the environment.
	env["OWNER_USER_ID"] = "not-a-number"

	_, err := Load(getenv(env))
	if err == nil {
		t.Fatal("Load succeeded, want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "OWNER_USER_ID") {
		t.Errorf("error %q does not name OWNER_USER_ID", msg)
	}
	for _, secret := range []string{"123:secret", "api-secret"} {
		if strings.Contains(msg, secret) {
			t.Errorf("error leaks secret %q: %q", secret, msg)
		}
	}
}

func TestLoadUsesOnlySuppliedGetter(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "from-process-environment")
	t.Setenv("OWNER_USER_ID", "999")

	cfg, err := Load(getenv(baseEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TelegramToken != "123:secret" {
		t.Errorf("TelegramToken = %q, want value from supplied getter", cfg.TelegramToken)
	}
	if cfg.OwnerUserID != 42 {
		t.Errorf("OwnerUserID = %d, want value from supplied getter", cfg.OwnerUserID)
	}
}

func TestLoadOverrides(t *testing.T) {
	env := baseEnv()
	env["TELEGRAM_API_URL"] = "http://127.0.0.1:8081"
	env["OPENCODE_API_URL"] = "http://127.0.0.1:5555"
	env["OPENCODE_API_UNIT"] = "custom-opencode.service"
	env["DEFAULT_MODEL"] = "pro"
	env["DEFAULT_RUNNER"] = "api"
	env["JOB_TIMEOUT"] = "90s"
	env["CLONE_TIMEOUT"] = "30s"
	env["MAX_QUEUE"] = "10"
	env["ALLOWED_HOSTS"] = "github.com"

	cfg, err := Load(getenv(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TelegramAPI != "http://127.0.0.1:8081" {
		t.Errorf("TelegramAPI = %q", cfg.TelegramAPI)
	}
	if cfg.OpenCodeAPI != "http://127.0.0.1:5555" {
		t.Errorf("OpenCodeAPI = %q", cfg.OpenCodeAPI)
	}
	if cfg.OpenCodeUnit != "custom-opencode.service" {
		t.Errorf("OpenCodeUnit = %q", cfg.OpenCodeUnit)
	}
	if cfg.DefaultModel != "pro" {
		t.Errorf("DefaultModel = %q", cfg.DefaultModel)
	}
	if cfg.DefaultRunner != "api" {
		t.Errorf("DefaultRunner = %q", cfg.DefaultRunner)
	}
	if cfg.JobTimeout != 90*time.Second {
		t.Errorf("JobTimeout = %v", cfg.JobTimeout)
	}
	if cfg.CloneTimeout != 30*time.Second {
		t.Errorf("CloneTimeout = %v", cfg.CloneTimeout)
	}
	if cfg.MaxQueue != 10 {
		t.Errorf("MaxQueue = %d", cfg.MaxQueue)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(map[string]string)
		wantVar string
	}{
		{"missing token", func(env map[string]string) { delete(env, "TELEGRAM_BOT_TOKEN") }, "TELEGRAM_BOT_TOKEN"},
		{"blank token", func(env map[string]string) { env["TELEGRAM_BOT_TOKEN"] = "   " }, "TELEGRAM_BOT_TOKEN"},
		{"bad telegram url", func(env map[string]string) { env["TELEGRAM_API_URL"] = "not a url" }, "TELEGRAM_API_URL"},
		{"non-http telegram url", func(env map[string]string) { env["TELEGRAM_API_URL"] = "ftp://example.com" }, "TELEGRAM_API_URL"},
		{"missing owner", func(env map[string]string) { delete(env, "OWNER_USER_ID") }, "OWNER_USER_ID"},
		{"owner zero", func(env map[string]string) { env["OWNER_USER_ID"] = "0" }, "OWNER_USER_ID"},
		{"owner negative", func(env map[string]string) { env["OWNER_USER_ID"] = "-1" }, "OWNER_USER_ID"},
		{"owner not a number", func(env map[string]string) { env["OWNER_USER_ID"] = "abc" }, "OWNER_USER_ID"},
		{"missing work dir", func(env map[string]string) { delete(env, "WORK_DIR") }, "WORK_DIR"},
		{"relative work dir", func(env map[string]string) { env["WORK_DIR"] = "gists" }, "WORK_DIR"},
		{"work dir root", func(env map[string]string) { env["WORK_DIR"] = "/" }, "WORK_DIR"},
		{"relative opencode bin", func(env map[string]string) { env["OPENCODE_BIN"] = "opencode" }, "OPENCODE_BIN"},
		{"missing api password", func(env map[string]string) { delete(env, "OPENCODE_API_PASSWORD") }, "OPENCODE_API_PASSWORD"},
		{"blank api password", func(env map[string]string) { env["OPENCODE_API_PASSWORD"] = "  " }, "OPENCODE_API_PASSWORD"},
		{"bad opencode url", func(env map[string]string) { env["OPENCODE_API_URL"] = "127.0.0.1:4096" }, "OPENCODE_API_URL"},
		{"missing model free", func(env map[string]string) { delete(env, "MODEL_FREE") }, "MODEL_FREE"},
		{"invalid model flash", func(env map[string]string) { env["MODEL_FLASH"] = "flash" }, "MODEL_FLASH"},
		{"missing model pro", func(env map[string]string) { delete(env, "MODEL_PRO") }, "MODEL_PRO"},
		{"unknown default model", func(env map[string]string) { env["DEFAULT_MODEL"] = "turbo" }, "DEFAULT_MODEL"},
		{"unknown runner", func(env map[string]string) { env["DEFAULT_RUNNER"] = "shell" }, "DEFAULT_RUNNER"},
		{"zero job timeout", func(env map[string]string) { env["JOB_TIMEOUT"] = "0s" }, "JOB_TIMEOUT"},
		{"bad job timeout", func(env map[string]string) { env["JOB_TIMEOUT"] = "soon" }, "JOB_TIMEOUT"},
		{"zero clone timeout", func(env map[string]string) { env["CLONE_TIMEOUT"] = "0s" }, "CLONE_TIMEOUT"},
		{"max queue zero", func(env map[string]string) { env["MAX_QUEUE"] = "0" }, "MAX_QUEUE"},
		{"max queue too high", func(env map[string]string) { env["MAX_QUEUE"] = "11" }, "MAX_QUEUE"},
		{"bad max queue", func(env map[string]string) { env["MAX_QUEUE"] = "three" }, "MAX_QUEUE"},
		{"host not allowed", func(env map[string]string) { env["ALLOWED_HOSTS"] = "gitlab.com" }, "ALLOWED_HOSTS"},
		{"multiple hosts not allowed", func(env map[string]string) { env["ALLOWED_HOSTS"] = "github.com,gitlab.com" }, "ALLOWED_HOSTS"},
		{"relative state file", func(env map[string]string) { env["STATE_FILE"] = "state.json" }, "STATE_FILE"},
		{"relative log dir", func(env map[string]string) { env["LOG_DIR"] = "logs" }, "LOG_DIR"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := baseEnv()
			tc.mutate(env)
			_, err := Load(getenv(env))
			if err == nil {
				t.Fatal("Load succeeded, want error")
			}
			if !strings.Contains(err.Error(), tc.wantVar) {
				t.Errorf("error %q does not name %s", err, tc.wantVar)
			}
			if strings.Contains(err.Error(), "123:secret") {
				t.Errorf("error leaks TELEGRAM_BOT_TOKEN: %q", err)
			}
			if strings.Contains(err.Error(), "api-secret") {
				t.Errorf("error leaks OPENCODE_API_PASSWORD: %q", err)
			}
		})
	}
}

func TestParseModelRef(t *testing.T) {
	valid := []struct {
		raw  string
		want ModelRef
	}{
		{"deepseek/deepseek-flash", ModelRef{ProviderID: "deepseek", ModelID: "deepseek-flash"}},
		{"deepseek/deepseek-flash#repo-scout", ModelRef{ProviderID: "deepseek", ModelID: "deepseek-flash", Variant: "repo-scout"}},
		{"opencode/deepseek-v4.1-flash-free", ModelRef{ProviderID: "opencode", ModelID: "deepseek-v4.1-flash-free"}},
		{"  deepseek/deepseek-v4-pro  ", ModelRef{ProviderID: "deepseek", ModelID: "deepseek-v4-pro"}},
	}
	for _, tc := range valid {
		ref, err := ParseModelRef(tc.raw)
		if err != nil {
			t.Errorf("ParseModelRef(%q): %v", tc.raw, err)
			continue
		}
		if ref != tc.want {
			t.Errorf("ParseModelRef(%q) = %#v, want %#v", tc.raw, ref, tc.want)
		}
		if got := ref.String(); got != strings.TrimSpace(tc.raw) {
			t.Errorf("ModelRef.String() = %q, want %q", got, strings.TrimSpace(tc.raw))
		}
	}

	invalid := []string{
		"",
		"   ",
		"deepseek",
		"/deepseek-flash",
		"deepseek/",
		"deepseek/deepseek-flash#",
		"deepseek/deepseek-flash#repo#scout",
		"deepseek//deepseek-flash",
		"deepseek/deep seek-flash",
		"deepseek/deepseek-flash#repo scout",
		"deepseek/deepseek-flash#repo/scout",
	}
	for _, raw := range invalid {
		if ref, err := ParseModelRef(raw); err == nil {
			t.Errorf("ParseModelRef(%q) = %#v, want error", raw, ref)
		}
	}
}

func TestResolveModel(t *testing.T) {
	cfg, err := Load(getenv(baseEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	ref, err := cfg.ResolveModel("flash", "")
	if err != nil {
		t.Fatalf("ResolveModel(flash): %v", err)
	}
	if got, want := ref.String(), "deepseek/deepseek-flash#repo-scout"; got != want {
		t.Errorf("ResolveModel(flash) = %q, want %q", got, want)
	}

	ref, err = cfg.ResolveModel("flash", "opencode/custom-free")
	if err != nil {
		t.Fatalf("ResolveModel(flash, override): %v", err)
	}
	if got, want := ref.String(), "deepseek/deepseek-flash#repo-scout"; got != want {
		t.Errorf("ResolveModel(flash, override) = %q, want %q", got, want)
	}

	ref, err = cfg.ResolveModel("free", "")
	if err != nil {
		t.Fatalf("ResolveModel(free): %v", err)
	}
	if got, want := ref.String(), "opencode/deepseek-v4.1-flash-free"; got != want {
		t.Errorf("ResolveModel(free) = %q, want %q", got, want)
	}

	ref, err = cfg.ResolveModel("free", "opencode/custom-free#fast")
	if err != nil {
		t.Fatalf("ResolveModel(free, override): %v", err)
	}
	if got, want := ref.String(), "opencode/custom-free#fast"; got != want {
		t.Errorf("ResolveModel(free, override) = %q, want %q", got, want)
	}

	if _, err := cfg.ResolveModel("turbo", ""); err == nil {
		t.Error("ResolveModel(turbo) succeeded, want error")
	}
	if _, err := cfg.ResolveModel("free", "not-a-model-ref"); err == nil {
		t.Error("ResolveModel(free, invalid override) succeeded, want error")
	}
}
