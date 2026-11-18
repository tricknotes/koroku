package github

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var (
	// ownerPattern follows GitHub's login rules (alphanumerics and hyphens, at
	// most 39 characters) plus the underscore used by managed user accounts.
	ownerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,38}$`)
	// repoPattern follows GitHub's repository name rules.
	repoPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// maxRefLength bounds a branch, tag or SHA given by a caller.
const maxRefLength = 255

// SplitFullName splits "owner/repo" and validates both halves.
func SplitFullName(fullName string) (owner, repo string, err error) {
	owner, repo, ok := strings.Cut(fullName, "/")
	if !ok {
		return "", "", fmt.Errorf("%w: repository %.120q is not in owner/repo form", ErrInvalidArgument, fullName)
	}
	if err := validateRepo(owner, repo); err != nil {
		return "", "", err
	}
	return owner, repo, nil
}

// validateRepo rejects owner and repository names that GitHub would not
// accept, so they can never add path segments or query parameters to a URL.
func validateRepo(owner, repo string) error {
	if !ownerPattern.MatchString(owner) {
		return fmt.Errorf("%w: owner %.80q", ErrInvalidArgument, owner)
	}
	if !repoPattern.MatchString(repo) || repo == "." || repo == ".." {
		return fmt.Errorf("%w: repository name %.120q", ErrInvalidArgument, repo)
	}
	return nil
}

// repoPath returns the escaped "/repos/{owner}/{repo}" prefix.
func repoPath(owner, repo string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

// validateRef checks a branch name, tag or SHA. The rules are a subset of
// git's: enough to keep a ref from being read as a dot segment or carrying
// control characters. Everything else is neutralized by escaping.
func validateRef(ref string) error {
	if ref == "" {
		return fmt.Errorf("%w: empty ref", ErrInvalidArgument)
	}
	if len(ref) > maxRefLength {
		return fmt.Errorf("%w: ref longer than %d bytes", ErrInvalidArgument, maxRefLength)
	}
	if ref == "." || strings.Contains(ref, "..") || hasControlOrSpace(ref) {
		return fmt.Errorf("%w: ref %q", ErrInvalidArgument, ref)
	}
	return nil
}

// escapeContentPath validates a repository-relative path and percent-encodes
// each segment. The root ("", "." or "/") becomes the empty string.
func escapeContentPath(p string) (string, error) {
	p = strings.Trim(p, "/")
	if p == "" || p == "." {
		return "", nil
	}
	segments := strings.Split(p, "/")
	for i, s := range segments {
		if s == "" || s == "." || s == ".." || hasControl(s) {
			return "", fmt.Errorf("%w: path %.200q", ErrInvalidArgument, p)
		}
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/"), nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func hasControlOrSpace(s string) bool {
	return hasControl(s) || strings.Contains(s, " ")
}
