package adapter

import (
	"context"
	"fmt"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
	"golang.org/x/sync/errgroup"
)

type Config struct {
	LibvirtSocket, LibvirtURI, Domain  string
	NiriSocket, FreeRDPAppID           string
	AgentEndpoint, InteractiveEndpoint string
	AgentToken                         func(context.Context) (string, error)
	Display                            DisplayConfig
	MemoryAlias                        string
	DialogBinary                       string
	Policy                             controller.Options
}
type Session struct {
	Controller   *controller.Controller
	vm           *Libvirt
	memory       *MemoryLibvirt
	agent        Agent
	display      *Display
	niriSocket   string
	freeRDPAppID string
	sessionUser  string
	dialogBinary string
	ctx          context.Context
	cancel       context.CancelFunc
}

// Open registers libvirt lifecycle events before the controller can issue a VM
// query/control. Configuration is explicit; CLI/TOML and deployment stay outside.
func Open(ctx context.Context, cfg Config) (*Session, error) {
	if cfg.Policy.Memory != nil {
		if err := cfg.Policy.Memory.Validate(); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	vm, err := OpenLibvirt(ctx, cfg.LibvirtSocket, cfg.LibvirtURI, cfg.Domain)
	if err != nil {
		cancel()
		return nil, err
	}
	s := &Session{vm: vm, agent: Agent{Endpoint: cfg.AgentEndpoint, AuthToken: cfg.AgentToken}, niriSocket: cfg.NiriSocket, freeRDPAppID: cfg.FreeRDPAppID, ctx: ctx, cancel: cancel}
	if cfg.Policy.Memory != nil {
		s.memory, err = OpenMemoryLibvirt(ctx, cfg.LibvirtSocket, cfg.LibvirtURI, cfg.Domain, cfg.MemoryAlias)
		if err != nil {
			s.Close()
			return nil, err
		}
	}
	s.sessionUser, s.dialogBinary = cfg.Display.User, cfg.DialogBinary
	if s.dialogBinary == "" {
		s.dialogBinary = "rdp-launcher-dialog"
	}
	s.Controller = controller.New(cfg.Policy, s.execute)
	// Background and interactive roles must not silently share a port.
	// Backend-only sessions may leave the interactive endpoint unset.
	s.display = NewDisplay(ctx, cfg.Display, Agent{Endpoint: cfg.InteractiveEndpoint, AuthToken: cfg.AgentToken}, s.Controller.Notify)
	return s, nil
}
func (s *Session) execute(ctx context.Context, op controller.Operation) controller.Outcome {
	if op.Kind == controller.Dispatch {
		if op.Request.Need == controller.Desktop || op.Request.Action == "connect" {
			return controller.Outcome{Data: AgentResponse{Status: "ok", Message: "display process started"}}
		}
		client := s.agent
		if op.Request.Need == controller.Interactive {
			client = s.display.agent
		}
		response, err := client.Call(ctx, op.Request)
		return controller.Outcome{Data: response, Err: err}
	}
	if op.Kind == controller.ConfirmSwitch {
		decision, err := chooseSwitch(ctx, s.dialogBinary, controller.SwitchPrompt{Activity: op.Activity, TargetMode: displayMode(op.Request.Need)})
		return controller.Outcome{Decision: decision, Err: err}
	}
	timeout := 15 * time.Second
	if op.Kind == controller.StartDisplay {
		timeout = 90 * time.Second
	}
	if op.Kind == controller.InspectSession || op.Kind == controller.LogoffSession {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	switch op.Kind {
	case controller.QueryVM, controller.SuspendVM:
		return s.vm.Execute(ctx, op)
	case controller.WakeVM:
		prepared := op.MemoryPrepared
		if s.memory != nil && op.VM == controller.VMPaused && !op.ResumeSafe {
			preparation := s.memory.Execute(ctx, controller.Operation{Kind: controller.PrepareMemory})
			if preparation.Err != nil {
				preparation.Err = fmt.Errorf("paused VM memory preparation failed; VM not resumed; recovery: docs/memory.md#recovery: %w", preparation.Err)
				return preparation
			}
			prepared = preparation.MemoryPrepared
		}
		out := s.vm.Execute(ctx, op)
		out.MemoryPrepared = prepared
		return out
	case controller.QueryMemory, controller.SetMemory, controller.PrepareMemory:
		if s.memory == nil {
			return controller.Outcome{Err: fmt.Errorf("memory control disabled")}
		}
		return s.memory.Execute(ctx, op)
	case controller.InspectSession:
		response, err := s.agent.call(ctx, agentRequest{Action: "inspect_session", User: s.sessionUser})
		return controller.Outcome{Activity: response.Activity, Err: err}
	case controller.LogoffSession:
		_, err := s.agent.call(ctx, agentRequest{Action: "logoff_session", User: s.sessionUser, SessionID: op.Activity.SessionID, Fingerprint: op.Activity.Fingerprint, RequireEmpty: op.RequireEmpty})
		return controller.Outcome{Err: err}
	case controller.StartDisplay:
		return controller.Outcome{Err: s.display.Start(ctx, op.ID, op.Request.Need == controller.Desktop)}
	case controller.StopDisplay:
		return controller.Outcome{Err: s.display.Stop(ctx)}
	default:
		return controller.Outcome{Err: fmt.Errorf("unsupported operation %s", op.Kind)}
	}
}
func (s *Session) Run() error {
	defer s.Close()
	group, ctx := errgroup.WithContext(s.ctx)
	group.Go(func() error { return s.Controller.Run(ctx) })
	group.Go(func() error { return s.vm.Watch(ctx, s.Controller.Notify) })
	if s.memory != nil {
		group.Go(func() error { return s.memory.Watch(ctx, s.Controller.Notify) })
	}
	if s.niriSocket != "" {
		group.Go(func() error {
			return retrySource(ctx, "niri", func(ctx context.Context) error {
				return WatchNiri(ctx, s.niriSocket, s.freeRDPAppID, s.Controller.Notify)
			})
		})
	}
	group.Go(func() error { return s.watchBackend(ctx) })
	return group.Wait()
}

// A VM transition owns the subscription lifetime. Running opens a new stream
// and gets a fresh snapshot; pause/off closes it. There is no timed polling or
// application heartbeat. Transport loss retries inside that VM-owned lifetime;
// a VM pause/off cancels the retry and a resume starts a fresh subscription.
func (s *Session) watchBackend(ctx context.Context) error {
	var cancel context.CancelFunc
	var ended <-chan error
	stop := func() {
		if cancel != nil {
			cancel()
			<-ended
			cancel = nil
			ended = nil
		}
	}
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case state, ok := <-s.Controller.VMChanges():
			if !ok {
				return ctx.Err()
			}
			stop()
			if state == controller.VMRunning {
				watchCtx, stopWatch := context.WithCancel(ctx)
				cancel = stopWatch
				result := make(chan error, 1)
				ended = result
				go func() {
					defer stopWatch()
					result <- retrySource(watchCtx, "backend", func(ctx context.Context) error {
						return s.agent.Watch(ctx, s.Controller.Notify)
					})
				}()
			}
		case err := <-ended:
			cancel()
			cancel = nil
			ended = nil
			return err
		}
	}
}

func (s *Session) Close() {
	s.cancel()
	s.vm.Close()
	if s.memory != nil {
		s.memory.Close()
	}
}
