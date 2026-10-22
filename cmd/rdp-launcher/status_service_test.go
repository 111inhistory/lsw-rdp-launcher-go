package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/host"
)

func statusController(t *testing.T, socket string, data string) {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Error(err)
		return
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var request host.Request
				if json.NewDecoder(conn).Decode(&request) != nil {
					return
				}
				if request.Action != "status" {
					t.Error("local operation sent a non-status request", request.Action)
				}
				json.NewEncoder(conn).Encode(host.Response{Data: json.RawMessage(data)})
			}()
		}
	}()
}

func TestHumanStatusAndJSONIncludeVersion(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "s")
	statusController(t, socket, `{"VM":"running","BackendReady":true,"SSH":"zero","Display":true,"DisplayMode":"remoteapp","Windows":-1,"Locked":false,"Queued":2,"Operation":"query-vm","LastError":"source error","Memory":{"GuestKnown":true,"Guest":{"total_bytes":8589934592,"available_bytes":2147483648},"LastError":"memory error","Growing":true}}`)
	for _, asJSON := range []bool{false, true} {
		var out, stderr bytes.Buffer
		args := []string{"--socket", socket, "status"}
		if asJSON {
			args = append(args, "--json")
		}
		if err := run(context.Background(), args, &out, &stderr); err != nil {
			t.Fatal(err)
		}
		if asJSON {
			var fields map[string]any
			if err := json.Unmarshal(out.Bytes(), &fields); err != nil || fields["Version"] != versionString() || fields["VM"] != "running" {
				t.Fatal(out.String(), err)
			}
		} else {
			for _, want := range []string{versionString(), "虚拟机：running", "窗口：未知", "显示：已连接（remoteapp）", "8.00 GiB", "2.00 GiB", "source error", "memory error", "扩容"} {
				if !strings.Contains(out.String(), want) {
					t.Fatal("missing status information", want, out.String())
				}
			}
			if json.Valid(out.Bytes()) {
				t.Fatal("default status is JSON")
			}
		}
	}
}

func TestHelpAndOfflineStatusShowVersion(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"--help"}, {"status", "--help"}, {"--socket", filepath.Join(t.TempDir(), "missing"), "status"}} {
		var out, stderr bytes.Buffer
		err := run(context.Background(), args, &out, &stderr)
		if err != nil && !errors.Is(err, flag.ErrHelp) && !strings.Contains(err.Error(), "connect controller") {
			t.Fatal(err)
		}
		if !strings.Contains(out.String()+stderr.String(), versionString()) {
			t.Fatal("version missing", args)
		}
	}
}

func TestStartDoesNotDuplicateAnExistingController(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "s")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "untouched"))
	statusController(t, socket, `{"VM":"paused"}`)
	var out bytes.Buffer
	if err := startHost(context.Background(), "/missing/config", socket, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "已在运行") {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(filepath.Join(root, "untouched")); !os.IsNotExist(err) {
		t.Fatal("start altered existing service configuration")
	}
}

func TestStartInstallsUserUnitAndWaitsForController(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "s")
	marker := filepath.Join(root, "started")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("START_MARKER", marker)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	script := `#!/bin/sh
case "$*" in
 '--user is-active --quiet rdp-launcher.service') exit 3 ;;
 '--user daemon-reload') exit 0 ;;
 '--user start rdp-launcher.service') touch "$START_MARKER"; exit 0 ;;
 *) exit 9 ;;
esac
`
	if err := os.WriteFile(filepath.Join(root, "systemctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() {
		for {
			if _, err := os.Stat(marker); err == nil {
				statusController(t, socket, `{"VM":"paused"}`)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	var out bytes.Buffer
	if err := startHost(ctx, filepath.Join(root, "custom config.toml"), socket, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "已启动") {
		t.Fatal(out.String())
	}
	unit := filepath.Join(root, "config", "systemd", "user", hostUnit)
	if _, err := os.Stat(unit); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(unit+".d", "50-rdp-launcher-command.conf"))
	if err != nil || !strings.Contains(string(data), "custom config.toml") || !strings.Contains(string(data), " serve\n") {
		t.Fatal(string(data), err)
	}
}

func TestServiceOverrideQuotesPathsWithoutExpandingThem(t *testing.T) {
	text, err := hostServiceOverride(`/opt/space dir/app%$"\`, "/tmp/line\nbreak", `/tmp/socket`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`%%$$\"\\`, `/tmp/line\nbreak`, "ExecStart=\nExecStart=", ` --socket "/tmp/socket" serve`} {
		if !strings.Contains(text, want) {
			t.Fatal("unsafe systemd argument", want, text)
		}
	}
	if _, err := hostServiceOverride("bad\x00path", "config", "socket"); err == nil {
		t.Fatal("NUL accepted")
	}
}
