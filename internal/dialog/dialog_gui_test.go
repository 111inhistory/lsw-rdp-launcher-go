package dialog

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
	"github.com/egoist/mygo/ui"
)

// The toolkit must own the process main thread and can run only once. Each
// choice therefore uses its own subprocess, never a real Windows controller.
func TestMain(m *testing.M) {
	if choice := controller.SwitchDecision(os.Getenv("_RDP_DIALOG_GUI_CHOICE")); choice != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		d := &dialog{prompt: controller.SwitchPrompt{
			TargetMode: controller.ModeRemoteApp,
			Allowed:    []controller.SwitchDecision{choice},
		}, decision: controller.SwitchCancel}
		got, err := d.run(ctx, ui.View(func(c *ui.Context) {
			d.view(c)
			// This runs in the same native draw callback as a clicked action. Closing
			// the window synchronously here used to SIGSEGV before IPC could reply.
			d.choose(choice)
		}))
		if err != nil || got != choice {
			fmt.Fprintf(os.Stderr, "choice = %q, error = %v; want %q\n", got, err, choice)
			os.Exit(1)
		}
		return
	}
	os.Exit(m.Run())
}

func TestChoiceSurvivesNativeDrawCallback(t *testing.T) {
	if os.Getenv("RDP_TEST_GUI") != "1" {
		t.Skip("set RDP_TEST_GUI=1 in a graphical session; no VM is contacted")
	}
	for _, choice := range []controller.SwitchDecision{controller.SwitchKeep, controller.SwitchLogoff, controller.SwitchCancel} {
		t.Run(string(choice), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(), "_RDP_DIALOG_GUI_CHOICE="+string(choice))
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("GUI choice subprocess: %v\n%s", err, output)
			}
		})
	}
}
