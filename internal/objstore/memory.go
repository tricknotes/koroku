package objstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
)

// memoryURLBase is the origin of the fake URLs returned by Memory.PresignGet.
// The .invalid top-level domain never resolves.
const memoryURLBase = "https://objstore.invalid/"

// Memory is an in-memory Store for tests. The zero value is ready to use and
// it is safe for concurrent use. It applies the same key and expiry
// validation as S3.
type Memory struct {
	// Clock supplies LastModified. The default is clock.System{}. Set it
	// before the first call.
	Clock clock.Clock

	mu      sync.Mutex
	objects map[string]memoryObject
}

type memoryObject struct {
	body         []byte
	contentType  string
	lastModified time.Time
}

var _ Store = (*Memory)(nil)

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory { return &Memory{} }

// Put stores a copy of body under key.
func (m *Memory) Put(ctx context.Context, key string, body []byte, contentType string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("objstore: put %q: %w", key, err)
	}
	if contentType == "" {
		contentType = defaultContentType
	}
	now := m.now()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.objects == nil {
		m.objects = make(map[string]memoryObject)
	}
	m.objects[key] = memoryObject{body: bytes.Clone(body), contentType: contentType, lastModified: now}
	return nil
}

// Get returns the content stored under key.
func (m *Memory) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("objstore: get %q: %w", key, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	object, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("objstore: get %q: %w", key, ErrNotFound)
	}
	// Stored bodies are never modified in place, so sharing the slice is safe.
	return io.NopCloser(bytes.NewReader(object.body)), nil
}

// List returns the objects whose key starts with prefix, sorted by key.
func (m *Memory) List(ctx context.Context, prefix string) ([]Object, error) {
	if err := validatePrefix(prefix); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("objstore: list %q: %w", prefix, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var objects []Object
	for key, object := range m.objects {
		if strings.HasPrefix(key, prefix) {
			objects = append(objects, Object{Key: key, Size: int64(len(object.body)), LastModified: object.lastModified})
		}
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	return objects, nil
}

// Delete removes key. Deleting a missing object is not an error.
func (m *Memory) Delete(ctx context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("objstore: delete %q: %w", key, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

// PresignGet returns a deterministic fake URL of the form
// https://objstore.invalid/<key>?expires=<seconds>. It does not check that the
// object exists, matching S3.
func (m *Memory) PresignGet(key string, expires time.Duration) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	seconds, err := presignSeconds(expires)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%s?expires=%d", memoryURLBase, uriEncode(key, false), seconds), nil
}

// ContentType returns the content type stored with key, for assertions in tests.
func (m *Memory) ContentType(key string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	object, ok := m.objects[key]
	return object.contentType, ok
}

func (m *Memory) now() time.Time {
	if m.Clock != nil {
		return m.Clock.Now().UTC()
	}
	return clock.System{}.Now()
}
