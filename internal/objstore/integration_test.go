//go:build integration

package objstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/runner"
)

// defaultIntegrationImage is a MinIO server build. Any image that takes
// "server <dir>" and reads MINIO_ROOT_USER / MINIO_ROOT_PASSWORD works; set
// OBJSTORE_TEST_IMAGE to use another one.
const defaultIntegrationImage = "cgr.dev/chainguard/minio:latest"

const integrationBucket = "objstore-it"

func randomHex(t *testing.T, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("read random bytes: %v", err)
	}
	return hex.EncodeToString(buf)
}

// startS3Server runs a throwaway S3-compatible server in Docker on a random
// loopback port and returns its endpoint and root credentials. The test is
// skipped when Docker or the image is not available.
func startS3Server(t *testing.T) (endpoint, accessKey, secretKey string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	run := runner.Exec{}

	if _, err := runner.Output(ctx, run, runner.Cmd{Name: "docker", Args: []string{"version", "--format", "{{.Server.Version}}"}}); err != nil {
		t.Skipf("Docker is not available: %v", err)
	}
	image := os.Getenv("OBJSTORE_TEST_IMAGE")
	if image == "" {
		image = defaultIntegrationImage
	}
	if _, err := runner.Output(ctx, run, runner.Cmd{Name: "docker", Args: []string{"image", "inspect", "--format", "{{.Id}}", image}}); err != nil {
		if _, err := runner.Output(ctx, run, runner.Cmd{Name: "docker", Args: []string{"pull", "--quiet", image}}); err != nil {
			t.Skipf("cannot pull %s: %v", image, err)
		}
	}

	accessKey = "it" + randomHex(t, 8)
	secretKey = randomHex(t, 20)
	name := "koroku-objstore-it-" + randomHex(t, 4)
	_, err := runner.Output(ctx, run, runner.Cmd{Name: "docker", Args: []string{
		"run", "--detach", "--rm", "--name", name,
		"--publish", "127.0.0.1::9000",
		"--env", "MINIO_ROOT_USER=" + accessKey,
		"--env", "MINIO_ROOT_PASSWORD=" + secretKey,
		image, "server", "/tmp/data", "--address", ":9000",
	}})
	if err != nil {
		t.Fatalf("start %s: %v", image, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := runner.Output(ctx, run, runner.Cmd{Name: "docker", Args: []string{"rm", "--force", "--volumes", name}}); err != nil {
			t.Logf("remove container %s: %v", name, err)
		}
	})

	out, err := runner.Output(ctx, run, runner.Cmd{Name: "docker", Args: []string{"port", name, "9000/tcp"}})
	if err != nil {
		t.Fatalf("look up the published port: %v", err)
	}
	address := strings.TrimSpace(strings.Split(strings.TrimSpace(string(out)), "\n")[0])
	if !strings.HasPrefix(address, "127.0.0.1:") {
		t.Fatalf("server is published on %q, want a loopback address", address)
	}
	endpoint = "http://" + address

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(60 * time.Second)
	for {
		resp, err := client.Get(endpoint + "/minio/health/live")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return endpoint, accessKey, secretKey
			}
		}
		if time.Now().After(deadline) {
			logs, _ := runner.Output(ctx, run, runner.Cmd{Name: "docker", Args: []string{"logs", "--tail", "20", name}})
			t.Fatalf("server did not become ready (last error: %v); logs:\n%s", err, logs)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestIntegrationS3(t *testing.T) {
	endpoint, accessKey, secretKey := startS3Server(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cfg := S3Config{
		Endpoint:        endpoint,
		Region:          "us-east-1",
		Bucket:          integrationBucket,
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
	}
	store, err := NewS3(cfg)
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}

	// The package has no bucket management, so create the bucket with a raw
	// signed request.
	// The liveness endpoint answers before the storage layer accepts
	// requests ("XMinioServerNotInitialized", HTTP 503), so retry for a while.
	ready := time.Now().Add(60 * time.Second)
	for {
		resp, err := store.do(ctx, http.MethodPut, store.bucketURL(), nil, nil)
		if err == nil {
			drainAndClose(resp.Body)
			break
		}
		var respErr *ResponseError
		if !errors.As(err, &respErr) || respErr.StatusCode != http.StatusServiceUnavailable || time.Now().After(ready) {
			t.Fatalf("create bucket: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	objects := map[string][]byte{
		"logs/blog/web/2026/01/02/00.log.gz": bytes.Repeat([]byte("gzip-ish payload\n"), 4096),
		"logs/blog/web/2026/01/02/04.log.gz": []byte("second"),
		"logs/blog/db/2026/01/02/04.log.gz":  {},
		"logs/shop/web/2026/01/02/04.log.gz": []byte("another app"),
		"odd/with space/日本語+1 (copy)$.log":   []byte("odd key"),
		"odd/percent 100%/a=b&c.log":         []byte("odd key 2"),
	}

	t.Run("list an empty bucket", func(t *testing.T) {
		got, err := store.List(ctx, "")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("got %+v, want nothing", got)
		}
	})

	t.Run("put and get", func(t *testing.T) {
		for key, body := range objects {
			if err := store.Put(ctx, key, body, "application/gzip"); err != nil {
				t.Fatalf("Put(%q): %v", key, err)
			}
		}
		for key, want := range objects {
			rc, err := store.Get(ctx, key)
			if err != nil {
				t.Fatalf("Get(%q): %v", key, err)
			}
			got, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				t.Fatalf("read %q: %v", key, err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("Get(%q) returned %d bytes, want %d", key, len(got), len(want))
			}
		}
	})

	t.Run("overwrite", func(t *testing.T) {
		const key = "logs/blog/web/2026/01/02/04.log.gz"
		objects[key] = []byte("second, replaced")
		if err := store.Put(ctx, key, objects[key], "application/gzip"); err != nil {
			t.Fatalf("Put: %v", err)
		}
		rc, err := store.Get(ctx, key)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		defer rc.Close()
		if got, _ := io.ReadAll(rc); !bytes.Equal(got, objects[key]) {
			t.Errorf("content = %q", got)
		}
	})

	t.Run("get a missing object", func(t *testing.T) {
		_, err := store.Get(ctx, "logs/blog/web/2026/01/02/23.log.gz")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	checkList := func(t *testing.T, s *S3, prefix string, want []string) {
		t.Helper()
		got, err := s.List(ctx, prefix)
		if err != nil {
			t.Fatalf("List(%q): %v", prefix, err)
		}
		var keys []string
		for _, object := range got {
			keys = append(keys, object.Key)
			if want := int64(len(objects[object.Key])); object.Size != want {
				t.Errorf("%q: Size = %d, want %d", object.Key, object.Size, want)
			}
			if age := time.Since(object.LastModified); age < -time.Minute || age > 10*time.Minute {
				t.Errorf("%q: LastModified = %s, which is not recent", object.Key, object.LastModified)
			}
		}
		if fmt.Sprint(keys) != fmt.Sprint(want) {
			t.Errorf("List(%q) =\n  %q\nwant\n  %q", prefix, keys, want)
		}
	}

	t.Run("list by prefix", func(t *testing.T) {
		checkList(t, store, "logs/blog/", []string{
			"logs/blog/db/2026/01/02/04.log.gz",
			"logs/blog/web/2026/01/02/00.log.gz",
			"logs/blog/web/2026/01/02/04.log.gz",
		})
		checkList(t, store, "logs/blog/web/2026/01/02/0", []string{
			"logs/blog/web/2026/01/02/00.log.gz",
			"logs/blog/web/2026/01/02/04.log.gz",
		})
		checkList(t, store, "odd/with space/", []string{"odd/with space/日本語+1 (copy)$.log"})
		checkList(t, store, "nothing/here/", nil)
	})

	t.Run("list follows real continuation tokens", func(t *testing.T) {
		paged, err := NewS3(cfg)
		if err != nil {
			t.Fatalf("NewS3: %v", err)
		}
		paged.listPageSize = 2
		checkList(t, paged, "", []string{
			"logs/blog/db/2026/01/02/04.log.gz",
			"logs/blog/web/2026/01/02/00.log.gz",
			"logs/blog/web/2026/01/02/04.log.gz",
			"logs/shop/web/2026/01/02/04.log.gz",
			"odd/percent 100%/a=b&c.log",
			"odd/with space/日本語+1 (copy)$.log",
		})
	})

	fetch := func(t *testing.T, rawURL string) (int, []byte) {
		t.Helper()
		resp, err := http.Get(rawURL)
		if err != nil {
			t.Fatalf("http.Get: %v", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		return resp.StatusCode, body
	}

	t.Run("presigned URL works with a plain http.Get", func(t *testing.T) {
		for _, key := range []string{"logs/blog/web/2026/01/02/00.log.gz", "odd/with space/日本語+1 (copy)$.log", "odd/percent 100%/a=b&c.log"} {
			presigned, err := store.PresignGet(key, 5*time.Minute)
			if err != nil {
				t.Fatalf("PresignGet(%q): %v", key, err)
			}
			status, body := fetch(t, presigned)
			if status != http.StatusOK || !bytes.Equal(body, objects[key]) {
				t.Errorf("%q: presigned fetch = HTTP %d with %d bytes, want 200 with %d bytes", key, status, len(body), len(objects[key]))
			}
		}
	})

	t.Run("presigned URL is bound to its key and signature", func(t *testing.T) {
		presigned, err := store.PresignGet("logs/blog/web/2026/01/02/04.log.gz", 5*time.Minute)
		if err != nil {
			t.Fatalf("PresignGet: %v", err)
		}
		other := strings.Replace(presigned, "/blog/", "/shop/", 1)
		if status, _ := fetch(t, other); status != http.StatusForbidden {
			t.Errorf("URL moved to another key = HTTP %d, want 403", status)
		}
		unsigned, _, _ := strings.Cut(presigned, "?")
		if status, _ := fetch(t, unsigned); status != http.StatusForbidden {
			t.Errorf("URL without the signature = HTTP %d, want 403", status)
		}
	})

	t.Run("expired presigned URL is rejected", func(t *testing.T) {
		pastCfg := cfg
		pastCfg.Clock = clock.NewFake(time.Now().UTC().Add(-time.Hour))
		past, err := NewS3(pastCfg)
		if err != nil {
			t.Fatalf("NewS3: %v", err)
		}
		presigned, err := past.PresignGet("logs/blog/web/2026/01/02/04.log.gz", time.Minute)
		if err != nil {
			t.Fatalf("PresignGet: %v", err)
		}
		if status, _ := fetch(t, presigned); status != http.StatusForbidden {
			t.Errorf("expired URL = HTTP %d, want 403", status)
		}
	})

	t.Run("wrong secret key is reported without leaking it", func(t *testing.T) {
		badCfg := cfg
		badCfg.SecretAccessKey = randomHex(t, 20)
		bad, err := NewS3(badCfg)
		if err != nil {
			t.Fatalf("NewS3: %v", err)
		}
		err = bad.Put(ctx, "denied", []byte("x"), "")
		var respErr *ResponseError
		if !errors.As(err, &respErr) {
			t.Fatalf("err = %v, want a *ResponseError", err)
		}
		if respErr.StatusCode != http.StatusForbidden || respErr.Code != "SignatureDoesNotMatch" || respErr.Message == "" {
			t.Errorf("got %+v, want 403 SignatureDoesNotMatch with a message", respErr)
		}
		if strings.Contains(err.Error(), badCfg.SecretAccessKey) || strings.Contains(err.Error(), secretKey) {
			t.Errorf("error leaks a secret key: %v", err)
		}
	})

	t.Run("missing bucket is not ErrNotFound", func(t *testing.T) {
		otherCfg := cfg
		otherCfg.Bucket = "objstore-it-missing"
		other, err := NewS3(otherCfg)
		if err != nil {
			t.Fatalf("NewS3: %v", err)
		}
		_, err = other.Get(ctx, "a")
		var respErr *ResponseError
		if !errors.As(err, &respErr) || respErr.Code != "NoSuchBucket" || errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want a NoSuchBucket *ResponseError", err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		for key := range objects {
			if err := store.Delete(ctx, key); err != nil {
				t.Fatalf("Delete(%q): %v", key, err)
			}
			if _, err := store.Get(ctx, key); !errors.Is(err, ErrNotFound) {
				t.Errorf("Get(%q) after Delete = %v, want ErrNotFound", key, err)
			}
		}
		if err := store.Delete(ctx, "logs/blog/web/2026/01/02/00.log.gz"); err != nil {
			t.Errorf("Delete of a missing object: %v", err)
		}
		got, err := store.List(ctx, "")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("bucket still holds %+v", got)
		}
	})
}
