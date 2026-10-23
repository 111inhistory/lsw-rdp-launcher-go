package dialog

import (
	"encoding/json"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
)

// DMS already renders Matugen colors. Reuse its active palette and metrics
// instead of adding another template/output file or a second theme generator.
type dmsStyle struct {
	colors      map[string]string
	hasSettings bool
	alpha       uint8
	radius      float32
	font        string
	fontScale   float32
	fontWeight  int
}

// A stopped shell can leave both its theme cache and runtime files behind.
// Only use DMS styling when its session matches this display and is reachable.
func loadActiveDMSStyle(configDir, cacheDir, runtimeDir, display string) *dmsStyle {
	if !dmsRunning(runtimeDir, display) {
		return nil
	}
	return loadDMSStyle(configDir, cacheDir)
}

func dmsRunning(runtimeDir, display string) bool {
	if runtimeDir == "" || display == "" {
		return false
	}
	files, err := filepath.Glob(filepath.Join(runtimeDir, "dms-*.session"))
	if err != nil {
		return false
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	for _, sessionFile := range files {
		if time.Until(deadline) <= 0 {
			return false
		}
		data, err := os.ReadFile(sessionFile)
		if err != nil || strings.TrimSpace(string(data)) != display {
			continue
		}
		base := strings.TrimSuffix(sessionFile, ".session")
		parent := strings.TrimPrefix(filepath.Base(base), "dms-")
		data, err = os.ReadFile(base + ".pid")
		if err != nil || !dmsProcessAlive(parent) || !dmsProcessAlive(strings.TrimSpace(string(data))) {
			continue
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		conn, err := net.DialTimeout("unix", base+".sock", remaining)
		if err == nil {
			_ = conn.Close()
			return true
		}
	}
	return false
}

func dmsProcessAlive(value string) bool {
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 0 {
		return false
	}
	info, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid)))
	return err == nil && info.IsDir()
}

func loadDMSStyle(configDir, cacheDir string) *dmsStyle {
	style := &dmsStyle{alpha: 255, radius: 12, font: "Inter Variable", fontScale: 1, fontWeight: 400}
	found := false
	var settings *struct {
		PopupTransparency          *float64 `json:"popupTransparency"`
		FloatingWindowSyncGlobal   *bool    `json:"floatingWindowSyncGlobal"`
		FloatingWindowTransparency *float64 `json:"floatingWindowTransparency"`
		CornerRadius               *float32 `json:"cornerRadius"`
		FontFamily                 string   `json:"fontFamily"`
		FontScale                  float32  `json:"fontScale"`
		FontWeight                 int      `json:"fontWeight"`
	}
	if data, err := os.ReadFile(filepath.Join(configDir, "DankMaterialShell", "settings.json")); err == nil && json.Unmarshal(data, &settings) == nil && settings != nil {
		found = true
		style.hasSettings = true
		alpha := settings.PopupTransparency
		if settings.FloatingWindowSyncGlobal != nil && !*settings.FloatingWindowSyncGlobal {
			alpha = settings.FloatingWindowTransparency
		}
		if alpha != nil && *alpha >= 0 && *alpha <= 1 {
			style.alpha = uint8(math.Round(*alpha * 255))
		}
		if settings.CornerRadius != nil && *settings.CornerRadius >= 0 && *settings.CornerRadius <= 128 {
			style.radius = *settings.CornerRadius
		}
		if settings.FontFamily != "" {
			style.font = settings.FontFamily
		}
		if settings.FontScale >= 0.5 && settings.FontScale <= 3 {
			style.fontScale = settings.FontScale
		}
		if settings.FontWeight >= 100 && settings.FontWeight <= 900 {
			style.fontWeight = settings.FontWeight
		}
	}
	var palette struct {
		Mode   string                       `json:"mode"`
		Colors map[string]map[string]string `json:"colors"`
	}
	if data, err := os.ReadFile(filepath.Join(cacheDir, "DankMaterialShell", "dms-colors.json")); err == nil && json.Unmarshal(data, &palette) == nil {
		if palette.Mode == "light" || palette.Mode == "dark" {
			style.colors = palette.Colors[palette.Mode]
			found = found || len(style.colors) > 0
		}
	}
	if !found {
		return nil // DMS is optional; preserve the system theme on other desktops.
	}
	return style
}

func (s *dmsStyle) apply(theme *ui.Theme) {
	if s.hasSettings {
		theme.Radius, theme.Font = s.radius, s.font
		theme.FontSize, theme.Spacing = 14*s.fontScale, 4*s.fontScale
	}
	for _, entry := range []struct {
		key   string
		color *ui.Color
	}{
		{"surface_container", &theme.Background},
		{"surface_container_high", &theme.Surface},
		{"on_surface", &theme.Text},
		{"on_surface_variant", &theme.TextMuted},
		{"primary", &theme.Accent},
		{"on_primary", &theme.AccentText},
		{"error", &theme.Danger},
	} {
		value := s.colors[entry.key]
		if len(value) != 7 || value[0] != '#' {
			continue
		}
		if _, err := strconv.ParseUint(value[1:], 16, 24); err == nil {
			*entry.color = ui.Hex(value)
		}
	}
	// DMS Theme.qml uses #FF9800 for warning; normal text needs a darker
	// variant on a light card (selected by warningColor at render time).
	theme.Warning = ui.Hex("#FF9800")
	// DankButton state layers use the label color at 12% hover / 20% press.
	theme.SurfaceHover = theme.Surface.Mix(theme.Text, 0.12)
	theme.SurfacePressed = theme.Surface.Mix(theme.Text, 0.20)
	theme.Border = theme.Surface
	theme.AccentHover = theme.Accent.Mix(theme.AccentText, 0.12)
	theme.AccentPressed = theme.Accent.Mix(theme.AccentText, 0.20)
	theme.Focus = theme.Accent
	theme.Focus.A = 140
}
