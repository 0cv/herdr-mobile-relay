package app

import (
	"context"
	"errors"
	"sync"
)

// contextLock serializes managed lifecycle transitions while allowing a
// caller's own deadline to cancel lock acquisition. Its zero value is ready.
type contextLock struct {
	once  sync.Once
	token chan struct{}
}

func (l *contextLock) Lock(ctx context.Context) (func(), error) {
	if ctx == nil {
		return nil, errors.New("lifecycle lock requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.once.Do(func() { l.token = make(chan struct{}, 1) })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case l.token <- struct{}{}:
	}
	if err := ctx.Err(); err != nil {
		<-l.token
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { <-l.token }) }, nil
}
