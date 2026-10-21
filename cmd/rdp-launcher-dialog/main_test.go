package main

import (
	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
	"github.com/egoist/mygo/ui"
	"image/png"
	"os"
	"strings"
	"testing"
)

func TestNativeChoicesAndEscape(t *testing.T) {
	for _, tc := range []struct {
		label string
		want  controller.SwitchDecision
	}{
		{"取消", controller.SwitchCancel}, {"保留并切换", controller.SwitchKeep}, {"注销并切换", controller.SwitchLogoff},
	} {
		t.Run(tc.label, func(t *testing.T) {
			one := uint32(1)
			d := &dialog{prompt: controller.SwitchPrompt{Activity: controller.SessionActivity{SessionID: 2, User: "user", Fingerprint: "logon", Windows: &one, TrayIcons: nil}, TargetMode: "desktop"}, decision: controller.SwitchCancel}
			closed := 0
			d.close = func() { closed++ }
			tt := ui.NewTester(d.view, 640, 420)
			if !tt.HasText("注销：关闭旧会话中的所有应用，未保存的内容可能丢失。") {
				t.Fatal(tt.Texts())
			}
			if err := tt.Click(tc.label); err != nil {
				t.Fatal(err)
			}
			if d.decision != tc.want || closed != 1 {
				t.Fatal(d.decision, closed)
			}
		})
	}
	d := &dialog{prompt: controller.SwitchPrompt{TargetMode: "desktop"}, decision: controller.SwitchCancel}
	tt := ui.NewTester(d.view, 640, 420)
	tt.Key(0, ui.KeyEscape)
	if !d.done || d.decision != controller.SwitchCancel {
		t.Fatal(d.decision)
	}
}
func TestDialogLayoutAndSafeDefault(t *testing.T) {
	one := uint32(1)
	d := &dialog{prompt: controller.SwitchPrompt{TargetMode: "desktop", Activity: controller.SessionActivity{SessionID: 2, User: `computer\user`, Fingerprint: "logon", Windows: &one, Note: strings.Repeat("无法可靠确认用户托盘状态，不会自动注销。", 5)}}, decision: controller.SwitchCancel}
	tt := ui.NewTester(d.view, 600, 400)
	for _, label := range []string{"取消", "保留并切换", "注销并切换"} {
		r, ok := tt.Find(label)
		if !ok || r.X < 0 || r.Y < 0 || r.X+r.W > 600 || r.Y+r.H > 400 {
			t.Fatal(label, r)
		}
	}
	if path := os.Getenv("RDP_DIALOG_PREVIEW"); path != "" {
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		err = png.Encode(file, tt.Image())
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	tt.Key(0, ui.KeyEnter)
	if !d.done || d.decision != controller.SwitchCancel {
		t.Fatal("Enter did not default to cancel", d.decision)
	}
}

func TestUnknownSessionCannotBeLoggedOff(t *testing.T) {
	d := &dialog{prompt: controller.SwitchPrompt{TargetMode: "remoteapp"}, decision: controller.SwitchCancel}
	tt := ui.NewTester(d.view, 640, 420)
	_ = tt.Click("注销并切换")
	if d.done {
		t.Fatal("unidentified session could be logged off")
	}
	if !tt.HasText("用户窗口：未知    用户托盘：未知") {
		t.Fatal(tt.Texts())
	}
}
