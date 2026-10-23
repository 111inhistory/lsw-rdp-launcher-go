package adapter

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/111inhistory/lsw-rdp-launcher-go/internal/controller"
	"golang.org/x/sync/errgroup"
)

type Config struct {
	LibvirtSocket, LibvirtURI, Domain string
	NiriSocket, FreeRDPAppID          string
	AgentEndpoint                     string
	AgentToken                        func(context.Context) (string, error)
	Display                           DisplayConfig
	MemoryAlias                       string
	Policy                            controller.Options
}
type Session struct {
	Controller                            *controller.Controller
	vm                                    *Libvirt
	memory                                *MemoryLibvirt
	agent                                 Agent
	display                               *Display
	niriSocket, freeRDPAppID, sessionUser string
	ctx                                   context.Context
	cancel                                context.CancelFunc
}

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
	s := &Session{vm: vm, agent: Agent{Endpoint: cfg.AgentEndpoint, AuthToken: cfg.AgentToken}, niriSocket: cfg.NiriSocket, freeRDPAppID: cfg.FreeRDPAppID, sessionUser: cfg.Display.User, ctx: ctx, cancel: cancel}
	if cfg.Policy.Memory != nil {
		s.memory, err = OpenMemoryLibvirt(ctx, cfg.LibvirtSocket, cfg.LibvirtURI, cfg.Domain, cfg.MemoryAlias)
		if err != nil {
			s.Close()
			return nil, err
		}
	}
	s.Controller = controller.New(cfg.Policy, s.execute)
	s.display = NewDisplay(ctx, cfg.Display, s.Controller.Notify)
	return s, nil
}

func (s *Session) execute(ctx context.Context, op controller.Operation) controller.Outcome {
	if op.Kind == controller.Dispatch {
		return s.dispatch(ctx, op)
	}
	if op.Kind == controller.ConfirmSwitch {
		ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		if op.Request.Choice != "" {
			return controller.Outcome{Decision: op.Request.Choice}
		}
		if op.Request.Confirm == nil {
			return controller.Outcome{Err: fmt.Errorf("request requires a choice from its originating CLI")}
		}
		choice, err := op.Request.Confirm(ctx, op.Prompt)
		return controller.Outcome{Decision: choice, Err: err}
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
		response, err := s.agent.InspectSessions(ctx, s.sessionUser)
		return controller.Outcome{Sessions: response.Snapshot, Err: err}
	case controller.LogoffSession:
		_, err := s.agent.call(ctx, agentRequest{Action: "logoff_session", User: s.sessionUser, SessionRef: &op.Reference})
		return controller.Outcome{Err: err}
	case controller.StartDisplay:
		before, err := s.agent.InspectSessions(ctx, s.sessionUser)
		if err != nil {
			return controller.Outcome{Err: fmt.Errorf("pre-connection snapshot: %w", err)}
		}
		if before.Snapshot == nil || !before.Snapshot.Fresh {
			return controller.Outcome{Err: fmt.Errorf("cannot start connection without a fresh baseline")}
		}
		if err := s.display.Start(ctx, op.ID, op.Request.Mode == controller.ModeDesktop); err != nil {
			return controller.Outcome{Err: err}
		}
		binding, snapshot, err := s.waitBinding(ctx, before.Snapshot)
		if err != nil {
			cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = s.display.Stop(cleanup)
			stop()
		}
		return controller.Outcome{Binding: binding, Sessions: snapshot, Err: err}
	case controller.StopDisplay:
		return controller.Outcome{Err: s.display.Stop(ctx)}
	default:
		return controller.Outcome{Err: fmt.Errorf("unsupported operation %s", op.Kind)}
	}
}

func (s *Session) dispatch(ctx context.Context, op controller.Operation) controller.Outcome {
	if op.Request.Action == "connect" {
		if !op.Reference.Valid() {
			return controller.Outcome{Err: fmt.Errorf("connection is not bound")}
		}
		return controller.Outcome{Data: AgentResponse{Protocol: 2, Status: "ok", Message: "display bound to user logon", SessionRef: &op.Reference}}
	}
	if op.Request.Action == "batch" {
		results := make([]AgentResponse, 0, len(op.Request.Batch))
		for index, command := range op.Request.Batch {
			response, err := s.agent.CallInSession(ctx, command, op.Reference)
			if err != nil {
				return controller.Outcome{Data: map[string]any{"results": results, "completed": len(results), "failed_index": index}, Err: fmt.Errorf("batch stopped after %d of %d commands; failing command outcome may be unknown: %w", len(results), len(op.Request.Batch), err)}
			}
			results = append(results, response)
		}
		return controller.Outcome{Data: results}
	}
	var response AgentResponse
	var err error
	if op.Request.NeedsUser() {
		response, err = s.agent.CallInSession(ctx, op.Request, op.Reference)
	} else {
		response, err = s.agent.Call(ctx, op.Request)
	}
	return controller.Outcome{Data: response, Err: err}
}

func (s *Session) waitBinding(ctx context.Context, before *controller.SessionSnapshot) (*controller.Login, *controller.SessionSnapshot, error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		response, err := s.agent.InspectSessions(ctx, s.sessionUser)
		if err != nil {
			return nil, nil, fmt.Errorf("binding session snapshot: %w", err)
		}
		snapshot := response.Snapshot
		if snapshot != nil && snapshot.Fresh {
			var selected *controller.Login
			for _, login := range snapshot.Sessions {
				if login.State != 0 || !login.Reference.Valid() || login.SID == "" || !strings.EqualFold(login.ClientName, s.display.config.hostname()) {
					continue
				}
				if before != nil {
					old, existed := before.Find(login.Reference)
					if existed && old.ConnectTime == login.ConnectTime {
						continue
					}
				}
				owner := login.Domain + `\` + login.User
				if !strings.EqualFold(login.User, s.sessionUser) && !strings.EqualFold(owner, s.sessionUser) {
					continue
				}
				if selected != nil {
					return nil, snapshot, fmt.Errorf("multiple active logons match the owned client hostname")
				}
				copy := login
				selected = &copy
			}
			if selected != nil {
				status, statusErr := s.agent.SessionStatus(ctx, selected.Reference)
				if statusErr != nil {
					return nil, snapshot, fmt.Errorf("bound user worker unavailable: %w", statusErr)
				}
				if status.SessionRef == nil || *status.SessionRef != selected.Reference || status.Activity == nil || status.Activity.SID != selected.SID || status.SessionActive == nil || !*status.SessionActive {
					return nil, snapshot, fmt.Errorf("worker did not confirm the bound active logon")
				}
				select {
				case <-s.display.exited:
					return nil, snapshot, fmt.Errorf("FreeRDP exited during binding")
				default:
				}
				return selected, snapshot, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, snapshot, fmt.Errorf("connection was not bound: %w", ctx.Err())
		case <-s.display.exited:
			return nil, snapshot, fmt.Errorf("FreeRDP exited before a matching logon was ready")
		case <-ticker.C:
		}
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
func (s *Session) watchBackend(ctx context.Context) error {
	var generations atomic.Uint64
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
						generation := generations.Add(1)
						return s.agent.Watch(ctx, func(ctx context.Context, event controller.Event) error {
							return s.Controller.Notify(ctx, controller.BackendStreamObserved{Generation: generation, Event: event})
						})
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
