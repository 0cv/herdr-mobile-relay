package coordinator

import (
	"context"
	"errors"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

const (
	notSentRetrySafe        = "Command was not sent; retry is safe"
	queueRejectedRetrySafe  = "command was not sent; retry is safe"
	partiallyAppliedMessage = "Part of the command already reached the agent; review it before retrying"
)

type failureKind int

const (
	failureOther failureKind = iota
	failurePartiallyApplied
	failureRefused
	failureCreatedTargetUnknown
	failureDispatchedUnknown
	failureNotStarted
)

type failureClass struct {
	kind      failureKind
	code      string
	transient bool
}

// classifyFailure decides whether a backend error proves the command never ran
// (not_started), may have run (dispatched_unknown) or failed in a way that says
// nothing about it. The order matters: an earlier applied step outranks a
// refusal, and a refusal outranks the started-subprocess marker that comes with
// it.
func classifyFailure(err error) failureClass {
	switch {
	case errors.Is(err, herdr.ErrPartiallyApplied):
		return failureClass{kind: failurePartiallyApplied}
	case herdr.IsRefused(err):
		return failureClass{kind: failureRefused, code: herdr.RefusalCode(err), transient: herdr.IsTransientRefused(err)}
	case errors.Is(err, herdr.ErrCreatedTargetUnknown):
		return failureClass{kind: failureCreatedTargetUnknown}
	case errors.Is(err, herdr.ErrDispatchedUnknown):
		return failureClass{kind: failureDispatchedUnknown}
	case errors.Is(err, herdr.ErrNotStarted), errors.Is(err, context.DeadlineExceeded):
		return failureClass{kind: failureNotStarted}
	default:
		return failureClass{kind: failureOther}
	}
}

func (c failureClass) phase() string {
	switch c.kind {
	case failurePartiallyApplied, failureCreatedTargetUnknown, failureDispatchedUnknown:
		return "dispatched_unknown"
	case failureRefused, failureNotStarted:
		return "not_started"
	default:
		return "failed"
	}
}

func uncertainOutcomeData() map[string]any {
	return map[string]any{"dispatched_unknown": true}
}

func failureSummary(action, message string) string {
	return strings.ReplaceAll(action, "_", " ") + " failed: " + message
}
