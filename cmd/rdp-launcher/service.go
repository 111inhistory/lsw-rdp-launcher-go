package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/host"
)

const hostUnit = "rdp-launcher.service"

func controllerRunning(ctx context.Context, socket string) (bool, error) {
	probe, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, err := host.Call(probe, socket, host.Request{Action: "status"})
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return false, nil
	}
	return false, fmt.Errorf("existing controller did not answer; not starting a duplicate: %w", err)
}

func startHost(ctx context.Context, config, socket string, out io.Writer) error {
	running, err := controllerRunning(ctx, socket)
	if err != nil {
		return err
	}
	if running {
		_, err = fmt.Fprintln(out, "控制器已在运行，保留当前连接和配置。")
		return err
	}
	// Do not restart an active unit using a different socket or still starting.
	active := exec.CommandContext(ctx, "systemctl", "--user", "is-active", "--quiet", hostUnit).Run()
	if err := ctx.Err(); err != nil {
		return err
	}
	if active == nil {
		return fmt.Errorf("user service is active but the selected socket is not ready; inspect systemctl --user status %s", hostUnit)
	}
	var exit *exec.ExitError
	if !errors.As(active, &exit) {
		return fmt.Errorf("check user service: %w", active)
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	config, err = filepath.Abs(config)
	if err != nil {
		return err
	}
	override := hostServiceOverride(executable, config, socket)
	dir, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	dir = filepath.Join(dir, "systemd", "user")
	if err = os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	// Existing units keep their settings; only our dedicated ExecStart drop-in
	// is managed here. start does not enable login autostart or require root.
	file, err := os.OpenFile(filepath.Join(dir, hostUnit), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err == nil {
		_, err = io.WriteString(file, "[Unit]\nDescription=RDP Launcher controller\nPartOf=graphical-session.target\nAfter=graphical-session-pre.target\n\n[Service]\nType=simple\nRestart=on-failure\nRestartSec=5\n\n[Install]\nWantedBy=graphical-session.target\n")
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	dropDir := filepath.Join(dir, hostUnit+".d")
	if err = os.MkdirAll(dropDir, 0755); err != nil {
		return err
	}
	staged, err := os.CreateTemp(dropDir, ".command-*")
	if err != nil {
		return err
	}
	defer os.Remove(staged.Name())
	if _, err = staged.WriteString(override); err != nil {
		staged.Close()
		return err
	}
	if err = staged.Chmod(0644); err != nil {
		staged.Close()
		return err
	}
	if err = staged.Close(); err != nil {
		return err
	}
	if err = os.Rename(staged.Name(), filepath.Join(dropDir, "50-rdp-launcher-command.conf")); err != nil {
		return err
	}
	for _, args := range [][]string{{"--user", "daemon-reload"}, {"--user", "start", hostUnit}} {
		if output, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, output)
		}
	}
	ready, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		running, err = controllerRunning(ready, socket)
		if err != nil {
			return err
		}
		if running {
			_, err = fmt.Fprintln(out, "后台控制器已启动。")
			return err
		}
		select {
		case <-ready.Done():
			return fmt.Errorf("controller startup timed out; inspect journalctl --user -u %s: %w", hostUnit, ready.Err())
		case <-ticker.C:
		}
	}
}

func hostServiceOverride(executable, config, socket string) string {
	quote := func(value string) string {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$", "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(value) + `"`
	}
	return "# Managed by rdp-launcher start. Other service settings are preserved.\n[Service]\nExecStart=\nExecStart=" + quote(executable) + " --config " + quote(config) + " --socket " + quote(socket) + " serve\n"
}
