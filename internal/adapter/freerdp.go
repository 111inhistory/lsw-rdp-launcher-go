package adapter

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

type DisplayConfig struct {
	Binary, Host, User, Program  string
	ClientHostname               string
	VideoDriver                  string
	ScaleDesktop                 int
	Clipboard, IgnoreCertificate bool
	ExtraArgs                    []string
	Env                          map[string]string
	Log                          io.Writer
	Password                     func(context.Context) (string, error)
}

func (c DisplayConfig) hostname() string {
	if c.ClientHostname == "" {
		return "rdp-launcher"
	}
	return c.ClientHostname
}

func (c DisplayConfig) args(desktop bool) ([]string, error) {
	name := c.hostname()
	if len(name) > 15 {
		return nil, fmt.Errorf("client hostname must fit in 15 ASCII characters")
	}
	for _, b := range []byte(name) {
		if b <= 32 || b >= 127 {
			return nil, fmt.Errorf("client hostname must contain printable non-space ASCII")
		}
	}
	for _, arg := range c.ExtraArgs {
		if strings.HasPrefix(strings.ToLower(arg), "/client-hostname") {
			return nil, fmt.Errorf("set client_hostname instead of overriding it in extra_args")
		}
	}
	host := c.Host
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	args := []string{"/v:" + host, "/u:" + c.User, "/from-stdin:force", "/client-hostname:" + name}
	if desktop {
		args = append(args, "/f", "+dynamic-resolution")
	} else {
		if c.Program == "" || strings.ContainsAny(c.Program, ",\"\r\n") {
			return nil, fmt.Errorf("RemoteApp program is empty or contains unsupported delimiters")
		}
		args = append(args, "/app:"+fmt.Sprintf("\"program:%s,cmd:remoteapp-shell\"", c.Program))
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

// One executor owns process fields; the watcher only waits and emits its generation.
// Readiness is established separately against the background session snapshot.
type Display struct {
	config  DisplayConfig
	root    context.Context
	emit    Sink
	command *exec.Cmd
	exited  <-chan struct{}
}

func NewDisplay(ctx context.Context, config DisplayConfig, emit Sink) *Display {
	return &Display{config: config, root: ctx, emit: emit}
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
	args, err := d.config.args(desktop)
	if err != nil {
		return err
	}
	env, err := displayEnvironment(d.config.VideoDriver, d.config.Env)
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
	command := exec.CommandContext(d.root, d.config.Binary, args...)
	command.Stdout, command.Stderr = d.config.Log, d.config.Log
	command.Stdin = strings.NewReader(password + "\n")
	command.Env = env
	if err = command.Start(); err != nil {
		return err
	}
	exited := make(chan struct{})
	d.command, d.exited = command, exited
	go func() {
		err := command.Wait()
		close(exited)
		if d.emit != nil {
			_ = d.emit(d.root, controller.DisplayExited{OperationID: id, Err: err})
		}
	}()
	return nil
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
func displayEnvironment(driver string, overrides map[string]string) ([]string, error) {
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		switch strings.ToLower(key) {
		case "http_proxy", "https_proxy", "all_proxy", "no_proxy":
			continue
		}
		values[key] = value
	}
	if driver != "" {
		values["SDL_VIDEODRIVER"] = driver
	}
	for key, value := range overrides {
		if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("invalid FreeRDP environment entry: key must be non-empty without = or NUL; values must not contain NUL")
		}
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env, nil
}
