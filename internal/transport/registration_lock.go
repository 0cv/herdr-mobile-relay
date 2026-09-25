package transport

import (
	"context"
	"errors"
	"sync"
)

// registrationLock preserves the Hub registration barrier while allowing
// lifecycle callers with deadlines to abandon lock acquisition without
// leaving a queued transition behind.
type registrationLock struct {
	once   sync.Once
	permit chan struct{}
}

func (l *registrationLock) init() {
	l.once.Do(func() {
		l.permit = make(chan struct{}, 1)
		l.permit <- struct{}{}
	})
}

func (l *registrationLock) Lock() {
	l.init()
	<-l.permit
}

func (l *registrationLock) LockContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("Hub registration lock requires a context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	l.init()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.permit:
	}
	if err := ctx.Err(); err != nil {
		l.permit <- struct{}{}
		return err
	}
	return nil
}

func (l *registrationLock) Unlock() {
	l.init()
	l.permit <- struct{}{}
}
