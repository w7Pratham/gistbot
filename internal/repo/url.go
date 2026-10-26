// Package repo validates GitHub repository targets, derives canonical
// repository identifiers, and discovers gist documentation inside installed
// repositories.
package repo

import (
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	githubScheme = "https"
	githubHost   = "github.com"

	// idPrefix and idSeparator form the canonical repository ID
	// github__<owner>__<repo>. IDs are validated as a single path component
	// before they are joined to WORK_DIR.
	idPrefix    = "github__"
	idSeparator = "__"
)

var (
	urlSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	idPattern         = regexp.MustCompile(`^github__[a-z0-9._-]+__[a-z0-9._-]+$`)
)

// Target is a validated GitHub repository reference. Owner and Name keep the
// spelling supplied in the URL, URL is the canonical HTTPS clone URL, and ID is
// the lowercase canonical identifier.
type Target struct {
	URL   string
	Host  string
	Owner string
	Name  string
	ID    string
}

// ParseGitHubURL accepts exactly https://github.com/<owner>/<repo> with an
// optional trailing slash or .git suffix. It rejects every other host, scheme,
// port, userinfo, query, fragment, extra path segment, percent-encoded path,
// dot segment, and segment starting with a hyphen. Returned errors never
// include the raw input.
func ParseGitHubURL(raw string) (Target, error) {
	if raw == "" {
		return Target{}, errors.New("repo: URL is empty")
	}
	if strings.ContainsAny(raw, " \t\r\n\v\f") {
		return Target{}, errors.New("repo: URL contains whitespace")
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return Target{}, errors.New("repo: URL is not a valid URL")
	}

	if parsed.Scheme != githubScheme {
		return Target{}, errors.New("repo: URL scheme must be https")
	}
	if parsed.Opaque != "" {
		return Target{}, errors.New("repo: URL must be hierarchical")
	}
	if parsed.User != nil {
		return Target{}, errors.New("repo: URL must not contain userinfo")
	}
	if parsed.Host != githubHost {
		return Target{}, errors.New("repo: URL host must be github.com")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return Target{}, errors.New("repo: URL must not contain a query")
	}
	if parsed.Fragment != "" || parsed.RawFragment != "" || strings.Contains(raw, "#") {
		return Target{}, errors.New("repo: URL must not contain a fragment")
	}
	if strings.Contains(raw, "%") {
		return Target{}, errors.New("repo: URL must not contain percent-encoded characters")
	}
	if !strings.HasPrefix(parsed.Path, "/") {
		return Target{}, errors.New("repo: URL path must be absolute")
	}

	// Trim the leading slash, at most one trailing slash, and at most one .git
	// suffix. Anything left must be exactly two non-empty segments.
	path := strings.TrimPrefix(parsed.Path, "/")
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		return Target{}, errors.New("repo: URL must name exactly one repository")
	}

	owner, name := parts[0], parts[1]
	if !validURLSegment(owner) || !validURLSegment(name) {
		return Target{}, errors.New("repo: URL has an invalid owner or repository name")
	}

	return Target{
		URL:   githubScheme + "://" + githubHost + "/" + owner + "/" + name,
		Host:  githubHost,
		Owner: owner,
		Name:  name,
		ID:    idPrefix + strings.ToLower(owner) + idSeparator + strings.ToLower(name),
	}, nil
}

// ValidID reports whether id is a canonical repository ID that is safe to use
// as a single path component under WORK_DIR. It rejects empty values,
// traversal segments, absolute paths, uppercase characters, and any embedded
// path separator.
func ValidID(id string) bool {
	if !idPattern.MatchString(id) {
		return false
	}

	owner, name, ok := splitID(id)
	if !ok {
		return false
	}
	if owner == "." || owner == ".." || name == "." || name == ".." {
		return false
	}
	return true
}

// RepoDir returns the installed directory for id under workDir. It returns an
// empty string when workDir is not an absolute path or id is not canonical, so
// callers can never construct a path from unvalidated input.
func RepoDir(workDir, id string) string {
	if workDir == "" || !filepath.IsAbs(workDir) || !ValidID(id) {
		return ""
	}
	return filepath.Join(filepath.Clean(workDir), id)
}

func validURLSegment(segment string) bool {
	if segment == "" || segment == "." || segment == ".." || strings.HasPrefix(segment, "-") {
		return false
	}
	return urlSegmentPattern.MatchString(segment)
}

// splitID splits a canonical ID into its owner and repository name. The first
// separator after the prefix is used, which matches GitHub owners because they
// cannot contain underscores.
func splitID(id string) (owner, name string, ok bool) {
	rest, ok := strings.CutPrefix(id, idPrefix)
	if !ok {
		return "", "", false
	}
	owner, name, ok = strings.Cut(rest, idSeparator)
	if !ok || owner == "" || name == "" {
		return "", "", false
	}
	return owner, name, true
}
