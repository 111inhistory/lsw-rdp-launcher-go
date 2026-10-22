// Native, short-lived confirmation UI. No webview and no session-control code.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

type dialog struct {
	prompt   controller.SwitchPrompt
	decision controller.SwitchDecision
	done     bool
	opaque   bool // Accessibility preference: do not draw translucent surfaces.
	style    *dmsStyle
	close    func()
}

func (d *dialog) choose(value controller.SwitchDecision) {
	if d.done {
		return
	}
	d.decision, d.done = value, true
	if d.close != nil {
		d.close()
	}
}
func count(value *uint32) string {
	if value == nil {
		return "未知"
	}
	return fmt.Sprint(*value)
}
func (d *dialog) scale() float32 {
	if d.style != nil {
		return d.style.fontScale
	}
	return 1
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
	target := "RemoteApp"
	if d.prompt.TargetMode == "desktop" {
		target = "完整桌面"
	}
	a := d.prompt.Activity
	scale, weight := d.scale(), 400
	if d.style != nil {
		weight = d.style.fontWeight
	}
	ui.Column(c).Fill().Padding(24 * scale).Gap(12 * scale).Radius(theme.Radius).Background(background).FontWeight(weight).Children(func() {
		ui.Text(c, "切换到"+target+"？").FontSize(20 * scale).FontWeight(min(weight+100, 900))
		// Keep the actions outside the scroll viewport: font fallback or
		// oversized text must not push Cancel/Keep/Logoff off-screen.
		ui.Scroll(c).Fill().Children(func() {
			ui.Column(c).FillWidth().Padding(12 * scale).Gap(8 * scale).Radius(theme.Radius).Background(theme.Surface).Children(func() {
				if a.CanLogoff() {
					ui.Text(c, "窗口 "+count(a.Windows)+" · 托盘 "+count(a.TrayIcons)).TextColor(theme.TextMuted)
					ui.Text(c, "注销将关闭所有应用，未保存的内容会丢失。").TextColor(warningColor(&theme))
				} else {
					ui.Text(c, "无法确认当前会话，仅支持保留切换。").TextColor(theme.TextMuted)
				}
			})
		})
		ui.Row(c).Gap(12 * scale).Justify(ui.End).Children(func() {
			cancel := ui.Button(c.Key("cancel"), "取消").Height(40*scale).Padding(0, 16*scale).FontWeight(500).AutoFocus()
			// AutoFocus need not enable MyGo's keyboard-only focus ring. Draw
			// an explicit outline while focused, including the first opening.
			if cancel.Focused() {
				cancel.Border(2*scale, theme.Accent)
			}
			if cancel.Clicked() {
				d.choose(controller.SwitchCancel)
			}
			if ui.Button(c.Key("logoff"), "注销并切换").Height(40*scale).Padding(0, 16*scale).FontWeight(500).Disabled(!a.CanLogoff()).TextColor(theme.Danger).Clicked() {
				d.choose(controller.SwitchLogoff)
			}
			if ui.PrimaryButton(c.Key("keep"), "保留并切换").Height(40*scale).Padding(0, 16*scale).FontWeight(500).Clicked() {
				d.choose(controller.SwitchKeep)
			}
		})
	})
}
func main() {
	var prompt controller.SwitchPrompt
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 64<<10)).Decode(&prompt); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if prompt.TargetMode != "desktop" && prompt.TargetMode != "remoteapp" {
		fmt.Fprintln(os.Stderr, "invalid target mode")
		os.Exit(1)
	}
	d := &dialog{prompt: prompt, decision: controller.SwitchCancel}
	mygo.App.SetName("RDP Launcher")
	mygo.App.WhenReady(func() {
		prefs := mygo.Theme.Preferences()
		d.opaque = prefs.ReduceTransparency || prefs.HighContrast
		if !prefs.HighContrast {
			configDir, _ := os.UserConfigDir()
			cacheDir, _ := os.UserCacheDir()
			if configDir != "" && cacheDir != "" {
				d.style = loadActiveDMSStyle(configDir, cacheDir, os.Getenv("XDG_RUNTIME_DIR"), os.Getenv("WAYLAND_DISPLAY"))
			}
		}
		width, height := int(480*d.scale()), int(220*d.scale())
		window := mygo.NewWindow(mygo.WindowOptions{Title: "切换 Windows 连接模式", Width: width, Height: height, MinWidth: width, MinHeight: height, DisableResize: true, Transparent: true, Content: ui.View(d.view)})
		d.close = window.Close
		window.OnClosed(func() {
			if !d.done {
				d.decision = controller.SwitchCancel
			}
		})
	})
	if err := mygo.App.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(d.decision); err != nil {
		os.Exit(1)
	}
}
