package host

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestDesktopStageUsesSerializerRequiredSuffix(t *testing.T) {
	path, err := stageDesktopEntry(t.TempDir(), map[string]string{"Name": "Staged application", "Exec": "/bin/true"})
	if err != nil {
		t.Fatal("desktop-file-edit could not serialize stage", err)
	}
	if !strings.HasSuffix(path, ".desktop") {
		t.Fatal("stage lacks suffix required by desktop-file-edit", path)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "Name=Staged application") {
		t.Fatal(string(data), err)
	}
}

func TestSyncCleansAbandonedStagesOnlyWhileHoldingLock(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	dir := filepath.Join(root, "data", "applications")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, ".rdp-launcher-go-stage-crashed.desktop")
	current := filepath.Join(dir, ".rdp-launcher-go-stage-another-crash.desktop")
	foreign := filepath.Join(dir, ".other-stage-preserved.desktop")
	for _, path := range []string{legacy, current, foreign} {
		if err := os.WriteFile(path, []byte("fixture"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(cacheDir(), 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(cacheDir(), ".sync.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	c := Config{}
	if err := c.SyncApps(nil, "/bin/launcher", "", ""); err == nil {
		t.Fatal("concurrent writer was accepted")
	}
	for _, path := range []string{legacy, current} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("stage belonging to active writer was removed", err)
		}
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := c.SyncApps(nil, "/bin/launcher", "", ""); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{legacy, current} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("abandoned stage was not removed", path, err)
		}
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatal("cleanup removed unrelated file", err)
	}
}
