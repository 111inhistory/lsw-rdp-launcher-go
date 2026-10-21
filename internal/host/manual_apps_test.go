package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/adapter"
)

func TestManualCatalogWithoutDiscoveryAndAfterSync(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	path := filepath.Join(root, "config.toml")
	content := `[[apps]]
id = "explorer"
name = "文件资源管理器"
target = 'C:\Windows\explorer.exe'
arguments = '/separate'
working_dir = 'C:\Windows'
icon = "system-file-manager"
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"explorer", "文件资源管理器"} {
		app := c.ResolveApp(name)
		if app.Target != `C:\Windows\explorer.exe` || app.Arguments != "/separate" || app.WorkingDir != `C:\Windows` {
			t.Fatal(app)
		}
	}
	apps, err := c.ListApps()
	if err != nil || len(apps) != 1 {
		t.Fatal(apps, err)
	}
	discovered := []adapter.App{{ID: "explorer", Name: "Automatic entry", Target: "old.exe"}, {ID: "editor", Name: "Editor", Target: "editor.exe"}}
	if err = c.SyncApps(discovered, "/opt/rdp-launcher", path, filepath.Join(root, "controller.sock")); err != nil {
		t.Fatal(err)
	}
	apps, err = c.ListApps()
	if err != nil || len(apps) != 2 || apps[0].Target != c.Apps[0].Target {
		t.Fatal(apps, err)
	}
	if discovered[0].Target != "old.exe" {
		t.Fatal("merging changed the discovery input")
	}
	desktop := filepath.Join(root, "data", "applications", "rdp-launcher-go-explorer.desktop")
	data, err := os.ReadFile(desktop)
	if err != nil || !strings.Contains(string(data), "Icon=system-file-manager\n") || !strings.Contains(string(data), "Name=文件资源管理器\n") {
		t.Fatal(string(data), err)
	}
	// Removing configuration exposes the original discovered entry, not a cached
	// copy of the manual target. The next sync removes stale manual desktop files.
	c.Apps = nil
	if app := c.ResolveApp("explorer"); app.Target != "old.exe" {
		t.Fatal(app)
	}
	if err = c.SyncApps(nil, "/opt/rdp-launcher", path, filepath.Join(root, "controller.sock")); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(desktop); !os.IsNotExist(err) {
		t.Fatal("stale desktop entry remains", err)
	}
}

func TestManualConfigRejectsAmbiguousEntries(t *testing.T) {
	for _, content := range []string{
		"[[apps]]\nid='explorer'\nname='Explorer'\n",
		"[[apps]]\nid='explorer'\nname='Explorer'\ntarget='explorer.exe'\n[[apps]]\nid='explorer'\nname='Other'\ntarget='other.exe'\n",
	} {
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Fatal("accepted invalid manual application", content)
		}
	}
}
