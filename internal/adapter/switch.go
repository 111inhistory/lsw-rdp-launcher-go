package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

func displayMode(need controller.Requirement) string {
	if need == controller.Desktop {
		return "desktop"
	}
	return "remoteapp"
}
func chooseSwitch(ctx context.Context, binary string, prompt controller.SwitchPrompt) (controller.SwitchDecision, error) {
	data, err := json.Marshal(prompt)
	if err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, binary)
	command.Stdin = bytes.NewReader(data)
	command.Stderr = os.Stderr
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("native switch dialog: %w", err)
	}
	var decision controller.SwitchDecision
	if err = json.Unmarshal(output, &decision); err != nil {
		return "", fmt.Errorf("native switch dialog response: %w", err)
	}
	switch decision {
	case controller.SwitchCancel, controller.SwitchKeep, controller.SwitchLogoff:
		return decision, nil
	default:
		return "", fmt.Errorf("native switch dialog returned invalid decision %q", decision)
	}
}
