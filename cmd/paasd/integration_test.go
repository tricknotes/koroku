//go:build integration

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/logs"
	"github.com/tricknotes/koroku/internal/objstore"
	"github.com/tricknotes/koroku/internal/store"
)

func randomHex(t *testing.T, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buf)
}

// startObjectStorage runs a throwaway S3-compatible server and waits until
// it accepts requests.
func startObjectStorage(t *testing.T) (endpoint, accessKey, secretKey string) {
	t.Helper()
	image := os.Getenv("OBJSTORE_TEST_IMAGE")
	if image == "" {
		image = "cgr.dev/chainguard/minio:latest"
	}
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		if out, err := exec.Command("docker", "pull", "--quiet", image).CombinedOutput(); err != nil {
			t.Skipf("cannot pull %s: %v: %s", image, err, out)
		}
	}
	accessKey, secretKey = "it"+randomHex(t, 8), randomHex(t, 20)
	name := "paasd-serve-it-" + randomHex(t, 4)
	out, err := exec.Command("docker", "run", "--detach", "--rm", "--name", name,
		"--publish", "127.0.0.1::9000",
		"--env", "MINIO_ROOT_USER="+accessKey, "--env", "MINIO_ROOT_PASSWORD="+secretKey,
		image, "server", "/tmp/data", "--address", ":9000").CombinedOutput()
	if err != nil {
		t.Fatalf("start %s: %v: %s", image, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "--force", "--volumes", name).Run() })
	port, err := exec.Command("docker", "port", name, "9000/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	endpoint = "http://" + strings.TrimSpace(strings.Split(strings.TrimSpace(string(port)), "\n")[0])

	// Ready means: answers S3 requests with something other than 503.
	client, err := objstore.NewS3(objstore.S3Config{
		Endpoint: endpoint, Region: "us-east-1", Bucket: "readiness-probe", AccessKeyID: accessKey, SecretAccessKey: secretKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		_, err := client.List(context.Background(), "")
		var respErr *objstore.ResponseError
		if err == nil || (errors.As(err, &respErr) && respErr.StatusCode != http.StatusServiceUnavailable) {
			return endpoint, accessKey, secretKey
		}
		if time.Now().After(deadline) {
			t.Fatalf("object storage did not become ready: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

type serveEnv struct {
	t       *testing.T
	base    string
	dataDir string
	etcDir  string
}

// startServe writes a configuration and runs the real `paasd serve`.
func startServe(t *testing.T, endpoint, accessKey, secretKey, bucket string) *serveEnv {
	t.Helper()
	root := t.TempDir()
	env := &serveEnv{t: t, dataDir: filepath.Join(root, "var"), etcDir: filepath.Join(root, "etc")}
	if err := os.MkdirAll(env.etcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(env.etcDir, "restic.key"), []byte(randomHex(t, 16)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	port := 20000 + int(time.Now().UnixNano()%20000)
	configText := fmt.Sprintf(`base_domain = "dev.example.com"
listen = "127.0.0.1:0"
data_dir = %q
master_key_file = %q
[deploy]
allow_file_repos = true
prune_images = false
disk_usage_limit_percent = 100
port_range_start = %d
port_range_end = %d
[caddy]
enabled = false
[storage]
endpoint = %q
region = "us-east-1"
bucket = %q
access_key_id = %q
secret_access_key = %q
[backup]
restic_password_file = %q
`, env.dataDir, filepath.Join(env.etcDir, "master.key"), port, port+20,
		endpoint, bucket, accessKey, secretKey, filepath.Join(env.etcDir, "restic.key"))
	if err := os.WriteFile(filepath.Join(env.etcDir, "config.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	var logs bytes.Buffer
	go func() {
		done <- serve(ctx, []string{"--config", filepath.Join(env.etcDir, "config.toml"), "--insecure-no-auth"}, &logs, ready)
	}()
	select {
	case addr := <-ready:
		env.base = "http://" + addr
	case err := <-done:
		t.Fatalf("serve exited early: %v\n%s", err, logs.String())
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not become ready")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			t.Error("serve did not shut down")
		}
		if t.Failed() {
			t.Logf("paasd log:\n%s", logs.String())
		}
	})
	return env
}

func (e *serveEnv) call(method, path string, body, out any) int {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, e.base+path, reader)
	if err != nil {
		e.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(data, out); err != nil {
			e.t.Fatalf("%s %s: decode %q: %v", method, path, data, err)
		}
	}
	if resp.StatusCode >= 400 {
		e.t.Logf("%s %s -> %d %s", method, path, resp.StatusCode, data)
	}
	return resp.StatusCode
}

// TestServeBackupLogsAndSystemRestore runs the real `paasd serve` against
// real Docker, real restic and an S3-compatible server: deploy an app, back
// it up (dump hook + system), list and download the dump through the API,
// find its output in the collected logs, and bring the database and the
// configuration back on a "new host" with restore-system.
func TestServeBackupLogsAndSystemRestore(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic is not installed")
	}
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose is not available")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed (it creates the test bucket)")
	}
	endpoint, accessKey, secretKey := startObjectStorage(t)
	bucket := "paasd-it-" + randomHex(t, 4)
	// In production the bucket is created by hand before paasd is set up;
	// restic does not create it. curl signs the request (SigV4) for us.
	if out, err := exec.Command("curl", "--fail", "--silent", "--show-error", "--request", "PUT",
		"--aws-sigv4", "aws:amz:us-east-1:s3", "--user", accessKey+":"+secretKey,
		endpoint+"/"+bucket).CombinedOutput(); err != nil {
		t.Fatalf("create the bucket: %v: %s", err, out)
	}
	env := startServe(t, endpoint, accessKey, secretKey, bucket)

	// A repository built from the sample app.
	repo := t.TempDir()
	if err := os.CopyFS(repo, os.DirFS("../../testdata/apps/build-app")); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet", "--initial-branch", "main")
	git("config", "uploadpack.allowAnySHA1InWant", "true")
	git("add", "--all")
	git("commit", "--quiet", "-m", "Version 1")
	sha := git("rev-parse", "HEAD")

	slug := "it-serve-" + randomHex(t, 4)
	t.Cleanup(func() {
		_ = exec.Command("docker", "compose", "-p", "paas-"+slug, "down", "--volumes", "--remove-orphans", "--rmi", "local", "--timeout", "1").Run()
	})
	var app struct {
		HostPort int `json:"host_port"`
	}
	// The dump hook stands in for pg_dump: it prints the page the app serves.
	// The data volume is excluded because volume directories are only
	// readable by root, which the test is not.
	status := env.call(http.MethodPost, "/api/apps", map[string]any{
		"slug": slug, "repo_url": "file://" + repo, "web_port": 8080,
		"dump_hook_service": "web", "dump_hook_command": "cat /www/index.html",
		"backup_exclude_volumes": []string{"data"},
	}, &app)
	if status != http.StatusCreated {
		t.Fatalf("create app: %d", status)
	}
	var deployment struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if status := env.call(http.MethodPost, "/api/apps/"+slug+"/deployments", map[string]string{"sha": sha}, &deployment); status != http.StatusAccepted {
		t.Fatalf("deploy: %d", status)
	}
	waitFor(t, 5*time.Minute, "the deployment", func() bool {
		env.call(http.MethodGet, fmt.Sprintf("/api/deployments/%d", deployment.ID), nil, &deployment)
		return deployment.Status == "succeeded" || deployment.Status == "failed"
	})
	if deployment.Status != "succeeded" {
		t.Fatalf("deployment %s: %s", deployment.Status, deployment.Error)
	}

	// --- backups -------------------------------------------------------
	type backupStatus struct {
		Configured    bool       `json:"configured"`
		Running       bool       `json:"running"`
		Warning       bool       `json:"warning"`
		LastSuccessAt *time.Time `json:"last_success_at"`
		NextRunAt     *time.Time `json:"next_run_at"`
		Runs          []struct {
			Status, Trigger, Error string
		} `json:"runs"`
	}
	var before backupStatus
	env.call(http.MethodGet, "/api/backups", nil, &before)
	if !before.Configured || !before.Warning || before.NextRunAt == nil || !before.NextRunAt.After(time.Now()) {
		t.Errorf("status before the first backup = %+v", before)
	}
	if status := env.call(http.MethodPost, "/api/backups", nil, nil); status != http.StatusAccepted {
		t.Fatalf("start backup: %d", status)
	}
	var after backupStatus
	waitFor(t, 5*time.Minute, "the backup run", func() bool {
		env.call(http.MethodGet, "/api/backups", nil, &after)
		return len(after.Runs) > 0 && after.Runs[0].Status != "running"
	})
	if after.Runs[0].Status != "succeeded" || after.Runs[0].Trigger != "manual" || after.Warning || after.LastSuccessAt == nil {
		t.Fatalf("backup run = %+v (status %+v)", after.Runs[0], after)
	}

	var listing struct {
		Snapshots []struct {
			ID, Kind, Target string
			SizeBytes        *int64 `json:"size_bytes"`
		} `json:"snapshots"`
	}
	env.call(http.MethodGet, "/api/apps/"+slug+"/backups", nil, &listing)
	if len(listing.Snapshots) != 1 || listing.Snapshots[0].Kind != "dump" || listing.Snapshots[0].Target != "web" {
		t.Fatalf("snapshots = %+v, want exactly the dump (the volume is excluded)", listing.Snapshots)
	}
	if listing.Snapshots[0].SizeBytes == nil || *listing.Snapshots[0].SizeBytes <= 0 {
		t.Errorf("snapshot size = %v", listing.Snapshots[0].SizeBytes)
	}
	resp, err := http.Get(env.base + "/api/apps/" + slug + "/backups/" + listing.Snapshots[0].ID + "/download")
	if err != nil {
		t.Fatal(err)
	}
	dump, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(dump), "version=v1") {
		t.Errorf("dump download: %d %q", resp.StatusCode, dump)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	// A volume restore of a dump snapshot is refused.
	if status := env.call(http.MethodPost, "/api/apps/"+slug+"/backups/restore",
		map[string]string{"snapshot_id": listing.Snapshots[0].ID}, nil); status != http.StatusNotFound {
		t.Errorf("restoring a dump as a volume: status %d, want 404", status)
	}

	// --- logs ----------------------------------------------------------
	var history struct {
		Lines []struct{ Service, Line string } `json:"lines"`
	}
	waitFor(t, 60*time.Second, "the app's log line", func() bool {
		env.call(http.MethodGet, "/api/apps/"+slug+"/logs/history?q=booted", nil, &history)
		return len(history.Lines) > 0
	})
	if history.Lines[0].Service != "web" || history.Lines[0].Line != "booted version=v1" {
		t.Errorf("collected line = %+v", history.Lines[0])
	}
	var system struct {
		LogArchive struct {
			Configured bool `json:"configured"`
		} `json:"log_archive"`
	}
	env.call(http.MethodGet, "/api/system", nil, &system)
	if !system.LogArchive.Configured {
		t.Error("the log archiver is not reported as configured")
	}

	// --- log archive: object storage and presigned download -------------
	// The archiver runs on a four-hour grid, so the test archives the
	// current window itself, with the same directory, bucket and time zone
	// as the server, and then goes through the server's API.
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	collector, err := logs.New(logs.Options{Dir: filepath.Join(env.dataDir, "logs"), Location: tokyo})
	if err != nil {
		t.Fatal(err)
	}
	objects, err := objstore.NewS3(objstore.S3Config{
		Endpoint: endpoint, Region: "us-east-1", Bucket: bucket, AccessKeyID: accessKey, SecretAccessKey: secretKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	archiver, err := logs.NewArchiver(logs.ArchiverOptions{Collector: collector, Store: objects, IntervalHours: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := archiver.ArchiveOnce(context.Background(), archiver.NextWindowEnd(time.Now())); err != nil {
		t.Fatalf("archive the current window: %v", err)
	}
	var archives struct {
		Archives []struct {
			Key, Service string
			Size         int64
		} `json:"archives"`
	}
	env.call(http.MethodGet, "/api/apps/"+slug+"/logs/archives?service=web", nil, &archives)
	if len(archives.Archives) != 1 || !strings.HasPrefix(archives.Archives[0].Key, "logs/"+slug+"/web/") || archives.Archives[0].Size <= 0 {
		t.Fatalf("archives = %+v", archives.Archives)
	}
	var link struct {
		URL string `json:"url"`
	}
	env.call(http.MethodGet, "/api/apps/"+slug+"/logs/archives/url?key="+url.QueryEscape(archives.Archives[0].Key), nil, &link)
	archived, err := http.Get(link.URL)
	if err != nil {
		t.Fatalf("download through the presigned URL: %v", err)
	}
	gz, err := gzip.NewReader(archived.Body)
	if err != nil {
		t.Fatalf("the archive is not gzip (status %d): %v", archived.StatusCode, err)
	}
	content, _ := io.ReadAll(gz)
	_ = archived.Body.Close()
	if !strings.Contains(string(content), "booted version=v1") {
		t.Errorf("archived log content = %q", content)
	}
	// A key of the backup repository in the same bucket is not signed.
	if status := env.call(http.MethodGet, "/api/apps/"+slug+"/logs/archives/url?key="+url.QueryEscape("restic/config"), nil, nil); status != http.StatusUnprocessableEntity {
		t.Errorf("presigning a backup key: status %d, want 422", status)
	}

	// --- restore-system on a "new host" -------------------------------
	newHost := t.TempDir()
	newEtc, newVar := filepath.Join(newHost, "etc"), filepath.Join(newHost, "var")
	if err := os.MkdirAll(newEtc, 0o700); err != nil {
		t.Fatal(err)
	}
	// The operator brings only the storage settings and the restic password.
	password, _ := os.ReadFile(filepath.Join(env.etcDir, "restic.key"))
	if err := os.WriteFile(filepath.Join(newEtc, "restic.key"), password, 0o600); err != nil {
		t.Fatal(err)
	}
	minimal := fmt.Sprintf("base_domain = \"dev.example.com\"\ndata_dir = %q\n[caddy]\nenabled = false\n[storage]\nendpoint = %q\nregion = \"us-east-1\"\nbucket = %q\naccess_key_id = %q\nsecret_access_key = %q\n[backup]\nrestic_password_file = %q\n",
		newVar, endpoint, bucket, accessKey, secretKey, filepath.Join(newEtc, "restic.key"))
	newConfig := filepath.Join(newEtc, "config.toml")
	if err := os.WriteFile(newConfig, []byte(minimal), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"restore-system", "--config", newConfig}, &stdout, &stderr); err != nil {
		t.Fatalf("restore-system: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	// The master key came back, and the restored database knows the app.
	original, _ := os.ReadFile(filepath.Join(env.etcDir, "master.key"))
	restoredKey, err := os.ReadFile(filepath.Join(newEtc, "master.key"))
	if err != nil || !bytes.Equal(original, restoredKey) {
		t.Errorf("master key not restored: %v", err)
	}
	if kept, _ := os.ReadFile(newConfig); string(kept) != minimal {
		t.Error("the new host's config.toml was overwritten")
	}
	restored, err := store.Open(context.Background(), filepath.Join(newVar, "paas.db"))
	if err != nil {
		t.Fatalf("open the restored database: %v", err)
	}
	defer func() { _ = restored.Close() }()
	got, err := restored.GetApp(context.Background(), slug)
	if err != nil || got.DumpHookCommand != "cat /www/index.html" || got.CurrentDeploymentID == nil {
		t.Errorf("app in the restored database = %+v, %v", got, err)
	}
	t.Logf("restore-system output:\n%s", stdout.String())
}

func waitFor(t *testing.T, timeout time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
