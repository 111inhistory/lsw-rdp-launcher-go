package dialog

import (
	"context"
	"errors"
	"image/png"
	"os"
	"strings"
	"testing"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
	"github.com/egoist/mygo/ui"
)

func testPrompt(target string, allowed ...controller.SwitchDecision) controller.SwitchPrompt {
	return controller.SwitchPrompt{TargetMode: target, Allowed: allowed}
}

func switchPrompt() controller.SwitchPrompt {
	return testPrompt("desktop", controller.SwitchCancel, controller.SwitchKeep, controller.SwitchLogoff)
}

func allChoicesPrompt() controller.SwitchPrompt {
	return testPrompt("desktop", controller.SwitchCancel, controller.SwitchKeep, controller.SwitchLogoff, controller.SwitchCurrent)
}

func (d *dialog) chosen() (controller.SwitchDecision, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.decision, d.done
}

func TestRenderedChoicesAndEscape(t *testing.T) {
	for _, tc := range []struct {
		prompt controller.SwitchPrompt
		label  string
		want   controller.SwitchDecision
	}{
		{switchPrompt(), "取消", controller.SwitchCancel},
		{switchPrompt(), "保留并切换", controller.SwitchKeep},
		{switchPrompt(), "注销并切换", controller.SwitchLogoff},
		{allChoicesPrompt(), "在当前桌面执行", controller.SwitchCurrent},
	} {
		t.Run(tc.label, func(t *testing.T) {
			one := uint32(1)
			tc.prompt.Activity = controller.SessionActivity{SessionID: 2, User: "user", Windows: &one, TrayIcons: nil}
			d := &dialog{prompt: tc.prompt, decision: controller.SwitchCancel}
			closed := 0
			d.close = func() { closed++ }
			width, height := d.size()
			tt := ui.NewTester(d.view, width, height)
			if tc.prompt.Allows(controller.SwitchLogoff) && !tt.HasText("注销将关闭所有应用，未保存的内容会丢失。") {
				t.Fatal(tt.Texts())
			}
			if err := tt.Click(tc.label); err != nil {
				t.Fatal(err)
			}
			if decision, done := d.chosen(); !done || decision != tc.want || closed != 1 {
				t.Fatal(decision, done, closed)
			}
		})
	}
	d := &dialog{prompt: switchPrompt(), decision: controller.SwitchCancel}
	tt := ui.NewTester(d.view, 480, 220)
	tt.Key(0, ui.KeyEscape)
	if decision, done := d.chosen(); !done || decision != controller.SwitchCancel {
		t.Fatal(decision, done)
	}
}

func TestRendersOnlyAllowedActions(t *testing.T) {
	prompt := testPrompt("desktop", controller.SwitchCancel, controller.SwitchKeep, controller.SwitchCurrent)
	// A logoff-capable identified session is not enough: only prompt.Allowed
	// grants the action.
	prompt.Activity = controller.SessionActivity{
		SessionID: 2,
		User:      `DOMAIN\user`,
		SID:       "S-1-5-21-1",
		Reference: controller.SessionRef{SessionID: 2, AuthenticationID: "00000000000003E7", BootIdentifier: "11111111-2222-4333-8444-555555555555"},
	}
	d := &dialog{prompt: prompt, decision: controller.SwitchCancel}
	width, height := d.size()
	tt := ui.NewTester(d.view, width, height)
	for _, label := range []string{"取消", "保留并切换", "在当前桌面执行"} {
		if _, ok := tt.Find(label); !ok {
			t.Fatalf("allowed action %q missing: %v", label, tt.Texts())
		}
	}
	for _, label := range []string{"注销并切换", "注销将关闭所有应用，未保存的内容会丢失。"} {
		if tt.HasText(label) {
			t.Fatalf("unallowed logoff UI %q rendered: %v", label, tt.Texts())
		}
	}
	if !tt.HasText("不会注销当前会话。") {
		t.Fatal(tt.Texts())
	}

	d = &dialog{prompt: testPrompt("remoteapp", controller.SwitchCancel), decision: controller.SwitchCancel}
	width, height = d.size()
	tt = ui.NewTester(d.view, width, height)
	if _, ok := tt.Find("取消"); !ok {
		t.Fatal(tt.Texts())
	}
	for _, label := range []string{"保留并切换", "在当前桌面执行", "注销并切换"} {
		if tt.HasText(label) {
			t.Fatalf("unexpected action %q: %v", label, tt.Texts())
		}
	}
}

func TestDialogLayoutAndSafeDefault(t *testing.T) {
	one := uint32(1)
	prompt := testPrompt("desktop", controller.SwitchCancel, controller.SwitchKeep, controller.SwitchLogoff)
	prompt.Activity = controller.SessionActivity{SessionID: 2, User: `computer\user`, Windows: &one, Note: strings.Repeat("无法可靠确认用户托盘状态，不会自动注销。", 5)}
	d := &dialog{prompt: prompt, decision: controller.SwitchCancel}
	tt := ui.NewTester(d.view, 480, 220)
	texts := strings.Join(tt.Texts(), "\n")
	if len(tt.Texts()) > 6 || strings.Contains(texts, d.prompt.Activity.User) || strings.Contains(texts, d.prompt.Activity.Note) {
		t.Fatal("dialog includes unnecessary session details", tt.Texts())
	}
	for _, label := range []string{"取消", "保留并切换", "注销并切换"} {
		r, ok := tt.Find(label)
		if !ok || r.X < 0 || r.Y < 0 || r.X+r.W > 480 || r.Y+r.H > 220 {
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
	if decision, done := d.chosen(); !done || decision != controller.SwitchCancel {
		t.Fatal("Enter did not default to cancel", decision, done)
	}
}

func TestCurrentDesktopActionLayout(t *testing.T) {
	d := &dialog{prompt: allChoicesPrompt(), decision: controller.SwitchCancel}
	width, height := d.size()
	tt := ui.NewTester(d.view, width, height)
	for _, label := range []string{"取消", "注销并切换", "保留并切换", "在当前桌面执行"} {
		r, ok := tt.Find(label)
		if !ok || r.X < 0 || r.Y < 0 || r.X+r.W > float32(width) || r.Y+r.H > float32(height) {
			t.Fatal("current desktop action is clipped", label, r)
		}
	}
	if err := tt.Click("在当前桌面执行"); err != nil {
		t.Fatal(err)
	}
	if decision, done := d.chosen(); !done || decision != controller.SwitchCurrent {
		t.Fatal(decision, done)
	}
}

func TestCancelFocusAndLogoffWarningAreVisible(t *testing.T) {
	prompt := switchPrompt()
	prompt.Activity = controller.SessionActivity{SessionID: 2, User: "user"}
	d := &dialog{prompt: prompt, decision: controller.SwitchCancel}
	tt := ui.NewTester(d.view, 480, 220)
	tt.Frame()
	if !tt.Focused("取消") {
		t.Fatal("Cancel lacks initial focus")
	}
	button, _ := tt.Find("取消")
	image := tt.Image()
	accent := ui.LightTheme().Accent
	focusPixels := 0
	// Find returns the label bounds; the control outline is outside them.
	for y := int(button.Y) - 16; y < int(button.Y+button.H)+16; y++ {
		for x := int(button.X) - 16; x < int(button.X+button.W)+16; x++ {
			p := image.RGBAAt(x, y)
			if p.R == accent.R && p.G == accent.G && p.B == accent.B {
				focusPixels++
			}
		}
	}
	if focusPixels < 8 {
		t.Fatal("Cancel focus outline is not visible")
	}
	warning, _ := tt.Find("注销将关闭所有应用，未保存的内容会丢失。")
	card := ui.LightTheme().Background.Mix(ui.LightTheme().Accent, 0.08)
	for y := int(warning.Y); y < int(warning.Y+warning.H); y++ {
		for x := int(warning.X); x < int(warning.X+warning.W); x++ {
			p := image.RGBAAt(x, y)
			if p.R > p.G && int(p.G) > 2*int(p.B) && contrastRatio(ui.RGB(p.R, p.G, p.B), card) >= 4.5 {
				return
			}
		}
	}
	t.Fatal("warning has no readable amber text")
}

func TestScrollableBodyPreservesActionsWithLargeFonts(t *testing.T) {
	for _, prompt := range []controller.SwitchPrompt{switchPrompt(), allChoicesPrompt()} {
		for _, scale := range []float32{1, 1.5, 2} {
			d := &dialog{prompt: prompt, style: &dmsStyle{hasSettings: true, alpha: 153, radius: 12, font: "sans-serif", fontScale: scale, fontWeight: 700}}
			base := float32(480)
			if prompt.Allows(controller.SwitchCurrent) {
				base = 560
			}
			width, height := int(base*scale), int(180*scale)
			tt := ui.NewTester(d.view, width, height)
			for _, choice := range d.actions() {
				r, ok := tt.Find(choice.label)
				if !ok || r.X < 0 || r.Y < 0 || r.X+r.W > float32(width) || r.Y+r.H > float32(height) {
					t.Fatal("action clipped with constrained scroll body", scale, choice.label, r)
				}
			}
			tt.Key(0, ui.KeyEnter)
			if decision, done := d.chosen(); !done || decision != controller.SwitchCancel {
				t.Fatal("large-font Enter did not cancel", decision, done)
			}
		}
	}
}

func TestPanelTransparencyKeepsControlsOpaque(t *testing.T) {
	for _, opaque := range []bool{false, true} {
		d := &dialog{prompt: switchPrompt(), opaque: opaque}
		tt := ui.NewTester(d.view, 480, 220)
		image := tt.Image()
		wantAlpha := uint8(214)
		if opaque {
			wantAlpha = 255
		}
		if alpha := image.RGBAAt(8, 110).A; alpha != wantAlpha {
			t.Fatalf("opaque=%t: panel alpha=%d, want %d", opaque, alpha, wantAlpha)
		}
		button, ok := tt.Find("保留并切换")
		if !ok {
			t.Fatal("missing primary action")
		}
		if alpha := image.RGBAAt(int(button.X+button.W/2), int(button.Y+button.H/2)).A; alpha != 255 {
			t.Fatalf("button faded with panel: alpha=%d", alpha)
		}
	}
}

func TestWindowCloseCancels(t *testing.T) {
	d := &dialog{prompt: switchPrompt(), decision: controller.SwitchKeep}
	d.closed()
	decision, err := d.result()
	if err != nil || decision != controller.SwitchCancel {
		t.Fatal(decision, err)
	}
}

func TestContextAbortCancelsButDoesNotOverrideDecision(t *testing.T) {
	d := &dialog{prompt: switchPrompt(), decision: controller.SwitchCancel}
	d.abort(context.DeadlineExceeded)
	decision, err := d.result()
	if decision != "" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(decision, err)
	}

	d = &dialog{prompt: switchPrompt(), decision: controller.SwitchCancel}
	d.choose(controller.SwitchLogoff)
	d.abort(context.Canceled)
	decision, err = d.result()
	if err != nil || decision != controller.SwitchLogoff {
		t.Fatal("context cancellation overwrote the user's choice", decision, err)
	}
}

func TestChooseRejectsBeforeGUIInitialization(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Choose(ctx, switchPrompt()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, prompt := range []controller.SwitchPrompt{
		testPrompt("desktop"),
		testPrompt("desktop", controller.SwitchDecision("delete")),
	} {
		if _, err := Choose(context.Background(), prompt); err == nil || !strings.Contains(err.Error(), "no allowed actions") {
			t.Fatal(err)
		}
	}
}
