package main

import (
	"fmt"
	"runtime/debug"
)

// Release builds can set these with -ldflags '-X main.version=... -X main.commit=...'.
// Ordinary go build obtains the commit and dirty state from Go's VCS metadata.
var version = "dev"
var commit string

func versionString() string {
	info, _ := debug.ReadBuildInfo()
	return formatVersion(version, commit, info)
}

func formatVersion(version, commit string, info *debug.BuildInfo) string {
	dirty := false
	// An explicitly supplied commit is authoritative; do not combine it
	// with metadata from an unrelated local checkout.
	if info != nil && commit == "" {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if commit == "" {
					commit = setting.Value
				}
			case "vcs.modified":
				dirty = setting.Value == "true"
			}
		}
	}
	if commit == "" {
		commit = "unknown"
	}
	if len(commit) > 8 {
		commit = commit[:8]
	}
	if dirty {
		commit += "-dirty"
	}
	return fmt.Sprintf("rdp-launcher %s (commit %s)", version, commit)
}
