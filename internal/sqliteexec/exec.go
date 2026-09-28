package sqliteexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/childenv"
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

// FromEnv builds an executor using HERDR_SQLITE_BACKEND (default auto).
func FromEnv() (Executor, error) {
	return Resolve(ParseBackend(os.Getenv(EnvBackend)), "sqlite3")
}

// Resolve builds an executor for the requested backend.
// cliBinary is the sqlite3 executable name or path used by the CLI backend.
func Resolve(backend Backend, cliBinary string) (Executor, error) {
	if cliBinary == "" {
		cliBinary = "sqlite3"
	}
	switch ParseBackend(string(backend)) {
	case BackendNative:
		return newNative(), nil
	case BackendCLI:
		cli := newCLI(cliBinary)
		if !cli.Ready() {
			return nil, fmt.Errorf("%w: %s not found", ErrUnavailable, cliBinary)
		}
		return cli, nil
	default: // auto
		native := newNative()
		if native.Ready() {
			return native, nil
		}
		cli := newCLI(cliBinary)
		if cli.Ready() {
			return cli, nil
		}
		return nil, ErrUnavailable
	}
}

// MustFromEnv returns FromEnv or a CLI fallback that reports not Ready when
// nothing works. Callers still check Ready/databases().
func MustFromEnv() Executor {
	executor, err := FromEnv()
	if err == nil {
		return executor
	}
	return newCLI("sqlite3")
}

type cliExecutor struct {
	binary string
}

func newCLI(binary string) *cliExecutor {
	return &cliExecutor{binary: binary}
}

func (c *cliExecutor) Ready() bool {
	_, err := exec.LookPath(c.binary)
	return err == nil
}

func (c *cliExecutor) QueryJSON(ctx context.Context, database, query string, maxBytes int) ([]byte, error) {
	if maxBytes < 1 {
		maxBytes = 1
	}
	command := childenv.CommandContext(ctx, c.binary, "-readonly", "-batch", "-json", database, query)
	stdout := &limitedBuffer{remaining: maxBytes}
	var stderr limitedBuffer
	stderr.remaining = 4096
	command.Stdout = stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if stdout.overflow {
			return nil, ErrOutputLimit
		}
		return nil, fmt.Errorf("sqlite3 query failed: %w", err)
	}
	if stdout.overflow {
		return nil, ErrOutputLimit
	}
	return stdout.Bytes(), nil
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
