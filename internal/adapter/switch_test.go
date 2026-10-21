package adapter

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

func TestSwitchDialogProtocolAndFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native-dialog")
	for _, tc := range []struct {
		body string
		want controller.SwitchDecision
		fail bool
	}{
		{"printf '\"keep\"\\n'", controller.SwitchKeep, false},
		{"printf '\"cancel\"\\n'", controller.SwitchCancel, false},
		{"printf '\"logoff\"\\n'", controller.SwitchLogoff, false},
		{"printf '\"anything\"\\n'", "", true},
		{"exit 1", "", true},
	} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\ncat >/dev/null\n"+tc.body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		decision, err := chooseSwitch(context.Background(), path, controller.SwitchPrompt{TargetMode: "desktop"})
		if (err != nil) != tc.fail || decision != tc.want {
			t.Fatal(decision, err)
		}
	}
}
