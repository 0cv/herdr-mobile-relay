//go:build herdr_sqlite_logical_test && !herdr_installed_hosted_test && !herdr_tailscale_test && !herdr_tailscale_cli_fixture_binary

package sqliteexec

import "context"

// Compile-time logical fixture only: not an operational backend or
// production/native qualification. Refusal precedes all backend effects.
func FromEnv() (Executor, error) { return nil, ErrUnavailable }

func Resolve(backend Backend, cliBinary string) (Executor, error) {
	return nil, ErrUnavailable
}

func MustFromEnv() Executor { return &logicalUnavailableExecutor{} }

type logicalUnavailableExecutor struct{}

func (*logicalUnavailableExecutor) Ready() bool { return false }
func (*logicalUnavailableExecutor) QueryJSON(ctx context.Context, database, query string, maxBytes int) ([]byte, error) {
	return nil, ErrUnavailable
}

func (n *nativeExecutor) Ready() bool { return false }
func (n *nativeExecutor) QueryJSON(ctx context.Context, database, query string, maxBytes int) ([]byte, error) {
	return nil, ErrUnavailable
}

func (c *cliExecutor) Ready() bool { return false }
func (c *cliExecutor) QueryJSON(ctx context.Context, database, query string, maxBytes int) ([]byte, error) {
	return nil, ErrUnavailable
}
