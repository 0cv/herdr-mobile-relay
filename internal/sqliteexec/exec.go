package sqliteexec

import (
	"context"
	"errors"
)

var ErrOutputLimit = errors.New("sqlite query output limit exceeded")

type Executor interface {
	QueryJSON(ctx context.Context, database, query string, maxBytes int) ([]byte, error)
}

func New() Executor {
	return &nativeExecutor{}
}
