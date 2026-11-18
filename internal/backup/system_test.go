package backup

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestCopyTreeSkipsTheDataDirectory: the local development setup keeps the
// data directory, and with it the staging directory of the system backup,
// inside the configuration directory. The copy must leave both out instead
// of copying its own output again and again.
func TestCopyTreeSkipsTheDataDirectory(t *testing.T) {
	config := t.TempDir()
	data := filepath.Join(config, "var")
	stage := filepath.Join(data, "backup-work", "system")
	for path, content := range map[string]string{
		filepath.Join(config, "config.toml"):        "base_domain = \"dev.localhost\"\n",
		filepath.Join(config, "keys", "master.key"): "key",
		filepath.Join(data, "koroku.db"):            "database",
		filepath.Join(data, "apps", "blog", "env"):  "SECRET=1\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(stage, 0o700); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(stage, SystemConfigDir)
	if err := copyTree(config, dst, []string{data + "/"}); err != nil {
		t.Fatalf("copyTree: %v", err)
	}

	var got []string
	err := filepath.WalkDir(dst, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dst, path)
			got = append(got, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{"config.toml", filepath.Join("keys", "master.key")}; !slices.Equal(got, want) {
		t.Errorf("copied %v, want %v", got, want)
	}
}

// TestCopyTreeNeverCopiesItsOwnOutput holds even when nothing is skipped
// explicitly: the destination below the source is left out by itself.
func TestCopyTreeNeverCopiesItsOwnOutput(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "config.toml"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(src, "work", "config")
	if err := copyTree(src, dst, nil); err != nil {
		t.Fatalf("copyTree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "work", "config")); !os.IsNotExist(err) {
		t.Errorf("the copy contains itself (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "config.toml")); err != nil {
		t.Errorf("config.toml was not copied: %v", err)
	}
}
