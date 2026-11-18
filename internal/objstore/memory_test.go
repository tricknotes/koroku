package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
)

func readAll(t *testing.T, store Store, key string) string {
	t.Helper()
	rc, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %q: %v", key, err)
	}
	return string(data)
}

func TestMemoryRoundTrip(t *testing.T) {
	ctx := context.Background()
	fake := clock.NewFake(testTime)
	store := &Memory{Clock: fake}

	body := []byte("first")
	if err := store.Put(ctx, "logs/blog/web/2026/01/02/00.log.gz", body, "application/gzip"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	body[0] = 'X' // the store must keep its own copy
	if got := readAll(t, store, "logs/blog/web/2026/01/02/00.log.gz"); got != "first" {
		t.Errorf("content = %q", got)
	}
	if ct, ok := store.ContentType("logs/blog/web/2026/01/02/00.log.gz"); !ok || ct != "application/gzip" {
		t.Errorf("ContentType = %q, %v", ct, ok)
	}

	fake.Advance(4 * time.Hour)
	if err := store.Put(ctx, "logs/blog/web/2026/01/02/04.log.gz", []byte("second!"), ""); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ct, _ := store.ContentType("logs/blog/web/2026/01/02/04.log.gz"); ct != "application/octet-stream" {
		t.Errorf("default ContentType = %q", ct)
	}
	if err := store.Put(ctx, "logs/blog/db/2026/01/02/04.log.gz", nil, ""); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.Put(ctx, "logs/shop/web/2026/01/02/04.log.gz", []byte("other app"), ""); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := store.List(ctx, "logs/blog/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []Object{
		{Key: "logs/blog/db/2026/01/02/04.log.gz", Size: 0, LastModified: testTime.Add(4 * time.Hour)},
		{Key: "logs/blog/web/2026/01/02/00.log.gz", Size: 5, LastModified: testTime},
		{Key: "logs/blog/web/2026/01/02/04.log.gz", Size: 7, LastModified: testTime.Add(4 * time.Hour)},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("List =\n  %v\nwant\n  %v", got, want)
	}

	all, err := store.List(ctx, "")
	if err != nil || len(all) != 4 {
		t.Errorf("List(\"\") = %d objects, %v; want 4", len(all), err)
	}
	partial, err := store.List(ctx, "logs/blog/w")
	if err != nil || len(partial) != 2 {
		t.Errorf("List with a partial segment = %d objects, %v; want 2", len(partial), err)
	}

	if err := store.Delete(ctx, "logs/blog/web/2026/01/02/00.log.gz"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, "logs/blog/web/2026/01/02/00.log.gz"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, "logs/blog/web/2026/01/02/00.log.gz"); err != nil {
		t.Errorf("Delete of a missing key: %v", err)
	}
}

func TestMemoryZeroValue(t *testing.T) {
	ctx := context.Background()
	var store Memory
	if _, err := store.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get = %v, want ErrNotFound", err)
	}
	if objects, err := store.List(ctx, ""); err != nil || len(objects) != 0 {
		t.Errorf("List = %v, %v", objects, err)
	}
	if err := store.Delete(ctx, "missing"); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if err := store.Put(ctx, "a", []byte("x"), ""); err != nil {
		t.Fatalf("Put: %v", err)
	}
	objects, err := store.List(ctx, "")
	if err != nil || len(objects) != 1 || objects[0].LastModified.IsZero() {
		t.Errorf("List = %v, %v", objects, err)
	}
	if got := readAll(t, &store, "a"); got != "x" {
		t.Errorf("content = %q", got)
	}
}

func TestMemoryOverwrite(t *testing.T) {
	ctx := context.Background()
	store := NewMemory()
	for _, body := range []string{"one", "two"} {
		if err := store.Put(ctx, "k", []byte(body), ""); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if got := readAll(t, store, "k"); got != "two" {
		t.Errorf("content = %q", got)
	}
}

func TestMemoryPresignGet(t *testing.T) {
	store := NewMemory()
	got, err := store.PresignGet("logs/my app/日本語+1.log.gz", 15*time.Minute)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	want := "https://objstore.invalid/logs/my%20app/%E6%97%A5%E6%9C%AC%E8%AA%9E%2B1.log.gz?expires=900"
	if got != want {
		t.Errorf("PresignGet = %q, want %q", got, want)
	}
	if _, err := store.PresignGet("a", 0); !errors.Is(err, ErrInvalidExpiry) {
		t.Errorf("zero expiry = %v, want ErrInvalidExpiry", err)
	}
	if _, err := store.PresignGet("a", MaxPresignExpiry+time.Second); !errors.Is(err, ErrInvalidExpiry) {
		t.Errorf("long expiry = %v, want ErrInvalidExpiry", err)
	}
	if _, err := store.PresignGet("a", MaxPresignExpiry); err != nil {
		t.Errorf("maximum expiry: %v", err)
	}
}

func TestMemoryValidatesLikeS3(t *testing.T) {
	ctx := context.Background()
	store := NewMemory()
	for _, key := range []string{"", "/a", "a/../b", "a//b"} {
		if err := store.Put(ctx, key, nil, ""); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Put(%q) = %v, want ErrInvalidKey", key, err)
		}
		if _, err := store.Get(ctx, key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Get(%q) = %v, want ErrInvalidKey", key, err)
		}
		if err := store.Delete(ctx, key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("Delete(%q) = %v, want ErrInvalidKey", key, err)
		}
		if _, err := store.PresignGet(key, time.Minute); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("PresignGet(%q) = %v, want ErrInvalidKey", key, err)
		}
	}
	if _, err := store.List(ctx, "../"); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("List = %v, want ErrInvalidKey", err)
	}
}

func TestMemoryHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := NewMemory()
	if err := store.Put(ctx, "a", nil, ""); !errors.Is(err, context.Canceled) {
		t.Errorf("Put = %v", err)
	}
	if _, err := store.Get(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Errorf("Get = %v", err)
	}
	if _, err := store.List(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Errorf("List = %v", err)
	}
	if err := store.Delete(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Errorf("Delete = %v", err)
	}
}

func TestMemoryConcurrentUse(t *testing.T) {
	ctx := context.Background()
	store := NewMemory()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("k/%02d", i)
			for j := 0; j < 50; j++ {
				if err := store.Put(ctx, key, []byte("x"), ""); err != nil {
					t.Errorf("Put: %v", err)
				}
				if rc, err := store.Get(ctx, key); err != nil {
					t.Errorf("Get: %v", err)
				} else {
					rc.Close()
				}
				if _, err := store.List(ctx, "k/"); err != nil {
					t.Errorf("List: %v", err)
				}
				if _, err := store.PresignGet(key, time.Minute); err != nil {
					t.Errorf("PresignGet: %v", err)
				}
			}
		}(i)
	}
	wg.Wait()
	objects, err := store.List(ctx, "k/")
	if err != nil || len(objects) != 16 {
		t.Errorf("List = %d objects, %v; want 16", len(objects), err)
	}
}

func TestValidateKey(t *testing.T) {
	valid := []string{
		"a",
		"logs/blog/web/2026/01/02/03.log.gz",
		"with space/日本語+1.log",
		"a..b/.hidden/c...",
		"...",
	}
	for _, key := range valid {
		if err := ValidateKey(key); err != nil {
			t.Errorf("ValidateKey(%q) = %v, want nil", key, err)
		}
	}
	invalid := []string{
		"",
		"/",
		"/a",
		"..",
		".",
		"../a",
		"a/..",
		"a/../b",
		"a/./b",
		"a//b",
		"a/",
		"a\x00b",
		"a\tb",
		"a\x7fb",
		"bad\xffutf8",
		strings.Repeat("k", 1025),
	}
	for _, key := range invalid {
		if err := ValidateKey(key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("ValidateKey(%q) = %v, want ErrInvalidKey", key, err)
		}
	}
	if err := ValidateKey(strings.Repeat("k", 1024)); err != nil {
		t.Errorf("1024-byte key rejected: %v", err)
	}
}
