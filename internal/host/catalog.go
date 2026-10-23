package host

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/adapter"
)

func cacheDir() string { dir, _ := os.UserCacheDir(); return filepath.Join(dir, "rdp-launcher-go") }
func ReadApps() ([]adapter.App, error) {
	data, err := os.ReadFile(filepath.Join(cacheDir(), "apps.json"))
	if err != nil {
		return nil, err
	}
	var apps []adapter.App
	err = json.Unmarshal(data, &apps)
	return apps, err
}

// Configured entries win by ID. Keep them out of the discovery cache so deleting
// a configuration entry cannot resurrect a stale manual application.
func (c Config) Catalog(discovered []adapter.App) []adapter.App {
	apps := append([]adapter.App(nil), c.Apps...)
	for _, app := range discovered {
		if !slices.ContainsFunc(c.Apps, func(manual adapter.App) bool { return manual.ID == app.ID }) {
			apps = append(apps, app)
		}
	}
	// Filter after merging so manual entries cannot bypass an explicit exclusion.
	// This slice is a copy: policy must not rewrite the discovery cache or config.
	return slices.DeleteFunc(apps, c.Filters.excludes)
}

func (c Config) ListApps() ([]adapter.App, error) {
	apps, err := ReadApps()
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return c.Catalog(apps), nil
}

func (c Config) ResolveApp(target string) adapter.App {
	apps, _ := ReadApps()
	for _, app := range c.Catalog(apps) {
		if app.ID == target || strings.EqualFold(app.Name, target) {
			return app
		}
	}
	// Explicit targets remain launchable. Catalog exclusion hides discovery;
	// it is not an authorization rule for executing a caller-supplied path.
	return adapter.App{Target: target}
}

// Only files in our desktop namespace are replaced or removed.
func (c Config) SyncApps(apps []adapter.App, executable, config, socket string) error {
	cache := cacheDir()
	if err := os.MkdirAll(cache, 0700); err != nil {
		return err
	}
	lockFile, err := os.OpenFile(filepath.Join(cache, ".sync.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	if err = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("sync already in progress: %w", err)
	}

	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "share")
	}
	dir = filepath.Join(dir, "applications")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	// desktop-file-edit requires .desktop even for temporary entries. Under
	// .sync.lock, no live sync owns these leftovers from interrupted writers.
	abandoned, err := filepath.Glob(filepath.Join(dir, ".rdp-launcher-go-stage-*.desktop"))
	if err != nil {
		return err
	}
	for _, path := range abandoned {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove abandoned desktop stage: %w", err)
		}
	}

	var stagedFiles []string
	defer func() {
		for _, f := range stagedFiles {
			_ = os.Remove(f)
		}
	}()

	// Persist facts before applying host policy. A later filter change can use
	// the same cache; only generated desktop entries reflect the current policy.
	stagedCache, err := stageCache(cache, apps)
	if err != nil {
		return err
	}
	stagedFiles = append(stagedFiles, stagedCache)

	catalogApps := c.Catalog(apps)

	keepIcons := make(map[string]bool)
	keepDesktops := make(map[string]bool)

	var stagedIconTargets []string
	stagedIconsMap := make(map[string]string)

	var stagedDesktopTargets []string
	stagedDesktopsMap := make(map[string]string)

	for _, app := range catalogApps {
		name := "rdp-launcher-go-" + url.PathEscape(app.ID)
		targetDesktop := filepath.Join(dir, name+".desktop")
		keepDesktops[filepath.Clean(targetDesktop)] = true

		command := desktopQuote(executable) + " --config " + desktopQuote(config) + " --socket " + desktopQuote(socket) + " open --app " + desktopQuote(app.ID) + " %F"
		categories := "Network;RemoteAccess;"
		var mimes []string
		for _, override := range c.Overrides {
			match := strings.ToLower(override.Match)
			if strings.EqualFold(app.ID, match) || strings.Contains(strings.ToLower(app.Name), match) || strings.Contains(strings.ToLower(app.Target), match) {
				if override.Categories != "" {
					categories = override.Categories
				}
				if len(override.Mimes) > 0 {
					mimes = override.Mimes
				}
				break
			}
		}
		fields := map[string]string{"Name": app.Name, "Exec": command, "Categories": categories}
		if len(mimes) > 0 {
			fields["MimeType"] = strings.Join(mimes, ";") + ";"
		}

		if app.IconBase64 != "" {
			targetIcon := filepath.Join(cache, name+".png")
			cleanIcon := filepath.Clean(targetIcon)
			keepIcons[cleanIcon] = true

			stagedIcon, err := stageIcon(cache, app.IconBase64)
			if err != nil {
				return fmt.Errorf("icon %s: %w", app.ID, err)
			}
			stagedFiles = append(stagedFiles, stagedIcon)
			if prev, exists := stagedIconsMap[targetIcon]; exists {
				_ = os.Remove(prev)
			} else {
				stagedIconTargets = append(stagedIconTargets, targetIcon)
			}
			stagedIconsMap[targetIcon] = stagedIcon
			fields["Icon"] = targetIcon
		} else if app.Icon != "" {
			fields["Icon"] = app.Icon
			keepIcons[filepath.Clean(app.Icon)] = true
			if !filepath.IsAbs(app.Icon) {
				keepIcons[filepath.Clean(filepath.Join(cache, app.Icon))] = true
			}
		}

		stagedDesktop, err := stageDesktopEntry(dir, fields)
		if err != nil {
			return err
		}
		stagedFiles = append(stagedFiles, stagedDesktop)
		if prev, exists := stagedDesktopsMap[targetDesktop]; exists {
			_ = os.Remove(prev)
		} else {
			stagedDesktopTargets = append(stagedDesktopTargets, targetDesktop)
		}
		stagedDesktopsMap[targetDesktop] = stagedDesktop
	}

	// The built-in desktop connection is a host feature, not a discovered app.
	// Its filename cannot collide with per-app IDs (including an ID "desktop").
	desktopTarget := filepath.Join(dir, "rdp-launcher-go.desktop")
	desktopStage, err := stageDesktopEntry(dir, map[string]string{
		"Name": "Windows 桌面", "Comment": "连接 Windows 完整桌面",
		"Exec": desktopQuote(executable) + " --config " + desktopQuote(config) + " --socket " + desktopQuote(socket) + " desktop",
		"Icon": "computer", "Categories": "Network;RemoteAccess;",
	})
	if err != nil {
		return err
	}
	stagedFiles = append(stagedFiles, desktopStage)
	stagedDesktopTargets = append(stagedDesktopTargets, desktopTarget)
	stagedDesktopsMap[desktopTarget] = desktopStage

	if err := os.Rename(stagedCache, filepath.Join(cache, "apps.json")); err != nil {
		return err
	}
	for _, target := range stagedIconTargets {
		if err := os.Rename(stagedIconsMap[target], target); err != nil {
			return err
		}
	}
	for _, target := range stagedDesktopTargets {
		if err := os.Rename(stagedDesktopsMap[target], target); err != nil {
			return err
		}
	}

	oldDesktops, err := filepath.Glob(filepath.Join(dir, "rdp-launcher-go-*.desktop"))
	if err != nil {
		return err
	}
	for _, path := range oldDesktops {
		if !keepDesktops[filepath.Clean(path)] {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}

	oldIcons, err := filepath.Glob(filepath.Join(cache, "rdp-launcher-go-*.png"))
	if err != nil {
		return err
	}
	for _, path := range oldIcons {
		if !keepIcons[filepath.Clean(path)] {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}

	return nil
}

func stageCache(cacheDir string, apps []adapter.App) (string, error) {
	data, err := json.MarshalIndent(apps, "", "  ")
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp(cacheDir, ".apps-*.json")
	if err != nil {
		return "", err
	}
	filePath := file.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(filePath)
		}
	}()

	if _, err := file.Write(data); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(filePath, 0600); err != nil {
		return "", err
	}
	cleanup = false
	return filePath, nil
}

func stageIcon(cacheDir string, iconBase64 string) (string, error) {
	png, err := base64.StdEncoding.DecodeString(iconBase64)
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp(cacheDir, ".rdp-launcher-go-icon-*.png")
	if err != nil {
		return "", err
	}
	filePath := file.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(filePath)
		}
	}()

	if _, err := file.Write(png); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(filePath, 0600); err != nil {
		return "", err
	}
	cleanup = false
	return filePath, nil
}

func stageDesktopEntry(dir string, fields map[string]string) (string, error) {
	file, err := os.CreateTemp(dir, ".rdp-launcher-go-stage-*.desktop")
	if err != nil {
		return "", err
	}
	filePath := file.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(filePath)
		}
	}()

	_, err = file.WriteString("[Desktop Entry]\nType=Application\nName=Application\nTerminal=false\n")
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}

	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var args []string
	for _, key := range keys {
		args = append(args, "--set-key="+key, "--set-value="+fields[key])
	}
	args = append(args, filePath)
	if output, err := exec.Command("desktop-file-edit", args...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("serialize desktop entry with desktop-file-edit (desktop-file-utils): %w: %s", err, output)
	}
	if err = os.Chmod(filePath, 0644); err != nil {
		return "", err
	}
	cleanup = false
	return filePath, nil
}
func desktopQuote(s string) string {
	s = strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "`", "\\`", "$", "\\$", "%", "%%").Replace(s)
	return "\"" + s + "\""
}

// desktop-file-edit uses GLib's Desktop Entry key-file serializer. In
// particular, its string escaping is not Go quoting, INI quoting or shell quoting.
func writeDesktopEntry(path string, fields map[string]string) error {
	staged, err := stageDesktopEntry(filepath.Dir(path), fields)
	if err != nil {
		return err
	}
	defer os.Remove(staged)
	return os.Rename(staged, path)
}

// Mounts are explicit drive-to-host-directory mappings, shared with the old TOML.
func (c Config) WindowsPath(input string) (string, error) {
	if strings.HasPrefix(input, "file:") {
		u, err := url.Parse(input)
		if err != nil {
			return "", err
		}
		if u.Host != "" && u.Host != "localhost" {
			return "", fmt.Errorf("file URI must be local")
		}
		input = u.Path
	}
	if len(input) >= 3 && input[1] == ':' && (input[2] == '\\' || input[2] == '/') || strings.HasPrefix(input, `\\`) {
		return input, nil
	}
	absolute, err := filepath.Abs(input)
	if err != nil {
		return "", err
	}
	var drive, root string
	for candidate, directory := range c.Mounts {
		directory, err = filepath.Abs(directory)
		if err != nil {
			return "", err
		}
		relative, e := filepath.Rel(directory, absolute)
		if e == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && len(directory) > len(root) {
			drive, root = candidate, directory
		}
	}
	if root == "" {
		return "", fmt.Errorf("no [mounts] mapping for %s", input)
	}
	relative, _ := filepath.Rel(root, absolute)
	drive = strings.ToUpper(strings.TrimSuffix(drive, ":"))
	if len(drive) != 1 || drive[0] < 'A' || drive[0] > 'Z' {
		return "", fmt.Errorf("invalid mount drive %q", drive)
	}
	if relative == "." {
		relative = ""
	}
	return drive + `:\` + strings.ReplaceAll(relative, "/", `\`), nil
}
func (c Config) LinuxPath(input string) (string, error) {
	if len(input) < 3 || input[1] != ':' || (input[2] != '\\' && input[2] != '/') {
		return "", fmt.Errorf("expected a Windows drive path")
	}
	drive := strings.ToUpper(input[:1])
	for key, root := range c.Mounts {
		if strings.ToUpper(strings.TrimSuffix(key, ":")) == drive {
			relative := strings.TrimLeft(strings.ReplaceAll(input[2:], `\`, "/"), "/")
			result := filepath.Join(root, relative)
			normalized, e := filepath.Rel(root, result)
			if e != nil || normalized == ".." || strings.HasPrefix(normalized, "../") {
				return "", fmt.Errorf("path leaves mapped drive")
			}
			return result, nil
		}
	}
	return "", fmt.Errorf("no [mounts] mapping for %s:", drive)
}
