package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

func TestParseOnConflictIsOneMutuallyExclusiveChoice(t *testing.T) {
	for _, value := range []string{"current", "keep", "logoff", "cancel"} {
		got, err := parseOnConflict(value)
		if err != nil || string(got) != value {
			t.Fatal(value, got, err)
		}
	}
	if got, err := parseOnConflict(""); err != nil || got != "" {
		t.Fatal(got, err)
	}
	for _, value := range []string{"yes", "no", "current,keep", "CURRENT"} {
		if _, err := parseOnConflict(value); err == nil {
			t.Fatal("accepted", value)
		}
	}
}

func TestChooseTTYOnlyOffersAllowedActions(t *testing.T) {
	prompt := controller.SwitchPrompt{
		TargetMode: controller.ModeRemoteApp,
		Activity:   controller.SessionActivity{SessionID: 4, User: `DOMAIN\user`},
		Allowed:    []controller.SwitchDecision{controller.SwitchCurrent, controller.SwitchKeep, controller.SwitchCancel},
	}
	var stderr bytes.Buffer
	decision, err := chooseTTY(context.Background(), prompt, strings.NewReader("9\nkeep\n"), &stderr)
	if err != nil || decision != controller.SwitchKeep {
		t.Fatal(decision, err)
	}
	text := stderr.String()
	for _, want := range []string{"会话：4", "在当前桌面执行", "保留旧登录", "取消", "无效选择"} {
		if !strings.Contains(text, want) {
			t.Fatal("missing prompt text", want, text)
		}
	}
	if strings.Contains(text, "注销旧登录") {
		t.Fatal("offered an action the controller did not allow")
	}
}

func TestChooseTTYEOFAndDeadlineDoNotAuthorizeChange(t *testing.T) {
	prompt := controller.SwitchPrompt{TargetMode: controller.ModeDesktop, Allowed: []controller.SwitchDecision{controller.SwitchKeep, controller.SwitchCancel}}
	decision, err := chooseTTY(context.Background(), prompt, strings.NewReader(""), &bytes.Buffer{})
	if err != nil || decision != controller.SwitchCancel {
		t.Fatal(decision, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	blocking, writer := io.Pipe()
	defer writer.Close()
	if _, err = chooseTTY(ctx, prompt, blocking, &bytes.Buffer{}); err == nil {
		t.Fatal("deadline did not cancel the prompt")
	}
}

func TestNoUsableDisplayOrTTYRequiresExplicitConflictChoice(t *testing.T) {
	frontend := confirmFrontend{
		stdin:   strings.NewReader(""),
		stderr:  &bytes.Buffer{},
		wayland: func() bool { return false },
		gui: func(context.Context, controller.SwitchPrompt) (controller.SwitchDecision, error) {
			t.Error("GUI started without a usable Wayland socket")
			return controller.SwitchCancel, nil
		},
	}
	prompt := controller.SwitchPrompt{TargetMode: controller.ModeRemoteApp, Allowed: []controller.SwitchDecision{controller.SwitchKeep, controller.SwitchCancel}}
	_, err := frontend.choose(context.Background(), prompt)
	if err == nil || !strings.Contains(err.Error(), "--on-conflict=current|keep|logoff|cancel") {
		t.Fatal(err)
	}
}

func TestWaylandProbeRequiresAConnectableSocket(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "wayland-test")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", root)
	t.Setenv("WAYLAND_DISPLAY", "wayland-test")
	if !waylandAvailable() {
		t.Fatal("live Wayland socket was not detected")
	}
	_ = listener.Close()
	if waylandAvailable() {
		t.Fatal("stale socket path was accepted")
	}
	t.Setenv("WAYLAND_DISPLAY", "absent-wayland")
	if waylandAvailable() {
		t.Fatal("missing socket was accepted")
	}
	t.Setenv("WAYLAND_DISPLAY", "")
	if waylandAvailable() {
		t.Fatal("empty display was accepted")
	}
}

func TestChooseUsesInProcessGUIWithUsableWayland(t *testing.T) {
	called := 0
	frontend := confirmFrontend{
		stdin:   strings.NewReader(""),
		stderr:  &bytes.Buffer{},
		wayland: func() bool { return true },
		gui: func(_ context.Context, prompt controller.SwitchPrompt) (controller.SwitchDecision, error) {
			called++
			if prompt.TargetMode != controller.ModeDesktop {
				t.Fatal(prompt.TargetMode)
			}
			return controller.SwitchCancel, nil
		},
	}
	decision, err := frontend.choose(context.Background(), controller.SwitchPrompt{TargetMode: controller.ModeDesktop, Allowed: []controller.SwitchDecision{controller.SwitchCancel}})
	if err != nil || decision != controller.SwitchCancel || called != 1 {
		t.Fatal(decision, err, called)
	}
}
