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
func (d *dialog) view(c *ui.Context) {
	if c.Shortcut(0, ui.KeyEscape) {
		d.choose(controller.SwitchCancel)
	}
	theme := c.Theme()
	ui.Column(c).Fill().Padding(24).Gap(16).Children(func() {
		ui.Text(c, "切换 Windows 连接模式").FontSize(22).Bold()
		target := "RemoteApp"
		if d.prompt.TargetMode == "desktop" {
			target = "完整桌面"
		}
		ui.Scroll(c).Fill().Children(func() {
			ui.Column(c).FillWidth().Gap(12).Children(func() {
				ui.Text(c, "将切换到："+target)
				a := d.prompt.Activity
				session := "未能唯一确定当前会话"
				if a.SessionID != 0 {
					session = fmt.Sprintf("当前会话：%d · %s", a.SessionID, a.User)
				}
				ui.Text(c, session).TextColor(theme.TextMuted)
				ui.Text(c, "用户窗口："+count(a.Windows)+"    用户托盘："+count(a.TrayIcons))
				if a.Note != "" {
					ui.Text(c, a.Note).TextColor(theme.TextMuted)
				}
				ui.Text(c, "保留：应用留在旧会话中，不会出现在另一种模式的会话里。")
				ui.Text(c, "注销：关闭旧会话中的所有应用，未保存的内容可能丢失。").TextColor(theme.Danger)
			})
		})
		a := d.prompt.Activity
		ui.Row(c).Gap(12).Justify(ui.End).Children(func() {
			if ui.Button(c.Key("cancel"), "取消").AutoFocus().Clicked() {
				d.choose(controller.SwitchCancel)
			}
			if ui.Button(c.Key("logoff"), "注销并切换").Disabled(!a.CanLogoff()).TextColor(theme.Danger).Clicked() {
				d.choose(controller.SwitchLogoff)
			}
			if ui.PrimaryButton(c.Key("keep"), "保留并切换").Clicked() {
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
		window := mygo.NewWindow(mygo.WindowOptions{Title: "切换 Windows 连接模式", Width: 640, Height: 420, MinWidth: 600, MinHeight: 400, Content: ui.View(d.view)})
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
