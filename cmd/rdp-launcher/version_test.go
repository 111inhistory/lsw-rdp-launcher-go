package main

import (
	"bytes"
	"context"
	"path/filepath"
	"runtime/debug"
	"testing"
)

func TestFormatVersion(t *testing.T) {
	for _, tc := range []struct {
		name, version, commit string
		info                  *debug.BuildInfo
		want                  string
	}{
		{"no metadata", "dev", "", nil, "rdp-launcher dev (commit unknown)"},
		{"explicit commit ignores local dirty state", "v1.2.3", "1234567890abcdef", &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "different"}, {Key: "vcs.modified", Value: "true"}}}, "rdp-launcher v1.2.3 (commit 12345678)"},
		{"clean", "dev", "", &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.modified", Value: "false"}}}, "rdp-launcher dev (commit abc123)"},
		{"dirty", "dev", "", &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.modified", Value: "true"}}}, "rdp-launcher dev (commit abc123-dirty)"},
		{"no automatic module version", "dev", "", &debug.BuildInfo{Main: debug.Module{Version: "v0.2.0"}}, "rdp-launcher dev (commit unknown)"},
		{"no dated pseudo version", "dev", "", &debug.BuildInfo{Main: debug.Module{Version: "v0.0.0-20261021011623-bf4a70400d0c+dirty"}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "bf4a70400d0cf9e44d4be01852df4263f351712d"}, {Key: "vcs.modified", Value: "true"}}}, "rdp-launcher dev (commit bf4a7040-dirty)"},
		{"development module", "dev", "", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "rdp-launcher dev (commit unknown)"},
		{"release override", "v1.2.3", "release123", &debug.BuildInfo{Main: debug.Module{Version: "v0.2.0"}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}}}, "rdp-launcher v1.2.3 (commit release1)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatVersion(tc.version, tc.commit, tc.info); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVersionDoesNotNeedConfigOrController(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		var out, stderr bytes.Buffer
		missing := filepath.Join(t.TempDir(), "missing")
		args = append([]string{"--config", missing + ".toml", "--socket", missing + ".sock"}, args...)
		if err := run(context.Background(), args, &out, &stderr); err != nil {
			t.Fatal(err)
		}
		if out.String() != versionString()+"\n" || stderr.Len() != 0 {
			t.Fatal(out.String(), stderr.String())
		}
	}
}

func TestVersionRejectsExtraArguments(t *testing.T) {
	for _, args := range [][]string{{"version", "extra"}, {"--version", "serve"}} {
		var out, stderr bytes.Buffer
		if err := run(context.Background(), args, &out, &stderr); err == nil || out.Len() != 0 {
			t.Fatal("accepted extra arguments", args, out.String(), err)
		}
	}
}
