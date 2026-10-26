package repo

import (
	"path/filepath"
	"testing"
)

func TestParseGitHubURLAccepts(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		url   string
		owner string
		repo  string
		id    string
	}{
		{
			name:  "plain",
			raw:   "https://github.com/Owner/Repo",
			url:   "https://github.com/Owner/Repo",
			owner: "Owner",
			repo:  "Repo",
			id:    "github__owner__repo",
		},
		{
			name:  "git suffix",
			raw:   "https://github.com/Owner/Repo.git",
			url:   "https://github.com/Owner/Repo",
			owner: "Owner",
			repo:  "Repo",
			id:    "github__owner__repo",
		},
		{
			name:  "trailing slash",
			raw:   "https://github.com/Owner/Repo/",
			url:   "https://github.com/Owner/Repo",
			owner: "Owner",
			repo:  "Repo",
			id:    "github__owner__repo",
		},
		{
			name:  "git suffix and trailing slash",
			raw:   "https://github.com/owner/repo.git/",
			url:   "https://github.com/owner/repo",
			owner: "owner",
			repo:  "repo",
			id:    "github__owner__repo",
		},
		{
			name:  "allowed punctuation",
			raw:   "https://github.com/o-w_n.e/r.p-o",
			url:   "https://github.com/o-w_n.e/r.p-o",
			owner: "o-w_n.e",
			repo:  "r.p-o",
			id:    "github__o-w_n.e__r.p-o",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseGitHubURL(tc.raw)
			if err != nil {
				t.Fatalf("ParseGitHubURL(%q) returned error", tc.raw)
			}
			if got.URL != tc.url {
				t.Errorf("URL = %q, want %q", got.URL, tc.url)
			}
			if got.Host != "github.com" {
				t.Errorf("Host = %q, want github.com", got.Host)
			}
			if got.Owner != tc.owner {
				t.Errorf("Owner = %q, want %q", got.Owner, tc.owner)
			}
			if got.Name != tc.repo {
				t.Errorf("Name = %q, want %q", got.Name, tc.repo)
			}
			if got.ID != tc.id {
				t.Errorf("ID = %q, want %q", got.ID, tc.id)
			}
		})
	}
}

func TestParseGitHubURLRejects(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "http scheme", raw: "http://github.com/owner/repo"},
		{name: "git scheme", raw: "git://github.com/owner/repo"},
		{name: "ssh scheme", raw: "ssh://github.com/owner/repo"},
		{name: "scp style", raw: "git@github.com:owner/repo.git"},
		{name: "other host", raw: "https://gitlab.com/owner/repo"},
		{name: "host suffix", raw: "https://github.com.evil.test/owner/repo"},
		{name: "host trailing dot", raw: "https://github.com./owner/repo"},
		{name: "uppercase host", raw: "https://GitHub.com/owner/repo"},
		{name: "missing repo", raw: "https://github.com/owner"},
		{name: "extra path", raw: "https://github.com/owner/repo/extra"},
		{name: "double slash path", raw: "https://github.com//owner/repo"},
		{name: "double trailing slash", raw: "https://github.com/owner/repo//"},
		{name: "query", raw: "https://github.com/owner/repo?ref=main"},
		{name: "empty query", raw: "https://github.com/owner/repo?"},
		{name: "fragment", raw: "https://github.com/owner/repo#readme"},
		{name: "empty fragment", raw: "https://github.com/owner/repo#"},
		{name: "userinfo", raw: "https://user@github.com/owner/repo"},
		{name: "userinfo with password", raw: "https://user:pass@github.com/owner/repo"},
		{name: "port", raw: "https://github.com:443/owner/repo"},
		{name: "percent encoded segments", raw: "https://github.com/%6fwner/repo"},
		{name: "percent encoded dots", raw: "https://github.com/owner/%2e%2e"},
		{name: "percent encoded slash", raw: "https://github.com/owner/repo%2Fextra"},
		{name: "dotdot owner", raw: "https://github.com/../repo"},
		{name: "dot owner", raw: "https://github.com/./repo"},
		{name: "dotdot repo", raw: "https://github.com/owner/.."},
		{name: "dot repo", raw: "https://github.com/owner/."},
		{name: "leading hyphen owner", raw: "https://github.com/-owner/repo"},
		{name: "leading hyphen repo", raw: "https://github.com/owner/-repo"},
		{name: "space in repo", raw: "https://github.com/owner/re po"},
		{name: "backslash in repo", raw: `https://github.com/owner/re\po`},
		{name: "trailing space", raw: "https://github.com/owner/repo "},
		{name: "non ASCII repo", raw: "https://github.com/owner/répo"},
		{name: "empty owner", raw: "https://github.com//repo"},
		{name: "git suffix only", raw: "https://github.com/owner/.git"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := ParseGitHubURL(tc.raw); err == nil {
				t.Fatalf("ParseGitHubURL(%q) = %+v, want error", tc.raw, got)
			}
		})
	}
}

func TestValidID(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want bool
	}{
		{name: "canonical", id: "github__owner__repo", want: true},
		{name: "punctuation", id: "github__o-w_n.e__r.p-o", want: true},
		{name: "underscores in repo", id: "github__owner__my__repo", want: true},
		{name: "empty", id: "", want: false},
		{name: "traversal", id: "../x", want: false},
		{name: "absolute", id: "/home/shiv/repo", want: false},
		{name: "url", id: "https://github.com/owner/repo", want: false},
		{name: "missing separator", id: "github__owner", want: false},
		{name: "empty repo", id: "github__owner__", want: false},
		{name: "empty owner", id: "github____repo", want: false},
		{name: "uppercase prefix", id: "GITHUB__owner__repo", want: false},
		{name: "uppercase segment", id: "github__Owner__repo", want: false},
		{name: "dotdot owner", id: "github__..__repo", want: false},
		{name: "dot owner", id: "github__.__repo", want: false},
		{name: "dotdot repo", id: "github__owner__..", want: false},
		{name: "dot repo", id: "github__owner__.", want: false},
		{name: "slash in repo", id: "github__owner__re/po", want: false},
		{name: "backslash in repo", id: `github__owner__re\po`, want: false},
		{name: "space in repo", id: "github__owner__re po", want: false},
		{name: "hidden root", id: ".github__owner__repo", want: false},
		{name: "trailing newline", id: "github__owner__repo\n", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidID(tc.id); got != tc.want {
				t.Errorf("ValidID(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

func TestRepoDir(t *testing.T) {
	workDir := t.TempDir()
	id := "github__owner__repo"

	if got, want := RepoDir(workDir, id), filepath.Join(workDir, id); got != want {
		t.Errorf("RepoDir(%q, %q) = %q, want %q", workDir, id, got, want)
	}

	invalid := []string{
		"",
		"../x",
		"/home/shiv/repo",
		"github__owner__repo/extra",
		"github__owner__repo/../other",
		"GITHUB__owner__repo",
	}
	for _, bad := range invalid {
		if got := RepoDir(workDir, bad); got != "" {
			t.Errorf("RepoDir(%q, %q) = %q, want empty", workDir, bad, got)
		}
	}

	if got := RepoDir("relative/path", id); got != "" {
		t.Errorf("RepoDir with a relative work dir = %q, want empty", got)
	}
}
