// Package objstore stores objects in an S3-compatible bucket. koroku uses it to
// archive logs and to hand out presigned download URLs.
//
// Only the S3-compatible API is used, so the backend can be switched between
// providers by changing the endpoint, region and bucket.
package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxPresignExpiry is the longest lifetime a presigned URL may have. It is the
// limit imposed by Signature Version 4.
const MaxPresignExpiry = 7 * 24 * time.Hour

// maxKeyBytes is the longest object key S3 accepts.
const maxKeyBytes = 1024

var (
	// ErrNotFound is returned when the requested object does not exist.
	ErrNotFound = errors.New("objstore: object not found")
	// ErrInvalidKey is returned when an object key or prefix is rejected before
	// any request is made.
	ErrInvalidKey = errors.New("objstore: invalid key")
	// ErrInvalidExpiry is returned when a presigned URL lifetime is out of range.
	ErrInvalidExpiry = errors.New("objstore: invalid presign expiry")
)

// Object describes one stored object.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// Store is an object storage bucket.
type Store interface {
	// Put stores body under key, replacing any existing object. An empty
	// contentType is stored as application/octet-stream.
	Put(ctx context.Context, key string, body []byte, contentType string) error
	// Get returns the content of key. The caller must close the reader. It
	// returns an error matching ErrNotFound when the object does not exist.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// List returns every object whose key starts with prefix, sorted by key.
	// An empty prefix lists the whole bucket.
	List(ctx context.Context, prefix string) ([]Object, error)
	// Delete removes key. Deleting a missing object is not an error.
	Delete(ctx context.Context, key string) error
	// PresignGet returns a URL that lets anyone download key without
	// credentials until expires has passed. expires must be between one
	// second and MaxPresignExpiry.
	PresignGet(key string, expires time.Duration) (string, error)
}

// ValidateKey reports whether key is acceptable as an object key. Keys must be
// non-empty valid UTF-8 of at most 1024 bytes, must not start with "/", and
// must not contain empty, "." or ".." path segments or control characters.
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: empty key", ErrInvalidKey)
	}
	if err := validateKeyChars(key); err != nil {
		return err
	}
	for _, segment := range strings.Split(key, "/") {
		if err := validateSegment(key, segment); err != nil {
			return err
		}
	}
	return nil
}

// validatePrefix applies the key rules to a List prefix. A prefix may be empty,
// may end with "/" and may end in the middle of a segment.
func validatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if err := validateKeyChars(prefix); err != nil {
		return err
	}
	segments := strings.Split(prefix, "/")
	for i, segment := range segments {
		if i == len(segments)-1 && segment == "" {
			break // trailing slash
		}
		if err := validateSegment(prefix, segment); err != nil {
			return err
		}
	}
	return nil
}

func validateKeyChars(key string) error {
	if len(key) > maxKeyBytes {
		return fmt.Errorf("%w: longer than %d bytes", ErrInvalidKey, maxKeyBytes)
	}
	if !utf8.ValidString(key) {
		return fmt.Errorf("%w: not valid UTF-8", ErrInvalidKey)
	}
	if strings.HasPrefix(key, "/") {
		return fmt.Errorf("%w: %q starts with a slash", ErrInvalidKey, key)
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: %q contains a control character", ErrInvalidKey, key)
		}
	}
	return nil
}

func validateSegment(key, segment string) error {
	switch segment {
	case "":
		return fmt.Errorf("%w: %q contains an empty path segment", ErrInvalidKey, key)
	case ".", "..":
		return fmt.Errorf("%w: %q contains a %q path segment", ErrInvalidKey, key, segment)
	}
	return nil
}

// presignSeconds validates a presigned URL lifetime and converts it to whole
// seconds.
func presignSeconds(expires time.Duration) (int64, error) {
	if expires < time.Second {
		return 0, fmt.Errorf("%w: %s is shorter than one second", ErrInvalidExpiry, expires)
	}
	if expires > MaxPresignExpiry {
		return 0, fmt.Errorf("%w: %s is longer than %s", ErrInvalidExpiry, expires, MaxPresignExpiry)
	}
	return int64(expires / time.Second), nil
}
