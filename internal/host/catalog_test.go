package host

import (
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/adapter"
)

func TestListAppsUsesIconPathsWithoutChangingSources(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	if err := os.MkdirAll(cacheDir(), 0700); err != nil {
		t.Fatal(err)
	}
	discovered := []adapter.App{
		{ID: "editor/with space%", IconBase64: "base64-data", Icon: "ignored-theme"},
		{ID: "theme", Icon: "accessories-text-editor"},
		{ID: "file", Icon: "/opt/icons/app.png"},
		{ID: "none"},
	}
	data, err := json.Marshal(discovered)
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(cacheDir(), "apps.json")
	if err := os.WriteFile(cachePath, data, 0600); err != nil {
		t.Fatal(err)
	}
	c := Config{Apps: []adapter.App{{ID: "manual", IconBase64: "manual-base64"}}}
	apps, err := c.ListApps()
	if err != nil {
		t.Fatal(err)
	}
	want := append([]adapter.App{{ID: "manual", Icon: filepath.Join(cacheDir(), "rdp-launcher-go-manual.png")}}, discovered...)
	want[1].IconBase64 = ""
	want[1].Icon = filepath.Join(cacheDir(), "rdp-launcher-go-editor%2Fwith%20space%25.png")
	if !reflect.DeepEqual(apps, want) {
		t.Fatalf("apps = %+v, want %+v", apps, want)
	}
	output, err := json.Marshal(apps)
	if err != nil || strings.Contains(string(output), "icon_base64") {
		t.Fatalf("output = %s, err = %v", output, err)
	}
	if c.Apps[0].IconBase64 != "manual-base64" {
		t.Fatal("listing changed configured icon data")
	}
	cached, err := os.ReadFile(cachePath)
	if err != nil || string(cached) != string(data) {
		t.Fatal("listing changed cached icon data", err)
	}
	entries, err := os.ReadDir(cacheDir())
	if err != nil || len(entries) != 1 {
		t.Fatal("listing should not create icon files", entries, err)
	}
}

func TestDesktopKeyFileSerialization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "special.desktop")
	name := "Name\nwith\\backslash\tand\"quote"
	command := desktopQuote(`/opt/with spaces/$dollar`+"`backtick`"+`\\rdp-launcher`) + " %F"
	if err := writeDesktopEntry(path, map[string]string{"Name": name, "Exec": command}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "Name=Name\\nwith\\\\backslash\tand\"quote") {
		t.Fatal(string(data), err)
	}
	if output, err := exec.Command("desktop-file-validate", path).CombinedOutput(); err != nil {
		t.Fatal(string(output), err)
	}
}

func TestConfiguredPathsAndDesktopCatalog(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	mapped := filepath.Join(root, "shared files")
	path := filepath.Join(root, "config.toml")
	config := `[server]
host="192.0.2.10"
user="test-user"
[freerdp]
video_driver="wayland"
app_id="org.example.OtherRDP"
[lifecycle]
vm_name="test-vm"
[memory]
start_percent=95
stop_percent=80
[mounts]
"J:"=` + `"` + mapped + `"` + `
[[overrides]]
match="office"
mimes=["text/plain"]
categories="Office;Network;RemoteAccess;"
`
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.Adapter()
	if err != nil || a.Domain != "test-vm" || a.Policy.Memory == nil || a.Display.VideoDriver != "wayland" || a.FreeRDPAppID != "org.example.OtherRDP" {
		t.Fatal(a, err)
	}
	source := filepath.Join(mapped, "quarterly report.docx")
	uri := (&url.URL{Scheme: "file", Path: source}).String()
	windows, err := c.WindowsPath(uri)
	if err != nil || windows != `J:\quarterly report.docx` {
		t.Fatal(windows, err)
	}
	back, err := c.LinuxPath(windows)
	if err != nil || back != source {
		t.Fatal(back, err)
	}
	if _, err = c.WindowsPath(mapped + "-other/file"); err == nil {
		t.Fatal("sibling directory was treated as mapped")
	}
	apps := []adapter.App{{ID: "office/editor%quoted", Name: `Editor "Office"`, Target: `C:\Program Files\Office\editor.exe`, Arguments: "/new"}}
	dir := filepath.Join(root, "data", "applications")
	if err = os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(dir, "other-program.desktop")
	if err = os.WriteFile(unrelated, []byte("owned elsewhere"), 0644); err != nil {
		t.Fatal(err)
	}
	if err = c.SyncApps(apps, "/opt/with spaces/rdp-launcher", path, filepath.Join(root, "controller.sock")); err != nil {
		t.Fatal(err)
	}
	app := c.ResolveApp(apps[0].ID)
	if app.Target != apps[0].Target || app.Arguments != "/new" {
		t.Fatal(app)
	}
	entries, err := filepath.Glob(filepath.Join(dir, "rdp-launcher-go-*.desktop"))
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
	content, err := os.ReadFile(entries[0])
	if err != nil || (!strings.Contains(string(content), "%F") || !strings.Contains(string(content), "MimeType=text/plain;")) {
		t.Fatal(string(content), err)
	}
	if validator, err := exec.LookPath("desktop-file-validate"); err == nil {
		if output, err := exec.Command(validator, entries[0]).CombinedOutput(); err != nil {
			t.Fatal(string(output), err)
		}
	}
	if err = c.SyncApps(nil, "/opt/rdp-launcher", path, filepath.Join(root, "controller.sock")); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(entries[0]); !os.IsNotExist(err) {
		t.Fatal("stale owned desktop entry remains", err)
	}
	if _, err = os.Stat(unrelated); err != nil {
		t.Fatal("unrelated desktop entry was removed", err)
	}
}
