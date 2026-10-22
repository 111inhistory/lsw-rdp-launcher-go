package host

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/adapter"
)

func TestBuiltInDesktopShortcutRollsBackWithCatalog(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	c := Config{}
	if err := c.SyncApps(nil, "/old/launcher", "/config", "/socket"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "data", "applications", "rdp-launcher-go.desktop")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(cacheDir(), "apps.json")
	if err := os.Remove(cache); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cache, 0700); err != nil {
		t.Fatal(err)
	}
	if err := c.SyncApps(nil, "/new/launcher", "/config", "/socket"); err == nil {
		t.Fatal("cache publish failure ignored")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("desktop entry did not roll back with catalog", err)
	}
}

func TestBuiltInDesktopShortcutSurvivesSyncAndAppIDCollision(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	c := Config{}
	c.Filters.ExcludeApps = []string{"Windows 桌面"}
	apps := []adapter.App{{ID: "desktop", Name: "Other application", Target: `C:\other.exe`}}
	for _, discovered := range [][]adapter.App{apps, nil, nil} {
		if err := c.SyncApps(discovered, "/opt/with spaces/rdp-launcher", "/tmp/config with spaces.toml", "/tmp/controller.sock"); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, "data", "applications", "rdp-launcher-go.desktop")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if !strings.Contains(text, "Name=Windows 桌面") || !strings.Contains(text, " desktop\n") || strings.Contains(text, "open --app") || strings.Contains(text, "%F") {
			t.Fatal(text)
		}
		if output, err := exec.Command("desktop-file-validate", path).CombinedOutput(); err != nil {
			t.Fatal(string(output), err)
		}
		cached, err := ReadApps()
		if err != nil || len(cached) != len(discovered) {
			t.Fatal("built-in entry polluted raw discovery cache", err)
		}
		if len(discovered) > 0 {
			data, err := os.ReadFile(filepath.Join(root, "data", "applications", "rdp-launcher-go-desktop.desktop"))
			if err != nil || !strings.Contains(string(data), "open --app") {
				t.Fatal("built-in collided with remote app ID", err)
			}
		}
	}
}
