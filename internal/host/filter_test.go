package host

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/adapter"
)

func TestHostFiltersUseRemoteIdentityAndPaths(t *testing.T) {
	app := adapter.App{ID: "sdk-doc", Name: "SDK Documentation", Target: `C:\SDK\Shortcuts\docs.url`, ShortcutPath: `C:\ProgramData\Microsoft\Windows\Start Menu\Programs\SDK\Docs.lnk`}
	for _, tc := range []struct {
		name    string
		filters AppFilters
		want    bool
	}{
		{"id", AppFilters{ExcludeApps: []string{"SDK-DOC"}}, true},
		{"name", AppFilters{ExcludeApps: []string{"sdk documentation"}}, true},
		{"target", AppFilters{ExcludeApps: []string{"c:/sdk/shortcuts/docs.url"}}, true},
		{"shortcut", AppFilters{ExcludeApps: []string{app.ShortcutPath}}, true},
		{"not substring", AppFilters{ExcludeApps: []string{"Documentation"}}, false},
		{"source subtree", AppFilters{ExcludeDirs: []string{`c:/programdata/microsoft/windows/start menu/programs/SDK/`}}, true},
		{"target subtree", AppFilters{ExcludeDirs: []string{`c:\sdk\`}}, true},
		{"path normalization", AppFilters{ExcludeDirs: []string{`C:\SDK\unused\..\Shortcuts`}}, true},
		{"sibling boundary", AppFilters{ExcludeDirs: []string{`C:\SD`}}, false},
		{"other drive", AppFilters{ExcludeDirs: []string{`D:\SDK`}}, false},
		{"not Linux mount", AppFilters{ExcludeDirs: []string{`/mnt/windows/SDK`}}, false},
		{"empty rules", AppFilters{ExcludeApps: []string{""}, ExcludeDirs: []string{""}}, false},
		{"no filter", AppFilters{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.filters.excludes(app); got != tc.want {
				t.Fatal(got, tc.want)
			}
		})
	}
	unc := adapter.App{Target: `\\server\share\folder\app.exe`}
	if !(AppFilters{ExcludeDirs: []string{"//SERVER/share/folder/"}}).excludes(unc) {
		t.Fatal("UNC directory did not match")
	}
	if (AppFilters{ExcludeDirs: []string{`\\server\other`}}).excludes(unc) {
		t.Fatal("another UNC share matched")
	}
}

func TestFilteringCacheAndDesktopEntriesWithoutChangingDiscovery(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	config := filepath.Join(root, "config.toml")
	if err := os.WriteFile(config, []byte(`[filters]
exclude_apps = ['Sample Desktop Apps']
exclude_dirs = ['C:\SDK\Shortcuts']
`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	discovered := []adapter.App{
		{ID: "docs", Name: "Documentation", Target: `C:\SDK\Shortcuts\docs.url`},
		{ID: "sample", Name: "Sample Desktop Apps", Target: `C:\Samples\sample.exe`},
		{ID: "editor", Name: "Editor", Target: `C:\Tools\editor.exe`, ShortcutPath: `C:\Users\user\Desktop\Editor.lnk`},
	}
	before := append([]adapter.App(nil), discovered...)
	// First publish all entries, then ensure applying filters removes stale files.
	plain := Config{}
	if err = plain.SyncApps(discovered, "/opt/rdp-launcher", config, "/tmp/controller.sock"); err != nil {
		t.Fatal(err)
	}
	if err = c.SyncApps(discovered, "/opt/rdp-launcher", config, "/tmp/controller.sock"); err != nil {
		t.Fatal(err)
	}
	apps, err := c.ListApps()
	if err != nil || len(apps) != 1 || apps[0].ID != "editor" {
		t.Fatal(apps, err)
	}
	for _, id := range []string{"docs", "sample"} {
		path := filepath.Join(root, "data", "applications", "rdp-launcher-go-"+id+".desktop")
		if _, err = os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("filtered desktop entry remains", path, err)
		}
	}
	raw, err := ReadApps()
	if err != nil || !reflect.DeepEqual(raw, before) || !reflect.DeepEqual(discovered, before) {
		t.Fatal("filter changed raw discovery", raw, err)
	}
	// Host-only changes immediately affect the cached catalog, without an agent RPC.
	c.Filters = AppFilters{}
	apps, err = c.ListApps()
	if err != nil || len(apps) != 3 {
		t.Fatal(apps, err)
	}
	c.Filters = AppFilters{ExcludeApps: []string{"editor"}}
	c.Apps = []adapter.App{discovered[2]}
	if len(c.Catalog(nil)) != 0 {
		t.Fatal("manual entry bypassed an explicit filter")
	}
}
