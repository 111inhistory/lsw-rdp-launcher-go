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

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
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
				decoder, encoder := json.NewDecoder(conn), json.NewEncoder(conn)
				if !hostGreeting(t, decoder, encoder) {
					return
				}
				var request host.Request
				if decoder.Decode(&request) != nil {
					return
				}
				if request.Action != "status" {
					t.Error("local operation sent a non-status request", request.Action)
				}
				_ = encoder.Encode(host.Response{Type: "result", Data: json.RawMessage(data)})
			}()
		}
	}()
}

func TestHumanStatusIncludesVersionAndRejectsJSON(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "s")
	statusController(t, socket, `{"VM":"running","BackendReady":true,"SSH":"zero","Display":true,"DisplayMode":"remoteapp","Windows":-1,"Locked":false,"Queued":2,"Operation":"query-vm","LastError":"source error","Stage":"dispatch","BlockingReason":"session snapshot stale","RetainedLogins":2,"LogoffDeadline":"2026-10-22T12:00:00Z","Binding":{"reference":{"session_id":4,"authentication_id":"AUTH","boot_identifier":"BOOT"},"sid":"S-1","user":"user","domain":"DOMAIN"},"Sessions":{"Revision":3,"Fresh":true,"Sessions":[{"reference":{"session_id":4,"authentication_id":"AUTH","boot_identifier":"BOOT"},"sid":"S-1","user":"user"}]},"Memory":{"GuestKnown":true,"Guest":{"total_bytes":8589934592,"available_bytes":2147483648},"LastError":"memory error","Growing":true}}`)
	var out, stderr bytes.Buffer
	if err := run(context.Background(), []string{"--socket", socket, "status"}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{versionString(), "虚拟机：running", "窗口：未知", "显示：已连接（remoteapp）", "8.00 GiB", "2.00 GiB", "source error", "memory error", "扩容", "绑定会话：4 DOMAIN\\user", "SSH：0", "登录信息：已更新（1 个登录）", "保留登录：2", "阶段：dispatch", "阻塞原因：session snapshot stale", "空闲注销时间："} {
		if !strings.Contains(out.String(), want) {
			t.Fatal("missing status information", want, out.String())
		}
	}
	if json.Valid(out.Bytes()) {
		t.Fatal("status is JSON")
	}
	out.Reset()
	stderr.Reset()
	if err := run(context.Background(), []string{"--socket", socket, "status", "--json"}, &out, &stderr); err == nil {
		t.Fatal("status --json is still accepted")
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
	text := hostServiceOverride(`/opt/space dir/app%$"\`, "/tmp/line\nbreak", `/tmp/socket`)
	for _, want := range []string{`%%$$\"\\`, `/tmp/line\nbreak`, "ExecStart=\nExecStart=", ` --socket "/tmp/socket" serve`} {
		if !strings.Contains(text, want) {
			t.Fatal("unsafe systemd argument", want, text)
		}
	}
}

func TestStatusRendersUnknownRetainedLoginsForStaleSnapshot(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "s")
	statusController(t, socket, `{"VM":"running","BackendReady":true,"SSH":"zero","Display":true,"DisplayMode":"remoteapp","Windows":-1,"RetainedLogins":-1,"Sessions":{"Revision":4,"Fresh":false,"Sessions":[{"reference":{"session_id":4,"authentication_id":"AUTH","boot_identifier":"BOOT"},"sid":"S-1","user":"user"}]}}`)
	var out, stderr bytes.Buffer
	if err := run(context.Background(), []string{"--socket", socket, "status"}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "保留登录：未知") {
		t.Fatal("negative retained count was not rendered as unknown", out.String())
	}
	if strings.Contains(out.String(), "保留登录：-1") {
		t.Fatal("negative retained count leaked into status", out.String())
	}
	if !strings.Contains(out.String(), "登录信息：待更新（上次记录 1 个登录）") {
		t.Fatal("stale login information was not labelled as a record", out.String())
	}
}

func TestStatusUsesReadableSSHAndHidesSnapshotRevision(t *testing.T) {
	for _, tc := range []struct{ state, want string }{
		{"zero", "0"}, {"nonzero", ">0"}, {"unknown", "未知"}, {"", "未知"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			data, err := json.Marshal(statusView{VM: "running", SSH: tc.state, Windows: -1, RetainedLogins: -1,
				Sessions: controller.SessionSnapshot{Revision: 19, Fresh: true}})
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := printStatus(&out, data); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "SSH："+tc.want+"\n") {
				t.Fatal(out.String())
			}
			for _, jargon := range []string{"新鲜", "修订", "会话快照"} {
				if strings.Contains(out.String(), jargon) {
					t.Fatal("internal terminology leaked", jargon, out.String())
				}
			}
		})
	}
}

func TestPausedStatusShowsRecordedValuesWithoutTreatingThemAsCurrent(t *testing.T) {
	s := statusView{VM: "paused", SSH: "unknown", Windows: -1, RetainedLogins: -1,
		Sessions: controller.SessionSnapshot{Revision: 19, Sessions: []controller.Login{{}}}}
	s.Memory.Guest = controller.GuestMemory{TotalBytes: 8 << 30, AvailableBytes: 2 << 30}
	// Zero actual/requested is valid after reclaim; capacity proves this is a
	// retained full device snapshot, not just an actual-size event.
	s.Memory.Device = controller.MemoryDevice{CapacityBytes: 13 << 30, BlockBytes: 128 << 20}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := printStatus(&out, data); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SSH：未知", "登录信息：上次记录（1 个登录，虚拟机已暂停）", "保留登录：未知",
		"内存（上次记录）：总量 8.00 GiB，可用 2.00 GiB", "virtio-mem（上次记录）：已插入 0.00 GiB，请求 0.00 GiB，容量 13.00 GiB"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal("missing paused record", want, out.String())
		}
	}
	for _, vm := range []string{"running", "off"} {
		s.VM = vm
		data, _ = json.Marshal(s)
		out.Reset()
		if err := printStatus(&out, data); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "GiB") {
			t.Fatal("invalid memory observation shown outside paused records", out.String())
		}
	}
}

func TestPausedStatusDoesNotInventUnobservedValues(t *testing.T) {
	var out bytes.Buffer
	if err := printStatus(&out, json.RawMessage(`{"VM":"paused","SSH":"unknown","RetainedLogins":-1,"Memory":{"Device":{"Alias":"mem0","ActualBytes":1024}}}`)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SSH：未知", "登录信息：未获取", "保留登录：未知", "内存：未知"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal(out.String())
		}
	}
	if strings.Contains(out.String(), "virtio-mem：") || strings.Contains(out.String(), "上次记录") {
		t.Fatal("incomplete event or unobserved value was shown as a full record", out.String())
	}
}
