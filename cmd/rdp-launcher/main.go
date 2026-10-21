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
	flags := flag.NewFlagSet("rdp-launcher", flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "print version and commit hash without contacting the controller")
	config := flags.String("config", host.DefaultConfigPath(), "TOML configuration")
	socket := flags.String("socket", host.DefaultSocket(), "resident controller's Unix socket")
	timeout := flags.Duration("timeout", 3*time.Minute, "client request timeout; 0 disables it")
	flags.Usage = func() {
		fmt.Fprintln(stderr, `Usage: rdp-launcher [global flags] COMMAND [command flags] [arguments]

  version                   Print version and commit hash; no controller needed
  serve / watcher           Run the single resident controller
  status                    Local controller snapshot; does not wake the VM
  lock / unlock             Disable / enable automatic VM suspension
  shutdown                  Stop the controller and its owned FreeRDP process
  ping                      Backend request; wakes the VM without starting RDP
  desktop / remoteapp        Connect or safely switch the display mode
  run [--params TEXT] [--working-dir PATH] TARGET
  open [--app TARGET] FILE...
  apps                      Query Windows applications plus configured [[apps]]
  sync-apps                 Cache applications, icons and .desktop entries
  list-apps                 List cached and configured applications; no VM wake
  mounts                    Query the interactive user's mapped drives
  path [--to-linux] PATH    Convert using explicit [mounts] mappings
  auth set / check / clear        Manage the RDP password keyring entry
  auth agent set / check / clear  Manage the shared Windows agent token
  show-config                     Print the loaded configuration

Command flags precede positional arguments. Global flags precede COMMAND.`)
		flags.PrintDefaults()
	}
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
	if command == "serve" || command == "watcher" {
		if len(args) != 0 {
			return fmt.Errorf("serve takes no positional arguments")
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
	print := func(data any) error {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(data)
	}
	call := func(request host.Request) (json.RawMessage, error) { return host.Call(ctx, *socket, request) }
	remote := func(request controller.Request) (json.RawMessage, error) {
		return call(host.Request{Action: "request", Request: request})
	}
	switch command {
	case "status", "lock", "unlock", "shutdown":
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
		need := controller.Interactive
		if command == "desktop" {
			need = controller.Desktop
		}
		data, err := call(host.Request{Action: "switch-display", Request: controller.Request{Action: "connect", Need: need}})
		if err != nil {
			return err
		}
		return print(data)
	case "ping", "apps", "mounts", "sync-apps":
		if len(args) != 0 {
			return fmt.Errorf("%s takes no arguments", command)
		}
		request := controller.Request{Action: "ping", Need: controller.BackendOnly}
		if command == "apps" || command == "sync-apps" {
			request = controller.Request{Action: "list_apps", Need: controller.Interactive, WithIcons: command == "sync-apps"}
		}
		if command == "mounts" {
			request = controller.Request{Action: "get_mapped_drives", Need: controller.Interactive}
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
		f := flag.NewFlagSet("run", flag.ContinueOnError)
		f.SetOutput(stderr)
		params := f.String("params", "", "Windows argument string")
		directory := f.String("working-dir", "", "Windows working directory")
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
		data, err := remote(controller.Request{Action: "run", Need: controller.Interactive, Target: app.Target, Params: arguments, WorkingDir: *directory})
		if err != nil {
			return err
		}
		return print(data)
	case "open":
		f := flag.NewFlagSet("open", flag.ContinueOnError)
		f.SetOutput(stderr)
		application := f.String("app", "", "Windows application target or cached name/ID; otherwise use the file association")
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
		var results []json.RawMessage
		for _, file := range files {
			request := controller.Request{Action: "open", Need: controller.Interactive}
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
				request = controller.Request{Action: "run", Need: controller.Interactive, Target: app.Target, Params: params, WorkingDir: app.WorkingDir}
			}
			result, err := remote(request)
			if err != nil {
				return err
			}
			results = append(results, result)
		}
		return print(results)
	case "path":
		f := flag.NewFlagSet("path", flag.ContinueOnError)
		f.SetOutput(stderr)
		toLinux := f.Bool("to-linux", false, "convert Windows drive path to host path")
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
