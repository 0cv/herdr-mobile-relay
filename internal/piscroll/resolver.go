package piscroll

import (
	"context"
	"errors"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
	"github.com/0cv/herdr-mobile-relay/internal/pibridge"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

type Target struct {
	Ref   protocol.TargetRef
	Agent string
}

type Evidence struct {
	Instance    string
	Incarnation string
	PID         int
	Renderer    string
	Wheel       string
	Grid        panesize.Grid
}

type Resolver interface {
	Resolve(context.Context, Target, bool) (Evidence, error)
}

type intentContextKey struct{}

func WithIntentID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, intentContextKey{}, id)
}
func IntentID(ctx context.Context) string { id, _ := ctx.Value(intentContextKey{}).(string); return id }

type PhaseMeasurement struct {
	Phase    string
	Duration time.Duration
}

type ResolveMetrics struct {
	Phases           []PhaseMeasurement
	IntentID         string
	Operation        string
	MeasuredGeometry bool
	Duration         time.Duration
	Phase            string
	ProcessReads     int
	BridgeQueries    int
	GeometryReads    int
	Failed           bool
	TerminalPresent  bool
	SessionPresent   bool
	SocketSupported  bool
	TargetCurrent    bool
}

type PiResolver struct {
	Process   func(context.Context, string) (*herdr.PaneProcessInfo, error)
	Bridge    func(context.Context, pibridge.Identity) (pibridge.ScrollResponse, error)
	Catalog   func(context.Context, pibridge.Identity) (pibridge.Response, error)
	Geometry  func(context.Context, int) (panesize.Grid, error)
	Instance  func() (string, error)
	Supported func() bool
	Current   func(Target) bool
	Observe   func(ResolveMetrics)
}

func Foreground(info *herdr.PaneProcessInfo, pane string) (int, error) {
	if info == nil || pane == "" || info.PaneID != pane || info.ShellPID <= 0 ||
		info.ForegroundProcessGroupID <= 0 || info.ForegroundProcessGroupID == info.ShellPID ||
		len(info.ForegroundProcesses) != 1 || info.ForegroundProcesses[0].PID != info.ForegroundProcessGroupID {
		return 0, errors.New("foreground ownership unavailable")
	}
	return info.ForegroundProcessGroupID, nil
}

func IsPi(agent string) bool { return agent == "pi" || agent == "picodingagent" }

func (r *PiResolver) Resolve(ctx context.Context, target Target, measure bool) (Evidence, error) {
	return r.resolve(ctx, target, measure, "scroll_state", func(ctx context.Context, id pibridge.Identity) (Evidence, error) {
		response, err := r.Bridge(ctx, id)
		return Evidence{Incarnation: response.Incarnation, Renderer: response.Renderer, Wheel: response.Wheel}, err
	})
}

func (r *PiResolver) ResolveCatalog(ctx context.Context, target Target) (pibridge.Response, error) {
	var response pibridge.Response
	_, err := r.resolve(ctx, target, false, "catalog", func(ctx context.Context, id pibridge.Identity) (Evidence, error) {
		var err error
		response, err = r.Catalog(ctx, id)
		return Evidence{Incarnation: response.Incarnation}, err
	})
	if err != nil {
		return pibridge.Response{}, err
	}
	return response, nil
}

func (r *PiResolver) resolve(ctx context.Context, target Target, measure bool, operation string, query func(context.Context, pibridge.Identity) (Evidence, error)) (evidence Evidence, resultErr error) {
	started := time.Now()
	metrics := ResolveMetrics{IntentID: IntentID(ctx), Operation: operation, MeasuredGeometry: measure, Phase: "target"}
	phaseStarted := started
	phase := func(name string) {
		metrics.Phases = append(metrics.Phases, PhaseMeasurement{Phase: metrics.Phase, Duration: time.Since(phaseStarted)})
		metrics.Phase, phaseStarted = name, time.Now()
	}
	defer func() {
		metrics.Phases = append(metrics.Phases, PhaseMeasurement{Phase: metrics.Phase, Duration: time.Since(phaseStarted)})
		metrics.Duration = time.Since(started)
		metrics.Failed = resultErr != nil
		if r.Observe != nil {
			r.Observe(metrics)
		}
	}()
	metrics.TerminalPresent = target.Ref.TerminalID != ""
	metrics.SessionPresent = target.Ref.AgentSessionID != ""
	metrics.SocketSupported = r.Supported()
	metrics.TargetCurrent = r.Current(target)
	for _, check := range []struct {
		phase string
		valid bool
	}{
		{"agent", IsPi(target.Agent)}, {"terminal", metrics.TerminalPresent},
		{"session", metrics.SessionPresent}, {"capability", operation == "catalog" || metrics.SocketSupported}, {"target", metrics.TargetCurrent},
	} {
		if !check.valid {
			metrics.Phase = check.phase
			return evidence, errors.New("Pi " + check.phase + " unavailable")
		}
	}
	phase("instance_before")
	instance, err := r.Instance()
	if err != nil {
		return evidence, err
	}
	phase("ownership_before")
	metrics.ProcessReads++
	before, err := r.Process(ctx, target.Ref.PaneID)
	if err != nil {
		return evidence, err
	}
	pid, err := Foreground(before, target.Ref.PaneID)
	if err != nil {
		return evidence, err
	}
	var grid panesize.Grid
	if measure {
		phase("geometry")
		metrics.GeometryReads++
		grid, err = r.Geometry(ctx, pid)
		if err != nil {
			return Evidence{}, err
		}
	}
	phase("bridge")
	metrics.BridgeQueries++
	evidence, err = query(ctx, pibridge.Identity{Instance: instance, Pane: target.Ref.PaneID, Session: target.Ref.AgentSessionID, PID: pid})
	if err != nil {
		return Evidence{}, err
	}
	evidence.Instance, evidence.PID, evidence.Grid = instance, pid, grid
	phase("ownership_after")
	metrics.ProcessReads++
	after, err := r.Process(ctx, target.Ref.PaneID)
	if err != nil {
		return Evidence{}, err
	}
	current, err := Foreground(after, target.Ref.PaneID)
	phase("instance_after")
	currentInstance, instanceErr := r.Instance()
	phase("target_recheck")
	if err != nil || instanceErr != nil || current != pid || currentInstance != instance || operation != "catalog" && !r.Supported() || !r.Current(target) {
		return Evidence{}, errors.New("Pi target replaced")
	}
	if err := ctx.Err(); err != nil {
		return Evidence{}, err
	}
	phase("complete")
	return evidence, nil
}
