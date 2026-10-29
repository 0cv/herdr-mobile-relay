package localcontrol

import (
	"context"
	"errors"

	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

// AcknowledgedPublication returns historical owned data, not readiness or a wire
// capability. It shares Dispatch's bounded admission, serialization and drain.
func (c InstalledControl) AcknowledgedPublication(ctx context.Context) (tailscalecli.AcknowledgedPublication, error) {
	var zero tailscalecli.AcknowledgedPublication
	refused := errors.New("installed publication data unavailable")
	s := c.state
	if s == nil || ctx == nil || ctx.Err() != nil {
		return zero, refused
	}
	s.mu.Lock()
	if s.retired || s.backend.AcknowledgedPublication == nil {
		s.mu.Unlock()
		return zero, refused
	}
	select {
	case s.slots <- struct{}{}:
	default:
		s.mu.Unlock()
		return zero, refused
	}
	s.active.Add(1)
	s.mu.Unlock()
	defer s.active.Done()
	defer func() { <-s.slots }()
	call, cancel := context.WithTimeout(s.ctx, StatusTimeout)
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { cancel(); close(done) })
	defer func() {
		cancel()
		if !stop() {
			<-done
		}
	}()
	select {
	case s.gate <- struct{}{}:
	case <-call.Done():
		return zero, refused
	case <-ctx.Done():
		return zero, refused
	}
	defer func() { <-s.gate }()
	if call.Err() != nil || ctx.Err() != nil || s.publication != "acknowledged" {
		return zero, refused
	}
	value, err := s.backend.AcknowledgedPublication(call)
	if err != nil {
		return zero, err
	}
	d := value.Snapshot()
	p := s.proposal
	if value == zero || !publicationMatchesProposal(d, p, s.backend.PublicationSelection) || call.Err() != nil || ctx.Err() != nil {
		return zero, refused
	}
	return value, nil
}

// Correlation of data only; this cannot establish owned publication authority.
func publicationMatchesProposal(d tailscalecli.PublicationSnapshot, p InstalledProposal, pin [32]byte) bool {
	return pin != [32]byte{} && d.Selection == pin && d.Profile == p.Profile && d.Generation == p.Run && d.Installation == p.Installation && d.Attempt == p.Attempt && d.Node == p.Node && d.Origin == p.Origin && d.HTTPSPort == int(p.HTTPSPort) && d.BackendPort == int(p.RelayPort)
}
