package main

import (
	"math"

	"github.com/egoist/mygo/ui"
)

// Warning text sits on the opaque content card, not on the wallpaper. Keep its
// warning hue, adjusting towards black/white only as far as normal-text AA needs.
func warningColor(theme *ui.Theme) ui.Color {
	base := theme.Warning
	if contrastRatio(base, theme.Surface) >= 4.5 {
		return base
	}
	target := ui.Hex("#000000")
	white := ui.Hex("#ffffff")
	if contrastRatio(white, theme.Surface) > contrastRatio(target, theme.Surface) {
		target = white
	}
	for step := 1; step <= 20; step++ {
		candidate := base.Mix(target, float32(step)/20)
		if contrastRatio(candidate, theme.Surface) >= 4.5 {
			return candidate
		}
	}
	return target
}

func contrastRatio(a, b ui.Color) float64 {
	luminance := func(c ui.Color) float64 {
		linear := func(v uint8) float64 {
			s := float64(v) / 255
			if s <= 0.04045 {
				return s / 12.92
			}
			return math.Pow((s+0.055)/1.055, 2.4)
		}
		return 0.2126*linear(c.R) + 0.7152*linear(c.G) + 0.0722*linear(c.B)
	}
	light, dark := luminance(a), luminance(b)
	if light < dark {
		light, dark = dark, light
	}
	return (light + 0.05) / (dark + 0.05)
}
