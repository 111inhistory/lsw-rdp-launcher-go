package adapter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
)

func confirmOperation(req controller.Request, prompt controller.SwitchPrompt) controller.Operation {
	return controller.Operation{Kind: controller.ConfirmSwitch, Request: req, Prompt: prompt}
}

func TestConfirmSwitchUsesExplicitChoiceWithoutCallback(t *testing.T) {
	s := &Session{}
	out := s.execute(context.Background(), confirmOperation(controller.Request{
		Choice: controller.SwitchKeep,
		Confirm: func(context.Context, controller.SwitchPrompt) (controller.SwitchDecision, error) {
			t.Fatal("explicit choice invoked the CLI callback")
			return "", nil
		},
	}, controller.SwitchPrompt{TargetMode: controller.ModeDesktop}))
	if out.Err != nil || out.Decision != controller.SwitchKeep {
		t.Fatal(out)
	}
}

func TestConfirmSwitchInvokesOriginatingRequestCallbackWithPromptAndDeadline(t *testing.T) {
	prompt := controller.SwitchPrompt{
		Activity:   controller.SessionActivity{SessionID: 4, User: `MACHINE\user`, SID: "S-1-5-21-1"},
		TargetMode: controller.ModeDesktop,
		Allowed:    []controller.SwitchDecision{controller.SwitchCancel, controller.SwitchKeep, controller.SwitchLogoff},
	}
	called := false
	out := (&Session{}).execute(context.Background(), confirmOperation(controller.Request{
		Confirm: func(ctx context.Context, got controller.SwitchPrompt) (controller.SwitchDecision, error) {
			called = true
			if got.TargetMode != prompt.TargetMode || got.Activity.SessionID != prompt.Activity.SessionID || !got.Allows(controller.SwitchLogoff) {
				t.Errorf("callback prompt = %+v", got)
			}
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("confirmation callback has no deadline")
			}
			remaining := time.Until(deadline)
			if remaining <= 50*time.Second || remaining > 61*time.Second {
				t.Fatalf("confirmation deadline = %v", remaining)
			}
			return controller.SwitchLogoff, nil
		},
	}, prompt))
	if !called || out.Err != nil || out.Decision != controller.SwitchLogoff {
		t.Fatal(called, out)
	}
}

func TestConfirmSwitchPropagatesCallbackFailure(t *testing.T) {
	sentinel := errors.New("operator closed the confirmation")
	out := (&Session{}).execute(context.Background(), confirmOperation(controller.Request{
		Confirm: func(context.Context, controller.SwitchPrompt) (controller.SwitchDecision, error) {
			return "", sentinel
		},
	}, controller.SwitchPrompt{TargetMode: controller.ModeRemoteApp}))
	if !errors.Is(out.Err, sentinel) || out.Decision != "" {
		t.Fatal(out)
	}
}

func TestConfirmSwitchWithoutChoiceOrCallbackFails(t *testing.T) {
	out := (&Session{}).execute(context.Background(), confirmOperation(controller.Request{}, controller.SwitchPrompt{TargetMode: controller.ModeDesktop}))
	if out.Err == nil || !strings.Contains(out.Err.Error(), "requires a choice") {
		t.Fatal(out)
	}
}
