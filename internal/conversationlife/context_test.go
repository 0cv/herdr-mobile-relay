package conversationlife

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestA1ContextAndInvalidInputs(t *testing.T) {
	owner := New()
	ctx, cancel := context.WithDeadline(context.Background(), time.Time{})
	t.Cleanup(func() { owner.Begin(); cancel(); owner.Join() })
	type key struct{}
	ctx = context.WithValue(ctx, key{}, "caller value")
	var got error
	var value any
	var deadline bool
	err := owner.Do(ctx, func(c *Call) error {
		got = c.Context().Err()
		value = c.Context().Value(key{})
		_, deadline = c.Context().Deadline()
		return nil
	})
	if err != nil || !errors.Is(got, context.DeadlineExceeded) || value != "caller value" || !deadline {
		t.Error("merged context semantics", err, got, value, deadline)
	}
	effects := 0
	callback := func(*Call) error { effects++; return nil }
	var missing *Owner
	var zero Owner
	var empty Call
	for _, err := range []error{
		missing.Do(context.Background(), callback),
		zero.Do(context.Background(), callback),
		owner.Do(nil, callback),
		owner.Do(context.Background(), nil),
		owner.DoIn(&empty, callback),
	} {
		if !errors.Is(err, ErrRefused) {
			t.Error("invalid input admitted", err)
		}
	}
	if effects != 0 || owner.Witnesses() != nil {
		t.Error("invalid inputs caused effects")
	}
}
