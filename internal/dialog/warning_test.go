package dialog

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

func TestWarningTextMeetsContrastOnContentCards(t *testing.T) {
	for _, background := range []string{"#eaeef2", "#27272a", "#ffffff", "#000000", "#777777"} {
		t.Run(background, func(t *testing.T) {
			theme := ui.LightTheme()
			theme.Surface, theme.Warning = ui.Hex(background), ui.Hex("#FF9800")
			got := warningColor(theme)
			if ratio := contrastRatio(got, theme.Surface); ratio < 4.5 {
				t.Fatalf("warning contrast %.2f is below 4.5:1", ratio)
			}
			if got.A != 255 {
				t.Fatal("warning text is translucent")
			}
			if contrastRatio(theme.Warning, theme.Surface) >= 4.5 && got != theme.Warning {
				t.Fatal("already readable warning was unnecessarily adjusted")
			}
		})
	}
	if got := contrastRatio(ui.Hex("#000000"), ui.Hex("#ffffff")); got != 21 {
		t.Fatal("unexpected WCAG contrast calculation", got)
	}
}
