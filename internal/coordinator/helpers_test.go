package coordinator

import "context"

func (d *Dispatcher) sendQuestionKeys(ctx context.Context, paneID string, keys []string) error {
	return d.sendQuestionKeysForSession(ctx, WorkerToken{
		PaneID:      paneID,
		Generation:  d.state.Generation(paneID),
		AllowAbsent: true,
	}, paneID, keys)
}

func (s *Scheduler) UpdateLedgerPhase(key string, generation int64, phase string) bool {
	result := &CommandResult{OK: phase == "confirmed", Phase: phase}
	return s.UpdateLedgerResult(key, generation, result)
}
