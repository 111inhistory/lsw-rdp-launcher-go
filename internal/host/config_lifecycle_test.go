package host

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIdleLogoffTimeoutDefaultsToDisabled(t *testing.T) {
	path := writeConfigFile(t, `[server]
host = "192.0.2.10"
user = "alice"
`)
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Lifecycle.IdleLogoff != -1 {
		t.Fatalf("expected idle_logoff_timeout -1, got %d", c.Lifecycle.IdleLogoff)
	}
	cfg, err := c.Adapter()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Policy.IdleLogoff != -1*time.Second {
		t.Fatalf("expected IdleLogoff policy -1s, got %v", cfg.Policy.IdleLogoff)
	}
}

func TestIdleLogoffTimeoutImmediateAfterDisconnect(t *testing.T) {
	path := writeConfigFile(t, `[server]
host = "192.0.2.10"
user = "alice"
[lifecycle]
idle_logoff_timeout = 0
`)
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Lifecycle.IdleLogoff != 0 {
		t.Fatalf("expected idle_logoff_timeout 0, got %d", c.Lifecycle.IdleLogoff)
	}
	cfg, err := c.Adapter()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Policy.IdleLogoff != 0 {
		t.Fatalf("expected IdleLogoff policy 0, got %v", cfg.Policy.IdleLogoff)
	}
}

func TestIdleLogoffTimeoutEqualBoundsWithSuspend(t *testing.T) {
	path := writeConfigFile(t, `[server]
host = "192.0.2.10"
user = "alice"
[lifecycle]
idle_logoff_timeout = 300
vm_suspend_timeout = 300
`)
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := c.Adapter()
	if err != nil {
		t.Fatalf("equal bounds should be allowed, got err: %v", err)
	}
	if cfg.Policy.IdleLogoff != 300*time.Second {
		t.Fatalf("expected IdleLogoff policy 300s, got %v", cfg.Policy.IdleLogoff)
	}
}

func TestIdleLogoffTimeoutSuspendDisabledAllowsAnyTimeout(t *testing.T) {
	path := writeConfigFile(t, `[server]
host = "192.0.2.10"
user = "alice"
[lifecycle]
idle_logoff_timeout = 600
vm_suspend_timeout = 0
`)
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := c.Adapter()
	if err != nil {
		t.Fatalf("when suspend is disabled (0), idle_logoff_timeout should not be bounded by suspend, got: %v", err)
	}
	if cfg.Policy.IdleLogoff != 600*time.Second {
		t.Fatalf("expected IdleLogoff policy 600s, got %v", cfg.Policy.IdleLogoff)
	}
}

func TestIdleLogoffTimeoutDisconnectDisabledAllowed(t *testing.T) {
	path := writeConfigFile(t, `[server]
host = "192.0.2.10"
user = "alice"
[lifecycle]
idle_disconnect_timeout = 0
idle_logoff_timeout = 120
vm_suspend_timeout = 300
`)
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := c.Adapter()
	if err != nil {
		t.Fatalf("idle_logoff_timeout should not depend on idle_disconnect_timeout, got: %v", err)
	}
	if cfg.Policy.IdleDisconnect != 0 || cfg.Policy.IdleLogoff != 120*time.Second {
		t.Fatalf("expected Disconnect 0 and Logoff 120s, got %v and %v", cfg.Policy.IdleDisconnect, cfg.Policy.IdleLogoff)
	}
}

func TestIdleLogoffTimeoutRejectsLessThanMinusOne(t *testing.T) {
	for _, val := range []string{"-2", "-10"} {
		path := writeConfigFile(t, `[server]
host = "192.0.2.10"
user = "alice"
[lifecycle]
idle_logoff_timeout = `+val+`
`)
		c, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Adapter(); err == nil || !strings.Contains(err.Error(), "cannot be less than -1") {
			t.Fatalf("expected error for idle_logoff_timeout=%s, got: %v", val, err)
		}
	}
}

func TestIdleLogoffTimeoutRejectsExceedingSuspend(t *testing.T) {
	path := writeConfigFile(t, `[server]
host = "192.0.2.10"
user = "alice"
[lifecycle]
idle_logoff_timeout = 301
vm_suspend_timeout = 300
`)
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Adapter(); err == nil || !strings.Contains(err.Error(), "cannot exceed vm_suspend_timeout") {
		t.Fatalf("expected error when idle_logoff_timeout exceeds vm_suspend_timeout, got: %v", err)
	}
}

func TestIdleTimeoutDurationOverflowRejection(t *testing.T) {
	overflowSeconds := int64(math.MaxInt64/time.Second) + 1
	if int64(int(overflowSeconds)) != overflowSeconds {
		t.Skip("skipping 64-bit overflow test on 32-bit architecture")
	}

	// Test IdleLogoff overflow (when suspend is disabled so suspend bound does not trigger first)
	var c Config
	c.Server.Host = "192.0.2.10"
	c.Server.BackendPort = 49153
	c.Lifecycle.IdleLogoff = int(overflowSeconds)
	c.Lifecycle.VMSuspend = 0
	if _, err := c.Adapter(); err == nil || !strings.Contains(err.Error(), "idle_logoff_timeout duration overflow") {
		t.Fatalf("expected idle_logoff overflow error, got: %v", err)
	}

	// Test IdleDisconnect overflow
	c = Config{}
	c.Server.Host = "192.0.2.10"
	c.Server.BackendPort = 49153
	c.Lifecycle.IdleLogoff = -1
	c.Lifecycle.IdleDisconnect = int(overflowSeconds)
	if _, err := c.Adapter(); err == nil || !strings.Contains(err.Error(), "idle timeout duration overflow") {
		t.Fatalf("expected idle_disconnect overflow error, got: %v", err)
	}

	// Test VMSuspend overflow
	c = Config{}
	c.Server.Host = "192.0.2.10"
	c.Server.BackendPort = 49153
	c.Lifecycle.IdleLogoff = -1
	c.Lifecycle.VMSuspend = int(overflowSeconds)
	if _, err := c.Adapter(); err == nil || !strings.Contains(err.Error(), "idle timeout duration overflow") {
		t.Fatalf("expected vm_suspend overflow error, got: %v", err)
	}

	// Test overflow from TOML
	path := writeConfigFile(t, `[server]
host = "192.0.2.10"
user = "alice"
[lifecycle]
idle_logoff_timeout = 9223372037
vm_suspend_timeout = 0
`)
	cLoaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cLoaded.Adapter(); err == nil || !strings.Contains(err.Error(), "idle_logoff_timeout duration overflow") {
		t.Fatalf("expected overflow error from TOML, got: %v", err)
	}
}

func TestLegacyAgentPortRejectionWithExplicitMigrationError(t *testing.T) {
	path := writeConfigFile(t, `[server]
host = "192.0.2.10"
user = "alice"
agent_port = 49152
backend_port = 49153
`)
	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "server.agent_port is removed") {
		t.Fatalf("expected explicit migration error for server.agent_port in LoadConfig, got: %v", err)
	}

	// Also test direct Adapter() call
	port := 49152
	var c Config
	c.Server.Host = "192.0.2.10"
	c.Server.LegacyAgentPort = &port
	c.Server.BackendPort = 49153
	c.Lifecycle.IdleLogoff = -1
	if _, err := c.Adapter(); err == nil || !strings.Contains(err.Error(), "server.agent_port is removed") {
		t.Fatalf("expected explicit migration error for server.agent_port in Adapter(), got: %v", err)
	}
}

func TestFreeRDPClientHostnameConfig(t *testing.T) {
	// Default client hostname
	path := writeConfigFile(t, `[server]
host = "192.0.2.10"
user = "alice"
`)
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.FreeRDP.ClientHostname != "rdp-launcher" {
		t.Fatalf("expected default client_hostname 'rdp-launcher', got %q", c.FreeRDP.ClientHostname)
	}
	cfg, err := c.Adapter()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Display.ClientHostname != "rdp-launcher" {
		t.Fatalf("expected adapter DisplayConfig.ClientHostname 'rdp-launcher', got %q", cfg.Display.ClientHostname)
	}

	// Custom valid hostname (<= 15 ASCII)
	path = writeConfigFile(t, `[server]
host = "192.0.2.10"
user = "alice"
[freerdp]
client_hostname = "win11-client"
`)
	c, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = c.Adapter()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Display.ClientHostname != "win11-client" {
		t.Fatalf("expected adapter DisplayConfig.ClientHostname 'win11-client', got %q", cfg.Display.ClientHostname)
	}

	// Exact 15 ASCII boundary (valid)
	fifteen := "123456789012345"
	c.FreeRDP.ClientHostname = fifteen
	cfg, err = c.Adapter()
	if err != nil || cfg.Display.ClientHostname != fifteen {
		t.Fatalf("expected 15-char hostname to be valid, got err: %v", err)
	}

	// 16 ASCII characters (rejected)
	c.FreeRDP.ClientHostname = "1234567890123456"
	if _, err := c.Adapter(); err == nil || !strings.Contains(err.Error(), "freerdp.client_hostname must be 1-15") {
		t.Fatalf("expected error for 16-char hostname, got: %v", err)
	}

	// Non-ASCII characters (rejected)
	c.FreeRDP.ClientHostname = "host-主机"
	if _, err := c.Adapter(); err == nil || !strings.Contains(err.Error(), "freerdp.client_hostname must be 1-15") {
		t.Fatalf("expected error for non-ASCII hostname, got: %v", err)
	}

	// Whitespace characters (rejected)
	c.FreeRDP.ClientHostname = "host name"
	if _, err := c.Adapter(); err == nil || !strings.Contains(err.Error(), "freerdp.client_hostname must be 1-15") {
		t.Fatalf("expected error for hostname with space, got: %v", err)
	}
}

func TestBackendPortValidation(t *testing.T) {
	for _, invalidPort := range []int{0, -1, 65536, 70000} {
		var c Config
		c.Server.Host = "192.0.2.10"
		c.Server.BackendPort = invalidPort
		c.Lifecycle.IdleLogoff = -1
		if _, err := c.Adapter(); err == nil || !strings.Contains(err.Error(), "backend_port must be a valid TCP port") {
			t.Fatalf("expected error for invalid backend port %d, got: %v", invalidPort, err)
		}
	}
}
