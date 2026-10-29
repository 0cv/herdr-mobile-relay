package conversationlife

import (
	"errors"
	"sync"
)

const WitnessLimit = 16

// Overflow means independent cleanup failures exceeded retained identities.
// Count saturates, rather than wrapping to apparent success.
type Overflow struct{ Count uint64 }

func (*Overflow) Error() string { return "conversation cleanup witness capacity exceeded" }

// CloseSlot owns the first application Close attempt, not driver/OS internals.
// Do not copy it. Independent resources must have independent slots.
type CloseSlot struct {
	once  sync.Once
	owner *Owner
	close func() error
	err   error
}

func (o *Owner) Own(close func() error) (*CloseSlot, error) {
	if o == nil || o.ctx == nil || close == nil {
		return nil, ErrRefused
	}
	return &CloseSlot{owner: o, close: close}, nil
}

func (s *CloseSlot) Close() error {
	if s == nil {
		return ErrRefused
	}
	s.once.Do(func() {
		s.err = s.close()
		s.close = nil
		if s.err != nil {
			s.owner.record(s.err)
		}
	})
	return s.err
}

func (o *Owner) record(err error) {
	o.ledgerMu.Lock()
	defer o.ledgerMu.Unlock()
	if len(o.witnesses) < WitnessLimit {
		o.witnesses = append(o.witnesses, err)
		return
	}
	if o.overflow != ^uint64(0) {
		o.overflow++
	}
}

// Witnesses is a bounded snapshot, never a cached terminal result. Later retained
// Browser resources MUST finalize after Join; only the future app owner may then
// collect once. Ordinary operation/cancellation errors are not recorded here.
func (o *Owner) Witnesses() error {
	if o == nil {
		return ErrRefused
	}
	o.ledgerMu.Lock()
	defer o.ledgerMu.Unlock()
	errs := append([]error(nil), o.witnesses...)
	if o.overflow != 0 {
		errs = append(errs, &Overflow{Count: o.overflow})
	}
	return errors.Join(errs...)
}
