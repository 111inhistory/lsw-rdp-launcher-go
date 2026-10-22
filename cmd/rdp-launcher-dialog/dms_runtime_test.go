package main

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// Keep Unix socket paths short, even when the subtest name is long.
func dmsRuntimeFixture(t *testing.T, parent, child, display string, listening bool) (string, func()) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("DMS runtime discovery uses Linux /proc")
	}
	dir, err := os.MkdirTemp("", "dms-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	base := filepath.Join(dir, "dms-"+parent)
	for suffix, value := range map[string]string{".session": display + "\n", ".pid": child + "\n"} {
		if err := os.WriteFile(base+suffix, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	closeSocket := func() {}
	if listening {
		listener, err := net.Listen("unix", base+".sock")
		if err != nil {
			t.Fatal(err)
		}
		closeSocket = func() { _ = listener.Close() }
		t.Cleanup(closeSocket)
	}
	return dir, closeSocket
}

func TestDMSRunningRequiresLiveCurrentDisplay(t *testing.T) {
	live := strconv.Itoa(os.Getpid())
	const dead = "2147483647"
	for _, tt := range []struct {
		name, parent, child, display string
		listening, want              bool
	}{
		{"active", live, live, "wayland-test", true, true},
		{"dead-parent-with-socket", dead, live, "wayland-test", true, false},
		{"dead-child-with-socket", live, dead, "wayland-test", true, false},
		{"different-display", live, live, "wayland-other", true, false},
		{"socket-unavailable", live, live, "wayland-test", false, false},
		{"invalid-parent", "not-a-pid", live, "wayland-test", true, false},
		{"invalid-child", live, "0", "wayland-test", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir, _ := dmsRuntimeFixture(t, tt.parent, tt.child, tt.display, tt.listening)
			if got := dmsRunning(dir, "wayland-test"); got != tt.want {
				t.Fatalf("dmsRunning=%t, want %t", got, tt.want)
			}
		})
	}
}

func TestDMSRuntimeFilesDoNotOutliveShell(t *testing.T) {
	pid := strconv.Itoa(os.Getpid())
	dir, closeSocket := dmsRuntimeFixture(t, pid, pid, "wayland-test", true)
	if !dmsRunning(dir, "wayland-test") {
		t.Fatal("live fixture was not detected")
	}
	closeSocket()
	if dmsRunning(dir, "wayland-test") {
		t.Fatal("session and pid files must not enable a stopped shell")
	}
	if err := os.Remove(filepath.Join(dir, "dms-"+pid+".pid")); err != nil {
		t.Fatal(err)
	}
	if dmsRunning(dir, "wayland-test") {
		t.Fatal("session file alone must not enable DMS")
	}
}

func TestDMSCacheRequiresRunningShell(t *testing.T) {
	root := t.TempDir()
	settings := filepath.Join(root, "DankMaterialShell")
	if err := os.MkdirAll(settings, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settings, "settings.json"), []byte(`{"cornerRadius":8}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settings, "dms-colors.json"), []byte(`{"mode":"dark","colors":{"dark":{"primary":"#123456"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if loadDMSStyle(root, root) == nil {
		t.Fatal("fixture must contain usable cached settings")
	}
	for _, environment := range [][2]string{{"", ""}, {"", "wayland-test"}, {t.TempDir(), ""}, {t.TempDir(), "wayland-test"}} {
		if loadActiveDMSStyle(root, root, environment[0], environment[1]) != nil {
			t.Fatal("cached settings without active DMS must preserve the system theme")
		}
	}
	pid := strconv.Itoa(os.Getpid())
	dir, closeSocket := dmsRuntimeFixture(t, pid, pid, "wayland-test", true)
	if style := loadActiveDMSStyle(root, root, dir, "wayland-test"); style == nil || style.radius != 8 {
		t.Fatal("active DMS settings were not loaded", style)
	}
	if loadActiveDMSStyle(root, root, dir, "wayland-other") != nil {
		t.Fatal("another display's DMS must not supply the theme")
	}
	closeSocket()
	if loadActiveDMSStyle(root, root, dir, "wayland-test") != nil {
		t.Fatal("stale settings and runtime files must not supply the theme")
	}
}
