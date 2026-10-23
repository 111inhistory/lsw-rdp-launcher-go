package host

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/adapter"
)

func TestSyncAppsInvalidIconLeavesPreviousCatalogAndDesktopUnchanged(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	dataDir := filepath.Join(root, "data")
	t.Setenv("XDG_CACHE_HOME", cacheDir)
	t.Setenv("XDG_DATA_HOME", dataDir)

	validPNG := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	c := Config{}
	initialApps := []adapter.App{
		{
			ID:         "app1",
			Name:       "Application One",
			Target:     `C:\Tools\app1.exe`,
			IconBase64: validPNG,
		},
	}

	configPath := filepath.Join(root, "config.toml")
	socketPath := filepath.Join(root, "controller.sock")
	if err := c.SyncApps(initialApps, "/bin/launcher", configPath, socketPath); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	appsJSONPath := filepath.Join(cacheDir, "rdp-launcher-go", "apps.json")
	initialAppsJSON, err := os.ReadFile(appsJSONPath)
	if err != nil {
		t.Fatalf("failed to read initial apps.json: %v", err)
	}

	desktopPath := filepath.Join(dataDir, "applications", "rdp-launcher-go-app1.desktop")
	initialDesktop, err := os.ReadFile(desktopPath)
	if err != nil {
		t.Fatalf("failed to read initial desktop file: %v", err)
	}

	iconPath := filepath.Join(cacheDir, "rdp-launcher-go", "rdp-launcher-go-app1.png")
	initialIcon, err := os.ReadFile(iconPath)
	if err != nil {
		t.Fatalf("failed to read initial icon file: %v", err)
	}

	// Second sync with an invalid base64 icon on a second app, and modified first app
	secondApps := []adapter.App{
		{
			ID:         "app1",
			Name:       "Application One MODIFIED",
			Target:     `C:\Tools\app1.exe`,
			IconBase64: validPNG,
		},
		{
			ID:         "app2",
			Name:       "Application Two",
			Target:     `C:\Tools\app2.exe`,
			IconBase64: "this-is-not-valid-base64!@#$%",
		},
	}

	err = c.SyncApps(secondApps, "/bin/launcher", configPath, socketPath)
	if err == nil {
		t.Fatal("expected SyncApps to fail on invalid base64 icon, but got nil")
	}
	if !strings.Contains(err.Error(), "icon app2") {
		t.Fatalf("expected error mentioning icon app2, got: %v", err)
	}

	// Verify apps.json, desktop file, and icon file are UNCHANGED
	currentAppsJSON, err := os.ReadFile(appsJSONPath)
	if err != nil || string(currentAppsJSON) != string(initialAppsJSON) {
		t.Fatalf("apps.json was modified on icon failure! got %q, want %q", string(currentAppsJSON), string(initialAppsJSON))
	}

	currentDesktop, err := os.ReadFile(desktopPath)
	if err != nil || string(currentDesktop) != string(initialDesktop) {
		t.Fatalf("desktop entry was modified on icon failure! got %q, want %q", string(currentDesktop), string(initialDesktop))
	}

	currentIcon, err := os.ReadFile(iconPath)
	if err != nil || string(currentIcon) != string(initialIcon) {
		t.Fatalf("icon file was modified on icon failure!")
	}

	// App 2 desktop and icon must NOT exist
	app2Desktop := filepath.Join(dataDir, "applications", "rdp-launcher-go-app2.desktop")
	if _, err := os.Stat(app2Desktop); !os.IsNotExist(err) {
		t.Fatalf("app2 desktop file should not exist, but got err=%v", err)
	}
	app2Icon := filepath.Join(cacheDir, "rdp-launcher-go", "rdp-launcher-go-app2.png")
	if _, err := os.Stat(app2Icon); !os.IsNotExist(err) {
		t.Fatalf("app2 icon file should not exist, but got err=%v", err)
	}

	// Verify no staging or temporary files left in cacheDir or applications
	cacheEntries, _ := os.ReadDir(filepath.Join(cacheDir, "rdp-launcher-go"))
	for _, entry := range cacheEntries {
		if strings.HasPrefix(entry.Name(), ".") && entry.Name() != ".sync.lock" {
			t.Fatalf("leftover temporary file in cache: %s", entry.Name())
		}
	}
	dataEntries, _ := os.ReadDir(filepath.Join(dataDir, "applications"))
	for _, entry := range dataEntries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("leftover temporary file in applications: %s", entry.Name())
		}
	}
}

func TestSyncAppsSerializerFailureLeavesPreviousCatalogAndDesktopUnchanged(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	dataDir := filepath.Join(root, "data")
	t.Setenv("XDG_CACHE_HOME", cacheDir)
	t.Setenv("XDG_DATA_HOME", dataDir)

	validPNG := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	c := Config{}
	initialApps := []adapter.App{
		{
			ID:         "app1",
			Name:       "Application One",
			Target:     `C:\Tools\app1.exe`,
			IconBase64: validPNG,
		},
	}

	configPath := filepath.Join(root, "config.toml")
	socketPath := filepath.Join(root, "controller.sock")
	if err := c.SyncApps(initialApps, "/bin/launcher", configPath, socketPath); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	appsJSONPath := filepath.Join(cacheDir, "rdp-launcher-go", "apps.json")
	initialAppsJSON, _ := os.ReadFile(appsJSONPath)
	desktopPath := filepath.Join(dataDir, "applications", "rdp-launcher-go-app1.desktop")
	initialDesktop, _ := os.ReadFile(desktopPath)
	iconPath := filepath.Join(cacheDir, "rdp-launcher-go", "rdp-launcher-go-app1.png")
	initialIcon, _ := os.ReadFile(iconPath)

	// Make desktop-file-edit serializer fail by putting a failing mock in PATH
	fakeBin := filepath.Join(root, "fakebin")
	if err := os.MkdirAll(fakeBin, 0755); err != nil {
		t.Fatal(err)
	}
	failingScript := filepath.Join(fakeBin, "desktop-file-edit")
	if err := os.WriteFile(failingScript, []byte("#!/bin/sh\necho 'simulated serializer failure' >&2\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))

	secondApps := []adapter.App{
		{
			ID:         "app1",
			Name:       "Application One MODIFIED",
			Target:     `C:\Tools\app1.exe`,
			IconBase64: validPNG,
		},
		{
			ID:     "app2",
			Name:   "Application Two",
			Target: `C:\Tools\app2.exe`,
		},
	}

	err := c.SyncApps(secondApps, "/bin/launcher", configPath, socketPath)
	if err == nil {
		t.Fatal("expected SyncApps to fail on serializer failure, but got nil")
	}
	if !strings.Contains(err.Error(), "serialize desktop entry with desktop-file-edit") {
		t.Fatalf("expected error mentioning desktop-file-edit serializer, got: %v", err)
	}

	// Verify apps.json, desktop file, and icon file are UNCHANGED
	currentAppsJSON, _ := os.ReadFile(appsJSONPath)
	if string(currentAppsJSON) != string(initialAppsJSON) {
		t.Fatalf("apps.json was modified on serializer failure! got %q, want %q", string(currentAppsJSON), string(initialAppsJSON))
	}
	currentDesktop, _ := os.ReadFile(desktopPath)
	if string(currentDesktop) != string(initialDesktop) {
		t.Fatalf("desktop entry was modified on serializer failure! got %q, want %q", string(currentDesktop), string(initialDesktop))
	}
	currentIcon, _ := os.ReadFile(iconPath)
	if string(currentIcon) != string(initialIcon) {
		t.Fatalf("icon was modified on serializer failure!")
	}

	// Verify no app2 desktop was created
	app2Desktop := filepath.Join(dataDir, "applications", "rdp-launcher-go-app2.desktop")
	if _, err := os.Stat(app2Desktop); !os.IsNotExist(err) {
		t.Fatalf("app2 desktop file should not exist, got err=%v", err)
	}

	// Verify no staging files left
	cacheEntries, _ := os.ReadDir(filepath.Join(cacheDir, "rdp-launcher-go"))
	for _, entry := range cacheEntries {
		if strings.HasPrefix(entry.Name(), ".") && entry.Name() != ".sync.lock" {
			t.Fatalf("leftover temporary file in cache: %s", entry.Name())
		}
	}
	dataEntries, _ := os.ReadDir(filepath.Join(dataDir, "applications"))
	for _, entry := range dataEntries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("leftover temporary file in applications: %s", entry.Name())
		}
	}
}

func TestSyncAppsCleanupOrphanIconsAndStaleDesktopEntries(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	dataDir := filepath.Join(root, "data")
	t.Setenv("XDG_CACHE_HOME", cacheDir)
	t.Setenv("XDG_DATA_HOME", dataDir)

	validPNG := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	c := Config{}

	initialApps := []adapter.App{
		{ID: "app1", Name: "App One", Target: `C:\app1.exe`, IconBase64: validPNG},
		{ID: "app2", Name: "App Two", Target: `C:\app2.exe`, IconBase64: validPNG},
		{ID: "app3", Name: "App Three", Target: `C:\app3.exe`, IconBase64: validPNG},
	}

	configPath := filepath.Join(root, "config.toml")
	socketPath := filepath.Join(root, "controller.sock")
	if err := c.SyncApps(initialApps, "/bin/launcher", configPath, socketPath); err != nil {
		t.Fatalf("initial sync failed: %v", err)
	}

	appCache := filepath.Join(cacheDir, "rdp-launcher-go")
	appData := filepath.Join(dataDir, "applications")

	// Verify all 3 icons and desktop files exist
	for _, id := range []string{"app1", "app2", "app3"} {
		if _, err := os.Stat(filepath.Join(appCache, "rdp-launcher-go-"+id+".png")); err != nil {
			t.Fatalf("expected icon for %s, err=%v", id, err)
		}
		if _, err := os.Stat(filepath.Join(appData, "rdp-launcher-go-"+id+".desktop")); err != nil {
			t.Fatalf("expected desktop for %s, err=%v", id, err)
		}
	}

	// Plant unrelated files that should NEVER be deleted
	unrelatedIcon := filepath.Join(appCache, "other-app.png")
	if err := os.WriteFile(unrelatedIcon, []byte("unrelated icon"), 0644); err != nil {
		t.Fatal(err)
	}
	unrelatedData := filepath.Join(appCache, "rdp-launcher-go-something.txt")
	if err := os.WriteFile(unrelatedData, []byte("unrelated txt"), 0644); err != nil {
		t.Fatal(err)
	}
	unrelatedDesktop := filepath.Join(appData, "unrelated.desktop")
	if err := os.WriteFile(unrelatedDesktop, []byte("unrelated desktop"), 0644); err != nil {
		t.Fatal(err)
	}

	// Second sync:
	// - app1 transitions to theme icon (IconBase64 cleared, Icon set)
	// - app2 is REMOVED
	// - app3 remains with IconBase64
	secondApps := []adapter.App{
		{ID: "app1", Name: "App One", Target: `C:\app1.exe`, Icon: "utilities-terminal"},
		{ID: "app3", Name: "App Three", Target: `C:\app3.exe`, IconBase64: validPNG},
	}

	if err := c.SyncApps(secondApps, "/bin/launcher", configPath, socketPath); err != nil {
		t.Fatalf("second sync failed: %v", err)
	}

	// app1 png icon should be removed (icon->theme transition)
	app1Icon := filepath.Join(appCache, "rdp-launcher-go-app1.png")
	if _, err := os.Stat(app1Icon); !os.IsNotExist(err) {
		t.Fatalf("orphan icon for app1 was not cleaned up after icon->theme transition")
	}
	// app1 desktop should exist and have Icon=utilities-terminal
	app1Desktop, err := os.ReadFile(filepath.Join(appData, "rdp-launcher-go-app1.desktop"))
	if err != nil || !strings.Contains(string(app1Desktop), "Icon=utilities-terminal") {
		t.Fatalf("app1 desktop should have Icon=utilities-terminal, got: %s (err=%v)", string(app1Desktop), err)
	}

	// app2 png icon and desktop should be removed (app removed)
	app2Icon := filepath.Join(appCache, "rdp-launcher-go-app2.png")
	if _, err := os.Stat(app2Icon); !os.IsNotExist(err) {
		t.Fatalf("orphan icon for removed app2 was not cleaned up")
	}
	app2Desktop := filepath.Join(appData, "rdp-launcher-go-app2.desktop")
	if _, err := os.Stat(app2Desktop); !os.IsNotExist(err) {
		t.Fatalf("stale desktop for removed app2 was not cleaned up")
	}

	// app3 png icon and desktop should still exist
	app3Icon := filepath.Join(appCache, "rdp-launcher-go-app3.png")
	if _, err := os.Stat(app3Icon); err != nil {
		t.Fatalf("app3 icon should still exist, err=%v", err)
	}
	app3Desktop := filepath.Join(appData, "rdp-launcher-go-app3.desktop")
	if _, err := os.Stat(app3Desktop); err != nil {
		t.Fatalf("app3 desktop should still exist, err=%v", err)
	}

	// Unrelated files must NOT be deleted
	if _, err := os.Stat(unrelatedIcon); err != nil {
		t.Fatalf("unrelated icon was deleted! err=%v", err)
	}
	if _, err := os.Stat(unrelatedData); err != nil {
		t.Fatalf("unrelated data was deleted! err=%v", err)
	}
	if _, err := os.Stat(unrelatedDesktop); err != nil {
		t.Fatalf("unrelated desktop was deleted! err=%v", err)
	}
}

func TestSyncAppsFilteredAppCleansIconsAndDesktops(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	dataDir := filepath.Join(root, "data")
	t.Setenv("XDG_CACHE_HOME", cacheDir)
	t.Setenv("XDG_DATA_HOME", dataDir)

	validPNG := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	discovered := []adapter.App{
		{ID: "app1", Name: "App One", Target: `C:\app1.exe`, IconBase64: validPNG},
		{ID: "app2", Name: "App Two Filtered", Target: `C:\app2.exe`, IconBase64: validPNG},
	}

	configPath := filepath.Join(root, "config.toml")
	socketPath := filepath.Join(root, "controller.sock")

	// First sync with no filters
	plain := Config{}
	if err := plain.SyncApps(discovered, "/bin/launcher", configPath, socketPath); err != nil {
		t.Fatal(err)
	}

	appCache := filepath.Join(cacheDir, "rdp-launcher-go")
	appData := filepath.Join(dataDir, "applications")

	if _, err := os.Stat(filepath.Join(appCache, "rdp-launcher-go-app2.png")); err != nil {
		t.Fatalf("expected app2 icon to exist after initial sync")
	}
	if _, err := os.Stat(filepath.Join(appData, "rdp-launcher-go-app2.desktop")); err != nil {
		t.Fatalf("expected app2 desktop to exist after initial sync")
	}

	// Second sync with filter excluding app2
	c := Config{
		Filters: AppFilters{ExcludeApps: []string{"app2"}},
	}
	if err := c.SyncApps(discovered, "/bin/launcher", configPath, socketPath); err != nil {
		t.Fatal(err)
	}

	// Raw discovery cache in apps.json MUST still contain both app1 and app2
	raw, err := ReadApps()
	if err != nil || len(raw) != 2 {
		t.Fatalf("raw discovery cache was rewritten! got: %+v, err=%v", raw, err)
	}

	// app2 icon and desktop MUST be cleaned up
	if _, err := os.Stat(filepath.Join(appCache, "rdp-launcher-go-app2.png")); !os.IsNotExist(err) {
		t.Fatalf("filtered app2 icon was not cleaned up!")
	}
	if _, err := os.Stat(filepath.Join(appData, "rdp-launcher-go-app2.desktop")); !os.IsNotExist(err) {
		t.Fatalf("filtered app2 desktop was not cleaned up!")
	}

	// app1 icon and desktop MUST still exist
	if _, err := os.Stat(filepath.Join(appCache, "rdp-launcher-go-app1.png")); err != nil {
		t.Fatalf("app1 icon should still exist, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(appData, "rdp-launcher-go-app1.desktop")); err != nil {
		t.Fatalf("app1 desktop should still exist, err=%v", err)
	}
}

func TestSyncAppsBlocksConcurrentWriter(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	dataDir := filepath.Join(root, "data")
	t.Setenv("XDG_CACHE_HOME", cacheDir)
	t.Setenv("XDG_DATA_HOME", dataDir)

	c := Config{}
	configPath := filepath.Join(root, "config.toml")
	socketPath := filepath.Join(root, "controller.sock")

	cache := filepath.Join(cacheDir, "rdp-launcher-go")
	if err := os.MkdirAll(cache, 0700); err != nil {
		t.Fatal(err)
	}

	// Lock the persistent .sync.lock nonblocking
	lockFile, err := os.OpenFile(filepath.Join(cache, ".sync.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("failed to acquire initial flock: %v", err)
	}

	apps := []adapter.App{
		{ID: "app1", Name: "App One", Target: `C:\app1.exe`},
	}

	// Concurrent sync must be rejected because lock is held
	err = c.SyncApps(apps, "/bin/launcher", configPath, socketPath)
	if err == nil {
		t.Fatal("expected SyncApps to fail with sync already in progress, got nil")
	}
	if !strings.Contains(err.Error(), "sync already in progress") {
		t.Fatalf("expected error mentioning sync already in progress, got: %v", err)
	}

	// Release lock
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	_ = lockFile.Close()

	// Now SyncApps should succeed
	if err = c.SyncApps(apps, "/bin/launcher", configPath, socketPath); err != nil {
		t.Fatalf("SyncApps failed after lock release: %v", err)
	}
}

func TestSyncAppsPreservesExplicitIconReference(t *testing.T) {
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	dataDir := filepath.Join(root, "data")
	t.Setenv("XDG_CACHE_HOME", cacheDir)
	t.Setenv("XDG_DATA_HOME", dataDir)

	c := Config{}
	configPath := filepath.Join(root, "config.toml")
	socketPath := filepath.Join(root, "controller.sock")

	appCache := filepath.Join(cacheDir, "rdp-launcher-go")
	if err := os.MkdirAll(appCache, 0700); err != nil {
		t.Fatal(err)
	}

	validPNG := []byte("fake-png-bytes")
	// Plant an own-prefix PNG that is explicitly referenced by manual app.Icon
	customIconPath := filepath.Join(appCache, "rdp-launcher-go-custom-manual.png")
	if err := os.WriteFile(customIconPath, validPNG, 0600); err != nil {
		t.Fatal(err)
	}

	// Plant another own-prefix PNG that is referenced by relative path
	customRelPath := filepath.Join(appCache, "rdp-launcher-go-custom-relative.png")
	if err := os.WriteFile(customRelPath, validPNG, 0600); err != nil {
		t.Fatal(err)
	}

	// Plant an unreferenced own-prefix orphan PNG
	orphanIconPath := filepath.Join(appCache, "rdp-launcher-go-orphan.png")
	if err := os.WriteFile(orphanIconPath, validPNG, 0600); err != nil {
		t.Fatal(err)
	}

	apps := []adapter.App{
		{
			ID:     "app-abs",
			Name:   "App with Abs Icon",
			Target: `C:\app1.exe`,
			Icon:   customIconPath,
		},
		{
			ID:     "app-rel",
			Name:   "App with Rel Icon",
			Target: `C:\app2.exe`,
			Icon:   "rdp-launcher-go-custom-relative.png",
		},
	}

	if err := c.SyncApps(apps, "/bin/launcher", configPath, socketPath); err != nil {
		t.Fatalf("SyncApps failed: %v", err)
	}

	// Explicitly referenced icons MUST be preserved!
	if _, err := os.Stat(customIconPath); err != nil {
		t.Fatalf("explicitly referenced absolute icon was deleted by cleanup: %v", err)
	}
	if _, err := os.Stat(customRelPath); err != nil {
		t.Fatalf("explicitly referenced relative icon was deleted by cleanup: %v", err)
	}

	// Unreferenced orphan own-prefix icon MUST be deleted!
	if _, err := os.Stat(orphanIconPath); !os.IsNotExist(err) {
		t.Fatalf("orphan icon was not deleted by cleanup! err=%v", err)
	}
}
