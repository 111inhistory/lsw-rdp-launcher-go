package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
	"github.com/111inhistory/lsw-rdp-launcher-go/internal/dialog"
)

// confirmFrontend shows the same action model in a native Wayland window or on
// a terminal. The controller supplies Prompt.Allowed; neither view grants
// authority the controller did not already offer.
type confirmFrontend struct {
	stdin   io.Reader
	stderr  io.Writer
	wayland func() bool
	gui     func(context.Context, controller.SwitchPrompt) (controller.SwitchDecision, error)
}

func newConfirmFrontend(stdin io.Reader, stderr io.Writer) confirmFrontend {
	return confirmFrontend{stdin: stdin, stderr: stderr, wayland: waylandAvailable, gui: dialog.Choose}
}

func (f confirmFrontend) choose(ctx context.Context, prompt controller.SwitchPrompt) (controller.SwitchDecision, error) {
	if f.wayland() {
		return f.gui(ctx, prompt)
	}
	if isTerminal(f.stdin) {
		return chooseTTY(ctx, prompt, f.stdin, f.stderr)
	}
	return "", fmt.Errorf("cannot ask how to handle the existing Windows login: no usable Wayland connection and no TTY; pass --on-conflict=current|keep|logoff|cancel")
}

// waylandAvailable proves that the compositor socket accepts a connection.
// WAYLAND_DISPLAY or a leftover socket file alone is not evidence of a usable
// display, so the in-process GUI is not started in that case.
func waylandAvailable() bool {
	name := os.Getenv("WAYLAND_DISPLAY")
	if name == "" {
		return false
	}
	if !filepath.IsAbs(name) {
		runtime := os.Getenv("XDG_RUNTIME_DIR")
		if runtime == "" {
			return false
		}
		name = filepath.Join(runtime, name)
	}
	conn, err := net.DialTimeout("unix", name, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func parseOnConflict(value string) (controller.SwitchDecision, error) {
	if value == "" {
		return "", nil
	}
	decision := controller.SwitchDecision(value)
	switch decision {
	case controller.SwitchCurrent, controller.SwitchKeep, controller.SwitchLogoff, controller.SwitchCancel:
		return decision, nil
	default:
		return "", fmt.Errorf("--on-conflict must be one of current, keep, logoff or cancel")
	}
}

func isTerminal(r io.Reader) bool {
	file, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func conflictSummary(prompt controller.SwitchPrompt) string {
	target := "RemoteApp"
	if prompt.TargetMode == controller.ModeDesktop {
		target = "完整桌面"
	}
	activity := prompt.Activity
	var text strings.Builder
	fmt.Fprintf(&text, "切换到%s前需要处理已有 Windows 登录。\n", target)
	if activity.SessionID != 0 {
		fmt.Fprintf(&text, "会话：%d", activity.SessionID)
		if activity.User != "" {
			fmt.Fprintf(&text, "（%s）", activity.User)
		}
		fmt.Fprintln(&text)
	}
	if activity.Windows != nil && activity.TrayIcons != nil {
		fmt.Fprintf(&text, "窗口：%d  托盘：%d\n", *activity.Windows, *activity.TrayIcons)
	} else {
		fmt.Fprintln(&text, "窗口/托盘活动：未知")
	}
	if prompt.Allows(controller.SwitchLogoff) {
		fmt.Fprintln(&text, "注销会关闭该登录的所有应用，未保存的内容将丢失。")
	}
	return text.String()
}

func decisionLabel(decision controller.SwitchDecision) string {
	switch decision {
	case controller.SwitchCurrent:
		return "在当前桌面执行"
	case controller.SwitchKeep:
		return "保留旧登录并连接目标模式"
	case controller.SwitchLogoff:
		return "注销旧登录并连接目标模式（未保存内容会丢失）"
	case controller.SwitchCancel:
		return "取消"
	default:
		return string(decision)
	}
}

func parseDecision(input string, allowed []controller.SwitchDecision) (controller.SwitchDecision, bool) {
	if input == "" {
		return "", false
	}
	if len(input) == 1 && input[0] >= '1' && input[0] <= '9' {
		index := int(input[0] - '1')
		if index < len(allowed) {
			return allowed[index], true
		}
		return "", false
	}
	value := controller.SwitchDecision(strings.ToLower(input))
	for _, decision := range allowed {
		if decision == value {
			return decision, true
		}
	}
	return "", false
}

func chooseTTY(ctx context.Context, prompt controller.SwitchPrompt, in io.Reader, stderr io.Writer) (controller.SwitchDecision, error) {
	if len(prompt.Allowed) == 0 {
		return "", fmt.Errorf("controller offered no confirmation choices")
	}
	fmt.Fprint(stderr, conflictSummary(prompt))
	for index, decision := range prompt.Allowed {
		fmt.Fprintf(stderr, "  %d) %s [%s]\n", index+1, decisionLabel(decision), decision)
	}
	fmt.Fprint(stderr, "选择: ")

	lines := make(chan string)
	errs := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(in)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
		errs <- scanner.Err()
	}()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case err := <-errs:
			if err != nil {
				return "", err
			}
			// EOF is a closed prompt, not an authorization to change the login.
			return controller.SwitchCancel, nil
		case line := <-lines:
			if decision, ok := parseDecision(strings.TrimSpace(line), prompt.Allowed); ok {
				return decision, nil
			}
			fmt.Fprintf(stderr, "无效选择；请输入 1-%d 或动作名称: ", len(prompt.Allowed))
		}
	}
}
