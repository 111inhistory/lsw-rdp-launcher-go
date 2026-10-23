// Package dialog shows the short-lived native confirmation UI in the CLI
// process that owns the request. It never starts a child process, and importing
// it does not initialize the GUI: only Choose touches the GUI toolkit.
package dialog

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

// action is one presentation choice. The controller supplies which actions are
// allowed; the view never broadens that set.
type action struct {
	decision controller.SwitchDecision
	label    string
	primary  bool
	danger   bool
}

var allActions = []action{
	{decision: controller.SwitchCancel, label: "取消"},
	{decision: controller.SwitchLogoff, label: "注销并切换", danger: true},
	{decision: controller.SwitchKeep, label: "保留并切换"},
	{decision: controller.SwitchCurrent, label: "在当前桌面执行", primary: true},
}

// dialog owns the confirmation window. choose, abort and OnClosed can run on
// different goroutines, so the result fields are guarded.
type dialog struct {
	prompt controller.SwitchPrompt

	mu       sync.Mutex
	decision controller.SwitchDecision
	done     bool
	ctxErr   error
	close    func()

	opaque bool // Accessibility preference: do not draw translucent surfaces.
	style  *dmsStyle
}

func (d *dialog) actions() []action {
	available := make([]action, 0, len(allActions))
	hasPrimary := false
	for _, choice := range allActions {
		if d.prompt.Allows(choice.decision) {
			available = append(available, choice)
			hasPrimary = hasPrimary || choice.primary
		}
	}
	// Keep the familiar primary action when the current desktop is not offered;
	// otherwise the rightmost/current choice stays primary.
	if !hasPrimary {
		for i := range available {
			if available[i].decision == controller.SwitchKeep {
				available[i].primary = true
			}
		}
	}
	return available
}

func (d *dialog) choose(value controller.SwitchDecision) {
	d.mu.Lock()
	if d.done {
		d.mu.Unlock()
		return
	}
	d.decision, d.done = value, true
	closeWindow := d.close
	d.mu.Unlock()
	if closeWindow != nil {
		closeWindow()
	}
}

// abort turns a canceled/timed-out context into a cancel result. A decision
// that already arrived wins: cancellation must not rewrite a user's choice.
func (d *dialog) abort(err error) {
	d.mu.Lock()
	if d.done {
		d.mu.Unlock()
		return
	}
	d.decision, d.done, d.ctxErr = controller.SwitchCancel, true, err
	closeWindow := d.close
	d.mu.Unlock()
	if closeWindow != nil {
		closeWindow()
	}
}

func (d *dialog) closed() {
	d.mu.Lock()
	if !d.done {
		d.decision, d.done = controller.SwitchCancel, true
	}
	d.mu.Unlock()
}

func (d *dialog) result() (controller.SwitchDecision, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctxErr != nil {
		return "", d.ctxErr
	}
	if !d.done {
		return "", fmt.Errorf("confirmation window closed without a decision")
	}
	return d.decision, nil
}

func (d *dialog) scale() float32 {
	if d.style != nil {
		return d.style.fontScale
	}
	return 1
}

func (d *dialog) title() string {
	target := "RemoteApp"
	if d.prompt.TargetMode == controller.ModeDesktop {
		target = "完整桌面"
	}
	if d.prompt.Allows(controller.SwitchCurrent) && !d.prompt.Allows(controller.SwitchKeep) && !d.prompt.Allows(controller.SwitchLogoff) {
		return "在当前桌面执行？"
	}
	return "切换到" + target + "？"
}

func (d *dialog) size() (int, int) {
	scale := d.scale()
	width := float32(480)
	if d.prompt.Allows(controller.SwitchCurrent) {
		width = 560
	}
	return int(width * scale), int(220 * scale)
}

func count(value *uint32) string {
	if value == nil {
		return "未知"
	}
	return fmt.Sprint(*value)
}

func (d *dialog) view(c *ui.Context) {
	if c.Shortcut(0, ui.KeyEscape) {
		d.choose(controller.SwitchCancel)
	}
	// Keep the system's light/dark appearance, accent and font. Use a subtle
	// accent tint and round controls instead of a stark white information panel.
	theme := *c.Theme()
	theme.Surface = theme.Background.Mix(theme.Accent, 0.08)
	theme.SurfaceHover = theme.Background.Mix(theme.Accent, 0.14)
	theme.SurfacePressed = theme.Background.Mix(theme.Accent, 0.20)
	theme.Border = theme.Surface
	if d.style == nil {
		theme.Radius = 18
	}
	background := theme.Background.Mix(theme.Accent, 0.04)
	alpha := uint8(214)
	if d.style != nil {
		d.style.apply(&theme)
		// Match DankFloatingWindow's surface alpha. The compositor supplies
		// a sharp wallpaper backdrop; readable content has its own solid card.
		background = theme.Background
		if d.style.hasSettings {
			alpha = d.style.alpha
		}
	}
	if !d.opaque {
		background.A = alpha // Only the panel is translucent; text/controls stay opaque.
		theme.Background = ui.Transparent
	}
	c.SetTheme(&theme)
	a := d.prompt.Activity
	scale, weight := d.scale(), 400
	if d.style != nil {
		weight = d.style.fontWeight
	}
	ui.Column(c).Fill().Padding(24 * scale).Gap(12 * scale).Radius(theme.Radius).Background(background).FontWeight(weight).Children(func() {
		ui.Text(c, d.title()).FontSize(20 * scale).FontWeight(min(weight+100, 900))
		// Keep the actions outside the scroll viewport: font fallback or
		// oversized text must not push Cancel/Keep/Logoff off-screen.
		ui.Scroll(c).Fill().Children(func() {
			ui.Column(c).FillWidth().Padding(12 * scale).Gap(8 * scale).Radius(theme.Radius).Background(theme.Surface).Children(func() {
				if d.prompt.Allows(controller.SwitchLogoff) {
					ui.Text(c, "窗口 "+count(a.Windows)+" · 托盘 "+count(a.TrayIcons)).TextColor(theme.TextMuted)
					ui.Text(c, "注销将关闭所有应用，未保存的内容会丢失。").TextColor(warningColor(&theme))
				} else {
					ui.Text(c, "不会注销当前会话。").TextColor(theme.TextMuted)
				}
			})
		})
		ui.Row(c).Gap(12 * scale).Justify(ui.End).Children(func() {
			actions := d.actions()
			for _, choice := range actions {
				var button ui.Element
				if choice.primary {
					button = ui.PrimaryButton(c.Key(string(choice.decision)), choice.label)
				} else {
					button = ui.Button(c.Key(string(choice.decision)), choice.label)
				}
				button.Height(40*scale).Padding(0, 16*scale).FontWeight(500)
				if choice.danger {
					button.TextColor(theme.Danger)
				}
				if choice.decision == controller.SwitchCancel {
					// AutoFocus need not enable MyGo's keyboard-only focus ring.
					// Draw an explicit outline while focused, including the
					// first opening.
					button.AutoFocus()
					if button.Focused() {
						button.Border(2*scale, theme.Accent)
					}
				}
				if button.Clicked() {
					d.choose(choice.decision)
					return
				}
			}
		})
	})
}

// Choose displays the confirmation UI and returns the user's decision. It must
// be called from the process main goroutine because the GUI event loop owns
// that goroutine. The caller checks Wayland connectivity and provides its own
// TTY/stderr fallback before choosing to show this UI.
//
// Window close and Escape cancel the confirmation. Context cancellation closes
// the window and returns the context error; it never starts a destructive
// action.
func Choose(ctx context.Context, prompt controller.SwitchPrompt) (controller.SwitchDecision, error) {
	if err := validatePrompt(prompt); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	d := &dialog{prompt: prompt, decision: controller.SwitchCancel}
	mygo.App.SetName("RDP Launcher")
	mygo.App.WhenReady(func() {
		d.mu.Lock()
		aborted := d.done
		d.mu.Unlock()
		if aborted {
			mygo.App.Quit()
			return
		}
		prefs := mygo.Theme.Preferences()
		d.opaque = prefs.ReduceTransparency || prefs.HighContrast
		if !prefs.HighContrast {
			configDir, _ := os.UserConfigDir()
			cacheDir, _ := os.UserCacheDir()
			if configDir != "" && cacheDir != "" {
				d.style = loadActiveDMSStyle(configDir, cacheDir, os.Getenv("XDG_RUNTIME_DIR"), os.Getenv("WAYLAND_DISPLAY"))
			}
		}
		width, height := d.size()
		window := mygo.NewWindow(mygo.WindowOptions{Title: "切换 Windows 连接模式", Width: width, Height: height, MinWidth: width, MinHeight: height, DisableResize: true, Transparent: true, Content: ui.View(d.view)})
		d.mu.Lock()
		d.close = window.Close
		aborted = d.done
		d.mu.Unlock()
		window.OnClosed(d.closed)
		if aborted {
			window.Close()
		}
	})
	finished := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			d.abort(ctx.Err())
		case <-finished:
		}
	}()
	err := mygo.App.Run()
	close(finished)
	if err != nil {
		return "", err
	}
	return d.result()
}

func validatePrompt(prompt controller.SwitchPrompt) error {
	if prompt.TargetMode != controller.ModeDesktop && prompt.TargetMode != controller.ModeRemoteApp {
		return fmt.Errorf("invalid confirmation target mode %q", prompt.TargetMode)
	}
	if len(prompt.Allowed) == 0 {
		return fmt.Errorf("confirmation has no allowed actions")
	}
	for _, decision := range prompt.Allowed {
		switch decision {
		case controller.SwitchCancel, controller.SwitchKeep, controller.SwitchLogoff, controller.SwitchCurrent:
		default:
			return fmt.Errorf("confirmation has unknown action %q", decision)
		}
		if !prompt.Allows(decision) {
			return fmt.Errorf("confirmation action %q is not allowed by the prompt", decision)
		}
	}
	return nil
}
