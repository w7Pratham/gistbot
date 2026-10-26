package jobs

import (
	"strings"
	"testing"
)

func TestBuildPromptIncludesTargetFocusAndSafety(t *testing.T) {
	got := BuildPrompt("auth flow")

	for _, want := range []string{
		"Target repo: ./repo",
		"auth flow",
		"repo-gist",
		"docs/gist",
		"untrusted data",
		"do not run code",
		"5-line summary",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("BuildPrompt() does not contain %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Target repo: /") {
		t.Errorf("BuildPrompt() target must stay relative to the job sandbox:\n%s", got)
	}
}

func TestBuildPromptOmitsEmptyFocus(t *testing.T) {
	got := BuildPrompt("   ")

	if !strings.Contains(got, "Focus: none.") {
		t.Errorf("BuildPrompt() empty focus = %q, want %q:\n%s", got, "Focus: none.", got)
	}
	if strings.Contains(got, "Focus: .") {
		t.Errorf("BuildPrompt() left an empty focus placeholder:\n%s", got)
	}
	if strings.Contains(got, "auth flow") {
		t.Errorf("BuildPrompt() invented a focus:\n%s", got)
	}
}

func TestBuildPromptCollapsesFocusWhitespace(t *testing.T) {
	got := BuildPrompt("  auth\n\tflow  ")

	if !strings.Contains(got, "Focus: auth flow.") {
		t.Errorf("BuildPrompt() did not normalize focus:\n%s", got)
	}
}
