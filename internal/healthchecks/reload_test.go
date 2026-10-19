package healthchecks

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadConfigFilePreservesCause(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.yml")
	if _, err := ReadConfigFile(missing); !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), missing) {
		t.Fatalf("missing configuration error lost its cause or path: %v", err)
	}
	// Opening a directory succeeds on Linux, but reading it as a file fails.
	_, err := ReadConfigFile(dir)
	var cause *os.PathError
	if !errors.As(err, &cause) || cause.Op != "read" || cause.Path != dir {
		t.Fatalf("configuration read error lost its cause or path: %v", err)
	}
}

func TestConfigReloadAtomicReplacementAndRecovery(t *testing.T) {
	e, _ := newTestEngine(t)
	e.Ping(testUUID, Input{Kind: "success", Body: "retained"})
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(path, []byte(simpleConfig), 0600); err != nil {
		t.Fatal(err)
	}
	w := NewConfigWatcher(path, e, os.LookupEnv)
	if _, err := w.Check(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("checks: broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Check(); err == nil {
		t.Fatal("accepted invalid reload")
	}
	if v := onlyCheck(t, e); v.Name != "Heartbeat" || v.Status != "up" || len(v.Events) != 1 {
		t.Fatal("invalid reload destroyed active state")
	}
	if e.Info().ReloadError == "" {
		t.Fatal("reload failure not visible")
	}
	replacement := filepath.Join(dir, "replacement.yml")
	if err := os.WriteFile(replacement, []byte(strings.Replace(simpleConfig, "Heartbeat", "Renamed", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if changed, err := w.Check(); err != nil || !changed {
		t.Fatalf("reload=%v %v", changed, err)
	}
	if v := onlyCheck(t, e); v.Name != "Renamed" || v.Status != "up" || len(v.Events) != 1 {
		t.Fatal("valid reload failed to retain history")
	}
	if e.Info().ReloadError != "" {
		t.Fatal("reload error not cleared")
	}
}

func TestConfigReloadFollowsConfigMapSymlink(t *testing.T) {
	e, _ := newTestEngine(t)
	dir := t.TempDir()
	for _, name := range []string{"old", "new"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "config.yml"), []byte(strings.Replace(simpleConfig, "Heartbeat", name, 1)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("old", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yml")
	if err := os.Symlink("..data/config.yml", path); err != nil {
		t.Fatal(err)
	}
	w := NewConfigWatcher(path, e, os.LookupEnv)
	if _, err := w.Check(); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("new", filepath.Join(dir, "..replacement")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "..replacement"), filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Check(); err != nil {
		t.Fatal(err)
	}
	if onlyCheck(t, e).Name != "new" {
		t.Fatal("did not follow atomically replaced symlink")
	}
}

func TestConfigReloadRevertToAppliedBytesClearsError(t *testing.T) {
	e, _ := newTestEngine(t)
	path := filepath.Join(t.TempDir(), "config.yml")
	write := func(config string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(simpleConfig)
	watcher := NewConfigWatcher(path, e, os.LookupEnv)
	if _, err := watcher.Check(); err != nil {
		t.Fatal(err)
	}
	write("checks: invalid")
	if _, err := watcher.Check(); err == nil {
		t.Fatal("invalid configuration accepted")
	}
	write(simpleConfig)
	if _, err := watcher.Check(); err != nil || e.Info().ReloadError != "" {
		t.Fatalf("reverting to applied YAML did not clear error: %v", err)
	}
}
