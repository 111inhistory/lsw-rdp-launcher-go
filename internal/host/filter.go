package host

import (
	"path"
	"strings"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/adapter"
)

// AppFilters is host catalog policy, not a guest scan directive or execution ACL.
// Keep the guest's raw discovery data so changing policy needs no agent RPC.
type AppFilters struct {
	ExcludeApps []string `toml:"exclude_apps"`
	ExcludeDirs []string `toml:"exclude_dirs"`
}

// These are Windows paths supplied by the guest, never Linux filesystem paths.
// No local filesystem lookups, mount conversions or environment expansion.
// Matching is lexical; the host cannot resolve guest junctions or NTFS aliases.
func remotePath(value string) string {
	value = strings.ReplaceAll(value, `\`, "/")
	if value == "" {
		return ""
	}
	// Clean only the rooted suffix: C:/../../Tools must not lose its drive.
	if len(value) >= 3 && value[1] == ':' && value[2] == '/' {
		return strings.ToLower(value[:2] + path.Clean(value[2:]))
	}
	unc := strings.HasPrefix(value, "//")
	value = path.Clean(value)
	if unc {
		value = "/" + value
	}
	return strings.ToLower(value)
}

func (f AppFilters) excludes(app adapter.App) bool {
	for _, match := range f.ExcludeApps {
		if match == "" {
			continue
		}
		if strings.EqualFold(match, app.ID) || strings.EqualFold(match, app.Name) || remotePath(match) == remotePath(app.Target) || (app.ShortcutPath != "" && remotePath(match) == remotePath(app.ShortcutPath)) {
			return true
		}
	}
	for _, directory := range f.ExcludeDirs {
		directory = strings.TrimRight(remotePath(directory), "/")
		if directory == "" {
			continue
		}
		for _, value := range []string{app.ShortcutPath, app.Target} {
			value = remotePath(value)
			// Require a component boundary: excluding SDK must not exclude SDKTools.
			if value == directory || strings.HasPrefix(value, directory+"/") {
				return true
			}
		}
	}
	return false
}
