package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
	"github.com/egoist/mygo/ui"
)

func TestDMSPopupAlphaAndActiveMatugenPalette(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "DankMaterialShell")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("settings.json", `{"popupTransparency":0.6}`)
	write("dms-colors.json", `{"mode":"light","colors":{"light":{"surface_container":"#f6fafe","primary":"#1b6484"},"dark":{"primary":"#abcdef"}}}`)
	style := loadDMSStyle(root, root)
	if style == nil || style.alpha != 153 {
		t.Fatal("DMS transparency is alpha, not one minus alpha", style)
	}
	theme := ui.LightTheme()
	style.apply(theme)
	if theme.Background != ui.Hex("#f6fafe") || theme.Accent != ui.Hex("#1b6484") {
		t.Fatal("did not select active Matugen palette", theme)
	}
	for _, opaque := range []bool{false, true} {
		d := &dialog{prompt: controller.SwitchPrompt{TargetMode: "desktop"}, style: style, opaque: opaque}
		tt := ui.NewTester(d.view, 480, 220)
		want := uint8(153)
		if opaque {
			want = 255
		}
		if got := tt.Image().RGBAAt(8, 110).A; got != want {
			t.Fatalf("opaque=%t alpha=%d, want %d", opaque, got, want)
		}
		button, _ := tt.Find("保留并切换")
		if got := tt.Image().RGBAAt(int(button.X+button.W/2), int(button.Y+button.H/2)).A; got != 255 {
			t.Fatal("DMS alpha must not fade controls", got)
		}
	}

	write("settings.json", `{"popupTransparency":0.6,"cornerRadius":8,"fontFamily":"Inter Variable","fontScale":1.25,"fontWeight":500}`)
	style = loadDMSStyle(root, root)
	theme = ui.LightTheme()
	style.apply(theme)
	if theme.Radius != 8 || theme.Font != "Inter Variable" || theme.FontSize != 17.5 || style.fontWeight != 500 {
		t.Fatal("DMS typography or radius was ignored", style, theme)
	}
	d := &dialog{prompt: controller.SwitchPrompt{TargetMode: "desktop"}, style: style}
	tt := ui.NewTester(d.view, int(480*d.scale()), int(220*d.scale()))
	for _, label := range []string{"取消", "注销并切换", "保留并切换"} {
		r, ok := tt.Find(label)
		if !ok || r.X < 0 || r.Y < 0 || r.X+r.W > 600 || r.Y+r.H > 275 {
			t.Fatal("scaled DMS action does not fit", label, r)
		}
	}

	write("settings.json", `{"popupTransparency":2}`)
	write("dms-colors.json", `{"mode":"dark","colors":{"dark":{"primary":"invalid"}}}`)
	style = loadDMSStyle(root, root)
	theme = ui.DarkTheme()
	accent := theme.Accent
	style.apply(theme)
	if style.alpha != 214 || theme.Accent != accent {
		t.Fatal("invalid settings should preserve defaults", style, theme)
	}
}

func TestDMSMissingOrMalformedFilesUseSystemTheme(t *testing.T) {
	root := t.TempDir()
	if style := loadDMSStyle(root, root); style != nil {
		t.Fatal(style)
	}
	dir := filepath.Join(root, "DankMaterialShell")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"settings.json", "dms-colors.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not json"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if style := loadDMSStyle(root, root); style != nil {
		t.Fatal(style)
	}
}
