package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/adapter"
	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
	"github.com/111inhistory/lsw-rdp-launcher-go/internal/host"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func auth(ctx context.Context, c host.Config, args []string, in io.Reader, out, stderr io.Writer) error {
	kind := "password"
	if len(args) == 2 && args[0] == "agent" {
		kind, args = "agent", args[1:]
	}
	if len(args) != 1 {
		return fmt.Errorf("auth requires set, check or clear (use auth agent ... for the agent token)")
	}
	var service, username, label, description string
	var check func(context.Context) (string, error)
	if kind == "agent" {
		service, username = c.Server.AgentService, host.AgentTokenUsername
		label, description, check = "RDP Launcher agent token", "Agent token", c.AgentToken
	} else {
		if c.Server.User == "" {
			return fmt.Errorf("server.user is required")
		}
		service, username = c.Server.Service, c.Server.User
		label, description, check = "RDP Launcher ("+c.Server.User+")", "RDP password", c.Password
	}
	if service == "" {
		return fmt.Errorf("keyring service is not configured")
	}
	switch args[0] {
	case "check":
		if _, err := check(ctx); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, description+" keyring entry exists")
		return err
	case "set", "clear":
		toolAction := args[0]
		if toolAction == "set" {
			toolAction = "store"
		}
		toolArgs := []string{toolAction}
		if toolAction == "store" {
			toolArgs = append(toolArgs, "--label="+label)
		}
		toolArgs = append(toolArgs, "service", service, "username", username)
		tool := exec.CommandContext(ctx, "secret-tool", toolArgs...)
		tool.Stdout, tool.Stderr = out, stderr
		if args[0] == "set" {
			input, err := validateSecretInput(in)
			if err != nil {
				return err
			}
			tool.Stdin = input
		} else {
			tool.Stdin = in
		}
		return tool.Run()
	default:
		return fmt.Errorf("auth requires set, check or clear")
	}
}

// A pipe is validated before secret-tool stores it. On a terminal the tool's
// own hidden prompt is preserved; retrieval still rejects CR/LF from either path.
func validateSecretInput(in io.Reader) (io.Reader, error) {
	if file, ok := in.(*os.File); ok {
		if info, err := file.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			return in, nil
		}
	}
	data, err := io.ReadAll(in)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("secret is empty; provide one line without a trailing newline")
	}
	if strings.ContainsAny(string(data), "\r\n") {
		return nil, fmt.Errorf("secret contains CR/LF; use printf %%s or auth set without a trailing newline")
	}
	return bytes.NewReader(data), nil
}

func run(ctx context.Context, args []string, out, stderr io.Writer) error {
	return runWith(ctx, args, out, stderr, newConfirmFrontend(os.Stdin, stderr))
}

func runWith(ctx context.Context, args []string, out, stderr io.Writer, frontend confirmFrontend) error {
	flags := flag.NewFlagSet("rdp-launcher", flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "Print version and commit; no controller needed")
	config := flags.String("config", host.DefaultConfigPath(), "Read TOML configuration from `FILE`")
	socket := flags.String("socket", host.DefaultSocket(), "Connect to controller Unix socket at `PATH`")
	timeout := flags.Duration("timeout", 3*time.Minute, "Client request timeout `DURATION`; 0 disables it")
	onConflict := flags.String("on-conflict", "", "Choose `ACTION` without prompting: current, keep, logoff, cancel")
	flags.Usage = func() { printRootHelp(stderr, flags) }
	if err := flags.Parse(args); err != nil {
		return err
	}
	args = flags.Args()
	if *showVersion || (len(args) > 0 && args[0] == "version") {
		expectedArgs := 1
		if *showVersion {
			expectedArgs = 0
		}
		if len(args) != expectedArgs {
			return fmt.Errorf("version takes no arguments or commands")
		}
		_, err := fmt.Fprintln(out, versionString())
		return err
	}
	var err error
	*socket, err = filepath.Abs(*socket)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		flags.Usage()
		return flag.ErrHelp
	}
	command, args := args[0], args[1:]
	if command == "help" {
		if len(args) != 0 {
			return fmt.Errorf("help takes no arguments")
		}
		flags.Usage()
		return nil
	}
	if command == "serve" || command == "watcher" {
		if len(args) != 0 {
			return fmt.Errorf("serve takes no positional arguments")
		}
		if *onConflict != "" {
			return fmt.Errorf("--on-conflict is not used by %s", command)
		}
		c, err := host.LoadConfig(*config)
		if err != nil {
			return err
		}
		return host.Run(ctx, c, *socket)
	}
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	choice, err := parseOnConflict(*onConflict)
	if err != nil {
		return err
	}
	switch command {
	case "desktop", "remoteapp", "apps", "mounts", "sync-apps", "run", "open":
	default:
		if choice != "" {
			return fmt.Errorf("--on-conflict is not used by %s", command)
		}
	}
	print := func(data any) error {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(data)
	}
	printRaw := func(data json.RawMessage) error {
		var value any
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
		return print(value)
	}
	call := func(request host.Request) (json.RawMessage, error) { return host.Call(ctx, *socket, request) }
	remote := func(request controller.Request) (json.RawMessage, error) {
		request.Choice = choice
		return host.CallWithConfirm(ctx, *socket, host.Request{Action: "request", Request: request}, frontend.choose)
	}
	switch command {
	case "start":
		if len(args) != 0 {
			return fmt.Errorf("start takes no arguments")
		}
		return startHost(ctx, *config, *socket, out)
	case "status":
		f := commandFlags("status", "status", "Show controller, connection and memory status without waking the VM.", stderr)
		if err := f.Parse(args); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return fmt.Errorf("status takes no positional arguments")
		}
		data, err := call(host.Request{Action: "status"})
		if err != nil {
			fmt.Fprintln(out, versionString())
			return err
		}
		return printStatus(out, data)
	case "lock", "unlock", "shutdown":
		if len(args) != 0 {
			return fmt.Errorf("%s takes no arguments", command)
		}
		data, err := call(host.Request{Action: command})
		if err != nil {
			return err
		}
		return print(data)
	case "desktop", "remoteapp":
		if len(args) != 0 {
			return fmt.Errorf("%s takes no arguments", command)
		}
		mode := controller.ModeRemoteApp
		if command == "desktop" {
			mode = controller.ModeDesktop
		}
		data, err := remote(controller.Request{Action: "connect", UserContext: true, Mode: mode})
		if err != nil {
			return err
		}
		return print(data)
	case "ping", "apps", "mounts", "sync-apps":
		if len(args) != 0 {
			return fmt.Errorf("%s takes no arguments", command)
		}
		request := controller.Request{Action: "ping"}
		if command == "apps" || command == "sync-apps" {
			request = controller.Request{Action: "list_apps", UserContext: true, WithIcons: command == "sync-apps"}
		}
		if command == "mounts" {
			request = controller.Request{Action: "get_mapped_drives", UserContext: true}
		}
		data, err := remote(request)
		if err != nil {
			return err
		}
		if command == "apps" || command == "sync-apps" {
			var response adapter.AgentResponse
			if err = json.Unmarshal(data, &response); err != nil {
				return err
			}
			c, err := host.LoadConfig(*config)
			if err != nil {
				return err
			}
			if command == "apps" {
				response.Apps = c.Catalog(response.Apps)
				return print(response)
			}
			executable, err := os.Executable()
			if err != nil {
				return err
			}
			path, err := filepath.Abs(*config)
			if err != nil {
				return err
			}
			if err = c.SyncApps(response.Apps, executable, path, *socket); err != nil {
				return err
			}
			return print(map[string]int{"applications": len(c.Catalog(response.Apps))})
		}
		return print(data)
	case "install-shortcuts":
		if len(args) != 0 {
			return fmt.Errorf("install-shortcuts takes no arguments")
		}
		c, err := host.LoadConfig(*config)
		if err != nil {
			return err
		}
		apps, err := host.ReadApps()
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		path, err := filepath.Abs(*config)
		if err != nil {
			return err
		}
		if err = c.SyncApps(apps, executable, path, *socket); err != nil {
			return err
		}
		return print(map[string]string{"status": "ok", "desktop": "Windows 桌面"})
	case "list-apps":
		c, err := host.LoadConfig(*config)
		if err != nil {
			return err
		}
		apps, err := c.ListApps()
		if err != nil {
			return err
		}
		return print(apps)
	case "run":
		f := commandFlags("run", "run [options] TARGET", "Start a Windows application by path, command or cached name/ID.\nAn existing desktop connection requires a choice before execution.", stderr)
		params := f.String("params", "", "Pass Windows argument string `TEXT`")
		directory := f.String("working-dir", "", "Use Windows working directory `PATH`")
		if err := f.Parse(args); err != nil {
			return err
		}
		if f.NArg() != 1 {
			return fmt.Errorf("run requires one application target or cached application name/ID")
		}
		c, err := host.LoadConfig(*config)
		if err != nil {
			return err
		}
		app := c.ResolveApp(f.Arg(0))
		arguments := app.Arguments
		if *params != "" {
			arguments = strings.TrimSpace(arguments + " " + *params)
		}
		if *directory == "" {
			*directory = app.WorkingDir
		}
		data, err := remote(controller.Request{Action: "run", UserContext: true, Mode: controller.ModeRemoteApp, Target: app.Target, Params: arguments, WorkingDir: *directory})
		if err != nil {
			return err
		}
		return print(data)
	case "open":
		f := commandFlags("open", "open [options] [FILE...]", "Open files in order, with one confirmation for the whole batch.\nWithout --app, use file associations. With --app, files are optional.", stderr)
		application := f.String("app", "", "Use application `TARGET` (path, command or cached name/ID)")
		if err := f.Parse(args); err != nil {
			return err
		}
		if f.NArg() == 0 && *application == "" {
			return fmt.Errorf("open requires files or --app")
		}
		c, err := host.LoadConfig(*config)
		if err != nil {
			return err
		}
		app := c.ResolveApp(*application)
		files := f.Args()
		if len(files) == 0 {
			files = []string{""}
		}
		var batch []controller.Request
		for _, file := range files {
			request := controller.Request{Action: "open"}
			if file != "" {
				request.File, err = c.WindowsPath(file)
				if err != nil {
					return err
				}
			}
			if *application != "" {
				params := app.Arguments
				if request.File != "" {
					if strings.ContainsAny(request.File, "\"\r\n") {
						return fmt.Errorf("file path cannot be represented as a Windows argument")
					}
					trailing := len(request.File) - len(strings.TrimRight(request.File, `\`))
					params = strings.TrimSpace(params + ` "` + request.File + strings.Repeat(`\`, trailing) + `"`)
				}
				request = controller.Request{Action: "run", Target: app.Target, Params: params, WorkingDir: app.WorkingDir}
			}
			batch = append(batch, request)
		}
		// Multiple files are one FIFO job and one confirmation. The outer request
		// carries the only mode and choice; items keep their original business payload.
		request := batch[0]
		if len(batch) > 1 {
			request = controller.Request{Action: "batch", UserContext: true, Mode: controller.ModeRemoteApp, Batch: batch}
		} else {
			request.UserContext = true
			request.Mode = controller.ModeRemoteApp
		}
		data, err := remote(request)
		if len(data) > 0 {
			if printErr := printRaw(data); printErr != nil {
				return printErr
			}
		}
		return err
	case "path":
		f := commandFlags("path", "path [options] PATH", "Convert paths using configured [mounts]; defaults to Linux -> Windows.", stderr)
		toLinux := f.Bool("to-linux", false, "Convert Windows drive path to Linux path instead")
		if err := f.Parse(args); err != nil {
			return err
		}
		if f.NArg() != 1 {
			return fmt.Errorf("path requires one path")
		}
		c, err := host.LoadConfig(*config)
		if err != nil {
			return err
		}
		var path string
		if *toLinux {
			path, err = c.LinuxPath(f.Arg(0))
		} else {
			path, err = c.WindowsPath(f.Arg(0))
		}
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, path)
		return err
	case "show-config":
		c, err := host.LoadConfig(*config)
		if err != nil {
			return err
		}
		return print(c)
	case "auth":
		c, err := host.LoadConfig(*config)
		if err != nil {
			return err
		}
		return auth(ctx, c, args, os.Stdin, out, stderr)
	default:
		return fmt.Errorf("unknown command %q; see --help", command)
	}
}
