//go:build !qa_reactor

package reactor

import (
	"context"

	"devicecode-go/services/telemetry"
	"devicecode-go/services/updater"
)

const (
	defaultFirmwareVersion = "0.0.0-dev"
	defaultFirmwareBuild   = "local"
	defaultFirmwareImageID = "img-dev"
)

// FirmwareVersion/FirmwareBuild/FirmwareImageID are linker-stamped values the
// updater publishes via state/self/software. Keep these zero-valued so TinyGo
// applies -ldflags -X overrides; firmwareIdentity supplies dev defaults.
var (
	FirmwareVersion string
	FirmwareBuild   string
	FirmwareImageID string
)

func firmwareIdentity() updater.Identity {
	return updater.Identity{
		Version: firmwareStringOr(FirmwareVersion, defaultFirmwareVersion),
		Build:   firmwareStringOr(FirmwareBuild, defaultFirmwareBuild),
		ImageID: firmwareStringOr(FirmwareImageID, defaultFirmwareImageID),
	}
}

func firmwareStringOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

// childState describes lifecycle owned by the top-level Reactor. It is
// intentionally small: children own their internal state machines; the Reactor
// only starts them, observes unexpected exits, and stops them with its own
// context.
type childState uint8

const (
	childStopped childState = iota
	childRunning
	childFailed
)

type childExit struct {
	name     string
	expected bool
}

type childRuntime struct {
	name   string
	run    func(context.Context)
	cancel context.CancelFunc
	state  childState
}

type childSupervisor struct {
	children []childRuntime
	done     chan childExit
}

func (s *childSupervisor) Add(name string, run func(context.Context)) {
	if name == "" || run == nil {
		return
	}
	s.children = append(s.children, childRuntime{name: name, run: run, state: childStopped})
}

func (s *childSupervisor) StartAll(ctx context.Context) {
	if s.done == nil {
		s.done = make(chan childExit, 4)
	}
	for i := range s.children {
		if s.children[i].state == childRunning {
			continue
		}
		childCtx, cancel := context.WithCancel(ctx)
		s.children[i].cancel = cancel
		s.children[i].state = childRunning
		name := s.children[i].name
		run := s.children[i].run
		done := s.done
		go func() {
			run(childCtx)
			expected := childCtx.Err() != nil
			select {
			case done <- childExit{name: name, expected: expected}:
			default:
			}
		}()
		log.Println("[svc] ", name, " started")
	}
}

func (s *childSupervisor) Done() <-chan childExit {
	if s == nil || s.done == nil {
		return nil
	}
	return s.done
}

func (s *childSupervisor) HandleExit(ev childExit) {
	if s == nil || ev.name == "" {
		return
	}
	for i := range s.children {
		if s.children[i].name != ev.name {
			continue
		}
		if ev.expected {
			s.children[i].state = childStopped
			log.Println("[svc] ", ev.name, " stopped")
		} else {
			s.children[i].state = childFailed
			log.Println("[svc] ", ev.name, " exited unexpectedly")
		}
		return
	}
}

func (s *childSupervisor) StopAll() {
	if s == nil {
		return
	}
	for i := range s.children {
		if s.children[i].cancel != nil {
			s.children[i].cancel()
		}
	}
}

func (r *Reactor) startCoreChildren(ctx context.Context) {
	if r == nil || r.uiConn == nil {
		return
	}

	// Updater publishes retained state/self/{software,updater,health} facts and
	// binds the local updater RPC topics. The default firmware build still keeps
	// Fabric staging safe-disabled at the Reactor boundary; fabric_uart_hwtest or
	// fabric_stage_enabled explicitly opt into using this service as Fabric
	// StageController.
	updater.GenerateBootID()
	updaterConn := r.uiConn.NewChildConnection("updater")
	if updaterConn != nil {
		updaterSvc := updater.New(updaterServiceOptions(updaterConn))
		log.Println("[updater] policy ", updaterRuntimeMode())
		r.updaterSvc = updaterSvc
		r.children.Add("updater", updaterSvc.Run)
	}

	telemetryConn := r.uiConn.NewChildConnection("telemetry")
	if telemetryConn != nil {
		telemetrySvc := telemetry.New(telemetryConn)
		r.children.Add("telemetry", telemetrySvc.Run)
	}
	r.addFabricSelfTestChild()
	r.children.StartAll(ctx)
}

func (r *Reactor) stopCoreChildren() {
	if r == nil {
		return
	}
	r.children.StopAll()
	r.updaterSvc = nil
}
