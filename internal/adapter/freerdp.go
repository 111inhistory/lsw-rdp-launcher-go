package adapter

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

type DisplayConfig struct {
	Binary, Host, User, Program  string
	VideoDriver                  string
	ScaleDesktop                 int
	Clipboard, IgnoreCertificate bool
	ExtraArgs                    []string
	// AgentTokenFile is the Windows-side path of the shared agent token file.
	// Only the path enters the RemoteApp command line; the token itself stays
	// in the keyring and file. Empty keeps the explicit loopback-only mode.
	AgentTokenFile string
	Log            io.Writer
	Password       func(context.Context) (string, error)
}

func (c DisplayConfig) args(endpoint string, desktop bool) ([]string, error) {
	host := c.Host
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	args := []string{"/v:" + host, "/u:" + c.User, "/from-stdin:force"}
	if desktop {
		args = append(args, "/f", "+dynamic-resolution")
	} else {
		if strings.ContainsAny(c.Program, ",\"\r\n") {
			return nil, fmt.Errorf("RemoteApp program contains unsupported delimiters")
		}
		command := "daemon --bind " + endpoint
		if c.AgentTokenFile != "" {
			// FreeRDP starts the interactive daemon on Windows. Pass the token
			// file path (never the token) so that daemon enforces the same auth.
			if strings.ContainsAny(c.AgentTokenFile, ",\"") || strings.IndexFunc(c.AgentTokenFile, unicode.IsSpace) >= 0 {
				return nil, fmt.Errorf("agent token file path contains whitespace or unsupported delimiters")
			}
			// Do not nest Windows quotes inside FreeRDP's quoted /app value.
			command += " --auth-token-file " + c.AgentTokenFile
		}
		app := fmt.Sprintf("\"program:%s,cmd:%s\"", c.Program, command)
		args = append(args, "/app:"+app)
	}
	if c.Clipboard {
		args = append(args, "+clipboard")
	}
	if c.IgnoreCertificate {
		args = append(args, "/cert:ignore")
	}
	if c.ScaleDesktop > 0 {
		args = append(args, "/scale-desktop:"+strconv.Itoa(c.ScaleDesktop))
	}
	return append(args, c.ExtraArgs...), nil
}

// Display's process fields are owned by the single operation executor. Its
// watcher owns Wait and reports an exit; it does not mutate these fields.
type Display struct {
	config  DisplayConfig
	agent   Agent // Interactive endpoint; may differ from the background endpoint.
	root    context.Context
	emit    Sink
	command *exec.Cmd
	exited  <-chan struct{}
}

func NewDisplay(ctx context.Context, config DisplayConfig, interactive Agent, emit Sink) *Display {
	return &Display{config: config, agent: interactive, root: ctx, emit: emit}
}
func (d *Display) Start(ctx context.Context, id uint64, desktop bool) error {
	if d.command != nil {
		select {
		case <-d.exited:
			d.command = nil
		default:
			return fmt.Errorf("owned FreeRDP process is already running")
		}
	}
	// daemon --bind needs a numeric guest address, as in the Rust launcher.
	if !desktop {
		if _, err := netip.ParseAddrPort(d.agent.Endpoint); err != nil {
			return fmt.Errorf("interactive endpoint must be a numeric guest IP:port")
		}
	}
	args, err := d.config.args(d.agent.Endpoint, desktop)
	if err != nil {
		return err
	}
	if d.config.Password == nil {
		return fmt.Errorf("FreeRDP password provider is not configured")
	}
	password, err := d.config.Password(ctx)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	probeAgent := d.agent
	if !desktop && d.agent.AuthToken != nil {
		// Read the shared token once for this startup probe loop instead of
		// invoking the keyring provider every 250 ms.
		token, tokenErr := d.agent.token(ctx)
		if tokenErr != nil {
			return tokenErr
		}
		probeAgent.AuthToken = func(context.Context) (string, error) { return token, nil }
	}
	// The resident controller owns the connection, not the requesting CLI.
	// Canceling a completed request must not disconnect its FreeRDP process.
	command := exec.CommandContext(d.root, d.config.Binary, args...)
	command.Stdout, command.Stderr = d.config.Log, d.config.Log
	// Credentials are sent through stdin, not argv or environment variables.
	command.Stdin = strings.NewReader(password + "\n")
	command.Env = displayEnvironment(d.config.VideoDriver)
	if err = command.Start(); err != nil {
		return err
	}
	exited := make(chan struct{})
	d.command, d.exited = command, exited
	go func() {
		err := command.Wait()
		close(exited)
		_ = d.emit(d.root, controller.DisplayExited{OperationID: id, Err: err})
	}()
	// Desktop does not bootstrap or depend on the interactive agent. Success
	// acknowledges process creation, not RDP authentication or a painted desktop.
	if desktop {
		return nil
	}
	// WORKAROUND: the interactive protocol has no ready event. This bounded handshake
	// bridges that protocol; it is not a RAIL-window appearance guarantee.
	// A preserved agent in a disconnected session must not make a different
	// newly-created connection look ready. Replace polling when the protocol
	// supplies an activation event; this is not steady-state window polling.
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		probeCtx, cancelProbe := context.WithTimeout(readyCtx, 500*time.Millisecond)
		response, probeErr := probeAgent.Call(probeCtx, controller.Request{Action: "status"})
		err = probeErr
		cancelProbe()
		if err == nil && response.SessionActive == nil {
			_ = command.Process.Kill()
			<-exited
			return fmt.Errorf("interactive agent does not report session activation; update the Windows agent")
		}
		if err == nil && response.SessionID != nil && *response.SessionID != 0 && *response.SessionActive {
			select {
			case <-exited:
				return fmt.Errorf("FreeRDP exited during startup")
			default:
				return nil
			}
		}
		select {
		case <-readyCtx.Done():
			_ = command.Process.Kill()
			<-exited
			return readyCtx.Err()
		case <-exited:
			return fmt.Errorf("FreeRDP exited during startup")
		case <-ticker.C:
		}
	}
}
func (d *Display) Stop(ctx context.Context) error {
	if d.command == nil {
		return nil
	}
	select {
	case <-d.exited:
		d.command = nil
		return nil
	default:
	}
	if err := d.command.Process.Signal(syscall.SIGTERM); err != nil && err != os.ErrProcessDone {
		return err
	}
	select {
	case <-d.exited:
		d.command = nil
		return nil
	case <-ctx.Done():
		_ = d.command.Process.Kill()
		<-d.exited
		d.command = nil
		return ctx.Err()
	}
}
func displayEnvironment(driver string) []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToLower(key) {
		case "http_proxy", "https_proxy", "all_proxy", "no_proxy":
			continue
		}
		env = append(env, entry)
	}
	if driver != "" {
		env = append(env, "SDL_VIDEODRIVER="+driver)
	}
	return env
}
