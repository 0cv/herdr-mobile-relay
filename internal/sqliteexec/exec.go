package sqliteexec

import (
	"bytes"
	"context"
	"errors"
	"strings"
)

// Backend selects how conversation readers execute SQL against agent SQLite files.
type Backend string

const (
	BackendAuto   Backend = "auto"
	BackendNative Backend = "native"
	BackendCLI    Backend = "cli"
)

// EnvBackend is the relay env var that selects the SQLite query backend.
const EnvBackend = "HERDR_SQLITE_BACKEND"

var (
	// ErrOutputLimit means the query result exceeded the caller max byte budget.
	ErrOutputLimit = errors.New("sqlite query output limit exceeded")
	// ErrUnavailable means no usable backend could be constructed.
	ErrUnavailable = errors.New("sqlite query backend unavailable")
)

// Executor runs read-only SQL and returns sqlite3 -json shaped bytes (a JSON array).
type Executor interface {
	// Ready reports whether this executor can run queries without a further
	// install step (native is always ready; CLI needs LookPath success).
	Ready() bool
	QueryJSON(ctx context.Context, database, query string, maxBytes int) ([]byte, error)
}

// ParseBackend normalizes env / config values. Empty means auto.
func ParseBackend(value string) Backend {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", string(BackendAuto):
		return BackendAuto
	case string(BackendNative):
		return BackendNative
	case string(BackendCLI):
		return BackendCLI
	default:
		return BackendAuto
	}
}

type cliExecutor struct {
	binary string
}

func newCLI(binary string) *cliExecutor {
	return &cliExecutor{binary: binary}
}

type limitedBuffer struct {
	bytes.Buffer
	remaining int
	overflow  bool
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	if len(data) > b.remaining {
		if b.remaining > 0 {
			_, _ = b.Buffer.Write(data[:b.remaining])
			b.remaining = 0
		}
		b.overflow = true
		return 0, ErrOutputLimit
	}
	b.remaining -= len(data)
	return b.Buffer.Write(data)
}
