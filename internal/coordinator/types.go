package coordinator

import (
	"context"
	"errors"
	"time"
)

type CommandID uint64
type OperationID uint64

type Command struct {
	ID         CommandID
	RequestID  string
	ReceivedAt time.Time
	Deadline   time.Time
	Action     string
	PaneID     string
}

type WorkerToken struct {
	OperationID OperationID
	PaneID      string
	Generation  int64
	AllowAbsent bool
	Deadline    time.Time
}

type EffectRunner interface {
	Run(context.Context, WorkerToken) EffectResult
}

type EffectFunc func(context.Context, WorkerToken) EffectResult

func (f EffectFunc) Run(ctx context.Context, token WorkerToken) EffectResult {
	return f(ctx, token)
}

type EffectResult struct {
	Result         *CommandResult
	BumpGeneration bool
	Continuation   *EffectContinuation
}

type EffectContinuation struct {
	PaneID      string
	Generation  int64
	AllowAbsent bool
	Deadline    time.Time
	Runner      EffectRunner
	Finalize    func(*CommandResult, error) *CommandResult
}

var (
	ErrIngressFull  = errors.New("coordinator ingress is full")
	ErrConflict     = errors.New("a different response was already submitted")
	ErrClosed       = errors.New("coordinator is closed")
	ErrPaneReplaced = errors.New("pane session was replaced")
)

type ScheduleOptions struct {
	Command
	RelayLevel     bool
	PaneGeneration int64
	AllowAbsent    bool
	LedgerKey      string
	PayloadHash    string
}
