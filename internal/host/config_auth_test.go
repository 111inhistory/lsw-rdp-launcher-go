package host

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSecretTool puts a deterministic secret-tool on PATH. It records argv and
// either emits FAKE_SECRET_FILE unchanged (lookup) or consumes stdin (store).
func fakeSecretTool(t *testing.T) (argsPath string) {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "secret-tool")
	content := `#!/bin/sh
printf '%s\n' "$@" > "$FAKE_ARGS_FILE"
case "$1" in
  lookup) cat "$FAKE_SECRET_FILE" ;;
  store) cat > "$FAKE_STORE_FILE" ;;
esac
`
	if err := os.WriteFile(script, []byte(content), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_ARGS_FILE", filepath.Join(dir, "arguments"))
	t.Setenv("FAKE_STORE_FILE", filepath.Join(dir, "stored"))
	return filepath.Join(dir, "arguments")
}

func writeFakeSecret(t *testing.T, secret []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, secret, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_SECRET_FILE", path)
}

func testConfig() Config {
	var c Config
	c.Server.Service = "rdp-bridge"
	c.Server.User = "alice"
	c.Server.AgentService = "rdp-bridge-agent"
	return c
}

func TestPasswordPreservesExactKeyringBytes(t *testing.T) {
	fakeSecretTool(t)
	writeFakeSecret(t, []byte("p@ss word with  spaces"))
	password, err := testConfig().Password(context.Background())
	if err != nil || password != "p@ss word with  spaces" {
		t.Fatalf("password=%q err=%v", password, err)
	}
}

func TestRetrievedSecretsRejectCRLFAndEmpty(t *testing.T) {
	fakeSecretTool(t)
	for _, secret := range []string{"secret\n", "secret\r\n", "sec\nret", "secret\r"} {
		writeFakeSecret(t, []byte(secret))
		if _, err := testConfig().Password(context.Background()); err == nil || !strings.Contains(err.Error(), "CR/LF") {
			t.Fatalf("secret %q: %v", secret, err)
		}
		if _, err := testConfig().AgentToken(context.Background()); err == nil || !strings.Contains(err.Error(), "CR/LF") {
			t.Fatalf("agent token %q: %v", secret, err)
		}
	}
	writeFakeSecret(t, nil)
	if _, err := testConfig().Password(context.Background()); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatal(err)
	}
}

func TestAgentTokenUsesSeparateKeyringEntryAndRejectsNonUTF8(t *testing.T) {
	args := fakeSecretTool(t)
	writeFakeSecret(t, []byte("shared-token"))
	token, err := testConfig().AgentToken(context.Background())
	if err != nil || token != "shared-token" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	arguments, err := os.ReadFile(args)
	if err != nil {
		t.Fatal(err)
	}
	got := string(arguments)
	if !strings.Contains(got, "rdp-bridge-agent") || !strings.Contains(got, "\nagent\n") {
		t.Fatalf("agent token selectors not separate: %q", got)
	}
	if strings.Contains(got, "alice") {
		t.Fatalf("agent token reused the RDP user selector: %q", got)
	}

	writeFakeSecret(t, []byte{0xff, 0xfe})
	if _, err = testConfig().AgentToken(context.Background()); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatal(err)
	}
}

func TestLoadConfigRejectsEmptyOverrideMatch(t *testing.T) {
	for _, match := range []string{`""`, `"   "`} {
		path := filepath.Join(t.TempDir(), "config.toml")
		content := "[[overrides]]\nmatch = " + match + "\ncategories = \"Office;\"\n"
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "non-empty match") {
			t.Fatalf("match %s: %v", match, err)
		}
	}
}

func TestAgentServiceDefaultsToSeparateConsistentName(t *testing.T) {
	for _, tc := range []struct {
		name, config, service, agentService string
	}{
		{"default", "", "rdp-bridge", "rdp-bridge-agent"},
		{"derived", "[server]\nservice = \"custom\"\n", "custom", "custom-agent"},
		{"explicit", "[server]\nservice = \"custom\"\nagent_service = \"shared-auth\"\n", "custom", "shared-auth"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.config), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := LoadConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if c.Server.Service != tc.service || c.Server.AgentService != tc.agentService {
				t.Fatalf("services = %q / %q", c.Server.Service, c.Server.AgentService)
			}
		})
	}
}

func TestAdapterWiresAgentTokenAndWindowsTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `[server]
host = "192.0.2.10"
user = "alice"
agent_token_file = 'C:\ProgramData\rdp-launcher\agent-token'
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.AgentTokenFile != `C:\ProgramData\rdp-launcher\agent-token` {
		t.Fatalf("config agent token file = %q", c.Server.AgentTokenFile)
	}
	cfg, err := c.Adapter()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AgentToken == nil {
		t.Fatal("adapter has no agent token provider")
	}
}
