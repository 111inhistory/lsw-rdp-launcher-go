// Package host connects the local CLI to one resident controller.
package host

import (
	"context"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/adapter"
	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
	"github.com/pelletier/go-toml/v2"
)

// AgentTokenUsername is the keyring username selector for the shared Windows
// agent token. It is deliberately separate from the RDP login user.
const AgentTokenUsername = "agent"

type Config struct {
	Server struct {
		Host, User, Service string
		AgentService        string `toml:"agent_service"`
		AgentTokenFile      string `toml:"agent_token_file"`
		LegacyAgentPort     *int   `toml:"agent_port"`
		BackendPort         int    `toml:"backend_port"`
	}
	Libvirt struct{ Socket, URI string }
	FreeRDP struct {
		Bin            string
		AppID          string `toml:"app_id"`
		ClientHostname string `toml:"client_hostname"`
		VideoDriver    string `toml:"video_driver"`
		ScaleDesktop   int    `toml:"scale_desktop"`
		Clipboard      bool
		CertIgnore     bool              `toml:"cert_ignore"`
		ExtraArgs      []string          `toml:"extra_args"`
		Env            map[string]string `toml:"env"`
	}
	RemoteApp struct {
		DefaultApp string `toml:"default_app"`
	}
	Lifecycle struct {
		VMName         string `toml:"vm_name"`
		IdleDisconnect int    `toml:"idle_disconnect_timeout"`
		IdleLogoff     int    `toml:"idle_logoff_timeout"`
		VMSuspend      int    `toml:"vm_suspend_timeout"`
		MemoryAlias    string `toml:"virtio_mem_alias"`
	}
	Memory *struct {
		StartPercent int `toml:"start_percent"`
		StopPercent  int `toml:"stop_percent"`
	}
	Filters   AppFilters
	Apps      []adapter.App
	Mounts    map[string]string
	Overrides []struct {
		Match      string `toml:"match"`
		Mimes      []string
		Categories string
	}
}

func DefaultConfigPath() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "rdp-launcher-go", "config.toml")
}
func DefaultSocket() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = cacheDir()
	}
	return filepath.Join(dir, "rdp-launcher-go.sock")
}
func LoadConfig(path string) (Config, error) {
	var c Config
	c.Server.Service = "rdp-bridge"
	c.Server.BackendPort = 49153
	c.Libvirt.Socket = "/run/libvirt/libvirt-sock"
	c.Libvirt.URI = "qemu:///system"
	c.FreeRDP.Bin = "sdl-freerdp"
	c.FreeRDP.AppID = adapter.DefaultFreeRDPAppID
	c.FreeRDP.ClientHostname = "rdp-launcher"
	c.FreeRDP.Clipboard = true
	c.RemoteApp.DefaultApp = "remoteapp-launcher.exe"
	c.Lifecycle.VMName = "win11"
	c.Lifecycle.IdleDisconnect = 30
	c.Lifecycle.IdleLogoff = -1
	c.Lifecycle.VMSuspend = 300
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err = toml.Unmarshal(data, &c); err != nil {
		return c, err
	}
	if c.Server.LegacyAgentPort != nil {
		return c, fmt.Errorf("server.agent_port is removed; the interactive endpoint is obsolete, configure server.backend_port instead")
	}
	if c.Server.AgentService == "" {
		c.Server.AgentService = c.Server.Service + "-agent"
	}
	ids := make(map[string]bool, len(c.Apps))
	for _, app := range c.Apps {
		if strings.TrimSpace(app.ID) == "" || strings.TrimSpace(app.Name) == "" || strings.TrimSpace(app.Target) == "" {
			return c, fmt.Errorf("[[apps]] requires id, name and target")
		}
		if ids[app.ID] {
			return c, fmt.Errorf("duplicate [[apps]] id %q", app.ID)
		}
		ids[app.ID] = true
	}
	for _, override := range c.Overrides {
		if strings.TrimSpace(override.Match) == "" {
			return c, fmt.Errorf("[[overrides]] requires a non-empty match")
		}
	}
	return c, nil
}
func (c Config) Adapter() (adapter.Config, error) {
	if net.ParseIP(c.Server.Host) == nil {
		return adapter.Config{}, fmt.Errorf("server.host must be an explicit guest IP")
	}
	if c.Server.LegacyAgentPort != nil {
		return adapter.Config{}, fmt.Errorf("server.agent_port is removed; the interactive endpoint is obsolete, configure server.backend_port instead")
	}
	if c.Server.BackendPort <= 0 || c.Server.BackendPort > 65535 {
		return adapter.Config{}, fmt.Errorf("backend_port must be a valid TCP port (1-65535)")
	}
	hostname := c.FreeRDP.ClientHostname
	if hostname == "" {
		hostname = "rdp-launcher"
	}
	if len(hostname) > 15 || !isASCII(hostname) {
		return adapter.Config{}, fmt.Errorf("freerdp.client_hostname must be 1-15 printable ASCII characters without whitespace")
	}
	const maxDurationSeconds = int64(math.MaxInt64 / time.Second)
	if c.Lifecycle.IdleDisconnect < 0 || c.Lifecycle.VMSuspend < 0 {
		return adapter.Config{}, fmt.Errorf("idle timeouts cannot be negative")
	}
	if int64(c.Lifecycle.IdleDisconnect) > maxDurationSeconds || int64(c.Lifecycle.VMSuspend) > maxDurationSeconds {
		return adapter.Config{}, fmt.Errorf("idle timeout duration overflow")
	}
	if c.Lifecycle.IdleLogoff < -1 {
		return adapter.Config{}, fmt.Errorf("idle_logoff_timeout cannot be less than -1")
	}
	if int64(c.Lifecycle.IdleLogoff) > maxDurationSeconds {
		return adapter.Config{}, fmt.Errorf("idle_logoff_timeout duration overflow")
	}
	if c.Lifecycle.VMSuspend > 0 && c.Lifecycle.IdleLogoff > c.Lifecycle.VMSuspend {
		return adapter.Config{}, fmt.Errorf("idle_logoff_timeout (%d) cannot exceed vm_suspend_timeout (%d)", c.Lifecycle.IdleLogoff, c.Lifecycle.VMSuspend)
	}
	policy := controller.Options{
		IdleDisconnect: time.Duration(c.Lifecycle.IdleDisconnect) * time.Second,
		IdleLogoff:     time.Duration(c.Lifecycle.IdleLogoff) * time.Second,
		VMSuspend:      time.Duration(c.Lifecycle.VMSuspend) * time.Second,
	}
	if c.Memory != nil {
		policy.Memory = &controller.MemoryPolicy{StartPercent: c.Memory.StartPercent, StopPercent: c.Memory.StopPercent}
		if err := policy.Memory.Validate(); err != nil {
			return adapter.Config{}, err
		}
	}
	return adapter.Config{
		LibvirtSocket: c.Libvirt.Socket, LibvirtURI: c.Libvirt.URI, Domain: c.Lifecycle.VMName,
		AgentEndpoint: net.JoinHostPort(c.Server.Host, strconv.Itoa(c.Server.BackendPort)),
		NiriSocket:    os.Getenv("NIRI_SOCKET"), FreeRDPAppID: c.FreeRDP.AppID, MemoryAlias: c.Lifecycle.MemoryAlias, Policy: policy,
		AgentToken: c.AgentToken,
		Display: adapter.DisplayConfig{
			Binary: c.FreeRDP.Bin, Host: c.Server.Host, User: c.Server.User, Program: c.RemoteApp.DefaultApp,
			ClientHostname: hostname,
			VideoDriver:    c.FreeRDP.VideoDriver, ScaleDesktop: c.FreeRDP.ScaleDesktop, Clipboard: c.FreeRDP.Clipboard,
			IgnoreCertificate: c.FreeRDP.CertIgnore, ExtraArgs: c.FreeRDP.ExtraArgs, Env: c.FreeRDP.Env,
			Log: os.Stderr, Password: c.Password,
		},
	}, nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 33 || s[i] > 126 {
			return false
		}
	}
	return true
}

// The selectors match the existing Rust keyring's service/username attributes.
// Secrets never enter config files, process arguments or controller replies.
func (c Config) Password(ctx context.Context) (string, error) {
	if c.Server.User == "" {
		return "", fmt.Errorf("server.user is required for interactive requests")
	}
	return keyringSecret(ctx, c.Server.Service, c.Server.User, "auth")
}

// AgentToken reads the shared token for both Windows TCP endpoints. The keyring
// service and username are separate from the RDP password entry.
func (c Config) AgentToken(ctx context.Context) (string, error) {
	token, err := keyringSecret(ctx, c.Server.AgentService, AgentTokenUsername, "auth agent")
	if err != nil {
		return "", err
	}
	// The Windows endpoint compares the JSON string form of the token.
	if !utf8.ValidString(token) {
		return "", fmt.Errorf("agent token is not valid UTF-8; re-set it with auth agent set")
	}
	return token, nil
}

// secret-tool writes the stored bytes unchanged to a pipe. Do not trim here:
// trimming would silently turn an unsupported credential into another secret.
func keyringSecret(ctx context.Context, service, username, action string) (string, error) {
	if service == "" {
		return "", fmt.Errorf("keyring service is not configured")
	}
	command := exec.CommandContext(ctx, "secret-tool", "lookup", "service", service, "username", username)
	data, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("keyring lookup failed; use %s set: %w", action, err)
	}
	secret := string(data)
	if secret == "" {
		return "", fmt.Errorf("keyring entry is empty; use %s set", action)
	}
	if strings.ContainsAny(secret, "\r\n") {
		return "", fmt.Errorf("keyring entry contains CR/LF; re-set it with %s set without a trailing newline", action)
	}
	return secret, nil
}
