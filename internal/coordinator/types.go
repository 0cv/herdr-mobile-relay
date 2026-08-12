package coordinator

import (
	"context"
	"errors"
	"time"
)

type CommandID uint64
type OperationID uint64
type ClientID string

type CommandKind string

const (
	CommandPrompt    CommandKind = "prompt"
	CommandKeys      CommandKind = "keys"
	CommandText      CommandKind = "text"
	CommandApproval  CommandKind = "approval"
	CommandQuestion  CommandKind = "question"
	CommandTabRename CommandKind = "agent_rename"
	CommandStop      CommandKind = "agent_stop"
	CommandClear     CommandKind = "agent_clear"
	CommandStart     CommandKind = "agent_start"
)

type Command struct {
	ID         CommandID
	ClientID   ClientID
	RequestID  string
	ReceivedAt time.Time
	Deadline   time.Time
	Kind       CommandKind
	PaneID     string
	Payload    any
}

type WorkerToken struct {
	OperationID OperationID
	PaneID      string
	Generation  uint64
	AllowAbsent bool
	Revision    uint64
	Deadline    time.Time
}

type EffectKind string

const (
	EffectPrompt   EffectKind = "prompt"
	EffectKeys     EffectKind = "keys"
	EffectText     EffectKind = "text"
	EffectApproval EffectKind = "approval"
	EffectQuestion EffectKind = "question"
	EffectRename   EffectKind = "rename"
	EffectStop     EffectKind = "stop"
	EffectClear    EffectKind = "clear"
	EffectStart    EffectKind = "start"
)

type EffectInput interface {
	effectInput()
}

type Effect struct {
	Token WorkerToken
	Kind  EffectKind
	Input EffectInput
}

type WorkerEvent interface {
	workerEvent()
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
	PaneGeneration uint64
	AllowAbsent    bool
	LedgerKey      string
	PayloadHash    string
}
