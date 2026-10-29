package sqliteexec

import (
	"context"
	"errors"

	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

// ErrCleanupUncertain is public, redacted uncertainty. Raw close identities live
// ONLY in the owner's ledger, below future provider redaction. A2/app must not
// re-collect those identities from query errors when composing the final result.
var ErrCleanupUncertain = errors.New("sqlite cleanup uncertain")

// InstalledExecutor never falls back to an ordinary environment/override owner.
// It implements Executor without changing the ordinary constructors/interface.
type InstalledExecutor struct {
	owner   *conversationlife.Owner
	backend Backend
	binary  string
	deps    installedDeps
}

var _ Executor = (*InstalledExecutor)(nil)

// Private per-instance seams are complete before resolution. Incomplete seams
// deny acquisition; they never inherit production defaults field by field.
type installedDeps struct {
	nativeReady func(context.Context) bool
	lookup      func(string) (string, error)
	open        func(string) (installedDB, error)
	run         func(context.Context, string, string, string, int) ([]byte, error)
}

// NewInstalled binds an explicit selection to exactly owner before any lookup.
// No environment, transport or late installed flag grants this authority.
func NewInstalled(ctx context.Context, owner *conversationlife.Owner, backend Backend, binary string) (*InstalledExecutor, error) {
	return resolveInstalled(ctx, owner, backend, binary, productionInstalledDeps())
}

func resolveInstalled(ctx context.Context, owner *conversationlife.Owner, backend Backend, binary string, deps installedDeps) (*InstalledExecutor, error) {
	var result *InstalledExecutor
	err := owner.Do(ctx, func(call *conversationlife.Call) error {
		if deps.nativeReady == nil || deps.lookup == nil || deps.open == nil || deps.run == nil {
			return ErrUnavailable
		}
		if err := call.Context().Err(); err != nil {
			return err
		}
		if binary == "" {
			binary = "sqlite3"
		}
		selected := ParseBackend(string(backend))
		if selected == BackendAuto {
			if deps.nativeReady(call.Context()) {
				selected = BackendNative
			} else {
				selected = BackendCLI
			}
		}
		if selected == BackendCLI {
			if _, err := deps.lookup(binary); err != nil {
				return errors.Join(ErrUnavailable, err)
			}
		}
		if err := call.Context().Err(); err != nil {
			return err
		}
		result = &InstalledExecutor{owner: owner, backend: selected, binary: binary, deps: deps}
		return nil
	})
	return result, err
}

func (e *InstalledExecutor) OwnedBy(owner *conversationlife.Owner) bool {
	return e != nil && owner != nil && e.owner == owner
}
func (e *InstalledExecutor) Ready() bool {
	if e == nil {
		return false
	}
	ready := false
	err := e.owner.Do(context.Background(), func(c *conversationlife.Call) error { ready = e.ready(c.Context()); return nil })
	return err == nil && ready
}
func (e *InstalledExecutor) ReadyIn(parent *conversationlife.Call) bool {
	if e == nil {
		return false
	}
	ready := false
	err := e.owner.DoIn(parent, func(c *conversationlife.Call) error { ready = e.ready(c.Context()); return nil })
	return err == nil && ready
}
func (e *InstalledExecutor) ready(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}
	if e.backend == BackendNative {
		return e.deps.nativeReady(ctx) && ctx.Err() == nil
	}
	_, err := e.deps.lookup(e.binary)
	return err == nil && ctx.Err() == nil
}

func (e *InstalledExecutor) QueryJSON(ctx context.Context, database, query string, maxBytes int) ([]byte, error) {
	if e == nil {
		return nil, conversationlife.ErrRefused
	}
	var data []byte
	err := e.owner.Do(ctx, func(c *conversationlife.Call) (err error) {
		data, err = e.query(c.Context(), database, query, maxBytes)
		return err
	})
	return data, err
}
func (e *InstalledExecutor) QueryJSONIn(parent *conversationlife.Call, database, query string, maxBytes int) ([]byte, error) {
	if e == nil {
		return nil, conversationlife.ErrRefused
	}
	var data []byte
	err := e.owner.DoIn(parent, func(c *conversationlife.Call) (err error) {
		data, err = e.query(c.Context(), database, query, maxBytes)
		return err
	})
	return data, err
}
func (e *InstalledExecutor) query(ctx context.Context, database, query string, maxBytes int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxBytes < 1 {
		maxBytes = 1
	}
	if e.backend == BackendCLI {
		return e.deps.run(ctx, e.binary, database, query, maxBytes)
	}
	return e.queryNative(ctx, database, query, maxBytes)
}
