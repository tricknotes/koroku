package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// MaxFileSize is the largest file ReadFile returns.
const MaxFileSize = 1 << 20

// contentEntry is the part of GitHub's contents object this package reads.
type contentEntry struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Size     int64  `json:"size"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

// contentsCall prepares a contents API request for path at ref (empty ref
// means the default branch).
func (c *Client) contentsCall(ctx context.Context, owner, repo, path, ref string) (call, error) {
	if err := validateRepo(owner, repo); err != nil {
		return call{}, err
	}
	escaped, err := escapeContentPath(path)
	if err != nil {
		return call{}, err
	}
	query := url.Values{}
	if ref != "" {
		if err := validateRef(ref); err != nil {
			return call{}, err
		}
		query.Set("ref", ref)
	}
	suffix := "/contents"
	if escaped != "" {
		suffix += "/" + escaped
	}
	return c.repoCall(ctx, owner, repo, suffix, query)
}

// ListDir returns the names of the regular files directly inside dir ("" is
// the repository root) at ref. Subdirectories, symlinks and submodules are
// left out. A missing directory, or a path that is a file, yields ErrNotFound.
func (c *Client) ListDir(ctx context.Context, owner, repo, dir, ref string) ([]string, error) {
	in, err := c.contentsCall(ctx, owner, repo, dir, ref)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if _, err := c.do(ctx, in, &raw); err != nil {
		return nil, fmt.Errorf("github: list %q in %s/%s: %w", dir, owner, repo, err)
	}
	// A directory is a JSON array; a single object means the path is a file.
	var entries []contentEntry
	if !isJSONArray(raw) || json.Unmarshal(raw, &entries) != nil {
		return nil, fmt.Errorf("github: list %q in %s/%s: %w: not a directory", dir, owner, repo, ErrNotFound)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Type == "file" {
			names = append(names, e.Name)
		}
	}
	return names, nil
}

// ReadFile returns the content of the file at path and ref. Files larger than
// MaxFileSize yield ErrTooLarge; a missing path, or one that is not a regular
// file, yields ErrNotFound.
func (c *Client) ReadFile(ctx context.Context, owner, repo, path, ref string) ([]byte, error) {
	if strings.Trim(path, "/") == "" {
		return nil, fmt.Errorf("%w: empty file path", ErrInvalidArgument)
	}
	in, err := c.contentsCall(ctx, owner, repo, path, ref)
	if err != nil {
		return nil, err
	}
	fail := func(err error) ([]byte, error) {
		return nil, fmt.Errorf("github: read %q in %s/%s: %w", path, owner, repo, err)
	}
	var raw json.RawMessage
	if _, err := c.do(ctx, in, &raw); err != nil {
		return fail(err)
	}
	var entry contentEntry
	if isJSONArray(raw) || json.Unmarshal(raw, &entry) != nil || entry.Type != "file" {
		return fail(fmt.Errorf("%w: not a regular file", ErrNotFound))
	}
	if entry.Size > MaxFileSize {
		return fail(fmt.Errorf("%w: %d bytes, limit is %d", ErrTooLarge, entry.Size, MaxFileSize))
	}
	if entry.Encoding != "base64" {
		return fail(fmt.Errorf("unsupported content encoding %.20q", entry.Encoding))
	}
	// GitHub wraps the base64 text every 60 characters.
	data, err := base64.StdEncoding.DecodeString(strings.NewReplacer("\n", "", "\r", "").Replace(entry.Content))
	if err != nil {
		return fail(fmt.Errorf("content is not valid base64"))
	}
	if len(data) > MaxFileSize {
		return fail(fmt.Errorf("%w: %d bytes, limit is %d", ErrTooLarge, len(data), MaxFileSize))
	}
	return data, nil
}

func isJSONArray(raw json.RawMessage) bool {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '['
}
