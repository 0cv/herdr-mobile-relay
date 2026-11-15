package push

import (
	"context"
	"time"
)

func terminalSubscriptionKey(subscription Subscription) string {
	return subscription.DeviceID + "\x00" + subscription.Endpoint
}

func (m *Manager) RunOnce(ctx context.Context, now time.Time) ([]DeliveryResult, error) {
	m.mu.Lock()
	reconciled := m.reconciled
	m.mu.Unlock()
	if !reconciled {
		return nil, nil
	}
	results, err := m.processDue(ctx, now)
	if err != nil {
		return results, err
	}
	return results, m.retireFinishedKeys(results)
}

func (m *Manager) retireFinishedKeys(results []DeliveryResult) error {
	seen := make(map[PushEventKey]bool)
	for _, result := range results {
		seen[result.Key] = true
	}
	for key := range seen {
		m.mu.Lock()
		retracting := m.retracting[key]
		m.mu.Unlock()
		if retracting && !m.queue.hasEntriesFor(key) {
			if err := m.queue.forgetDelivered(key); err != nil {
				return err
			}
			m.mu.Lock()
			delete(m.retracting, key)
			m.mu.Unlock()
			continue
		}
		if m.queue.hasEntriesFor(key) {
			continue
		}
		if key.Category == CategoryTest || key.Category == CategoryUpdate {
			if err := m.queue.forgetDelivered(key); err != nil {
				return err
			}
		}
		if key.Category == CategoryTest || key.Category == CategoryUpdate || len(m.queue.deliveredFor(key)) == 0 {
			m.mu.Lock()
			delete(m.active, key)
			m.mu.Unlock()
		}
	}
	return nil
}

func (m *Manager) processDue(ctx context.Context, now time.Time) ([]DeliveryResult, error) {
	m.queue.process.Lock()
	defer m.queue.process.Unlock()
	entries := m.queue.dueEntries(now)
	results := make([]DeliveryResult, 0, len(entries))
	terminalSubscriptions := make(map[string]bool)
	dirty := false
	for _, entry := range entries {
		if ctx.Err() != nil {
			return m.finishPass(results, dirty, now, ctx.Err())
		}
		result, err := m.deliverOne(ctx, now, entry, terminalSubscriptions)
		if err != nil {
			return m.finishPass(results, dirty, now, err)
		}
		if result.Disposition == DeliveryPruned {
			terminalSubscriptions[terminalSubscriptionKey(entry.Subscription)] = true
		}
		changed, err := m.applyResult(entry, result, now)
		dirty = changed || dirty
		if err != nil {
			return m.finishPass(results, dirty, now, err)
		}
		results = append(results, result)
	}
	return m.finishPass(results, dirty, now, nil)
}

func (m *Manager) applyResult(entry queueEntry, result DeliveryResult, now time.Time) (bool, error) {
	if result.Disposition == DeliveryRetrying {
		return m.queue.rescheduleInMemory(entry.ID, entry, result.Attempts, result.NextAttempt), nil
	}
	changed := m.queue.finishInMemory(entry.ID, entry, result.Disposition, now)
	if result.Disposition != DeliveryAccepted {
		return changed, nil
	}
	return changed, m.markAccepted(entry.Event.Key, now)
}

func (m *Manager) finishPass(results []DeliveryResult, dirty bool, now time.Time, cause error) ([]DeliveryResult, error) {
	recovered, err := m.recoverPrunedSubscriptionsWhileProcessing(results, now)
	dirty = recovered || dirty
	if err != nil {
		dirty = m.queue.restorePrunedWhileProcessing(results, now) || dirty
		if persistErr := m.flush(dirty); persistErr != nil {
			return results, persistErr
		}
		return results, err
	}
	if err := m.flush(dirty); err != nil {
		return results, err
	}
	return results, cause
}

func (m *Manager) flush(dirty bool) error {
	if !dirty {
		return nil
	}
	return m.queue.write()
}

func (m *Manager) deliverable(key PushEventKey, now time.Time) bool {
	m.mu.Lock()
	active := m.active[key]
	retracting := m.retracting[key]
	m.mu.Unlock()
	return retracting || active && m.policy.Allows(key, "en", now)
}

func (m *Manager) markAccepted(key PushEventKey, acceptedAt time.Time) error {
	m.mu.Lock()
	retracting := m.retracting[key]
	m.mu.Unlock()
	if retracting {
		return nil
	}
	return m.policy.MarkAccepted(key, acceptedAt)
}

func (m *Manager) deliverOne(
	ctx context.Context,
	now time.Time,
	entry queueEntry,
	terminalSubscriptions map[string]bool,
) (result DeliveryResult, err error) {
	if terminalSubscriptions[terminalSubscriptionKey(entry.Subscription)] {
		return newDeliveryResult(entry, DeliveryPruned, entry.Attempts), nil
	}
	if !now.Before(entry.Event.ExpiresAt) {
		return newDeliveryResult(entry, DeliveryExpired, entry.Attempts), nil
	}
	if !m.deliverable(entry.Event.Key, now) {
		return newDeliveryResult(entry, DeliveryStale, entry.Attempts), nil
	}
	sendErr := m.sendPush(ctx, entry.Subscription, entry.Event.Payload)
	if sendErr != nil && ctx.Err() != nil {
		return result, ctx.Err()
	}
	attempts := entry.Attempts + 1
	if sendErr == nil {
		return newDeliveryResult(entry, DeliveryAccepted, attempts), nil
	}
	if isTerminalError(sendErr) {
		return newDeliveryResult(entry, DeliveryPruned, attempts), nil
	}
	if retryablePushError(sendErr) && attempts < maxDeliveryAttempts {
		next := now.Add(retryDelay(attempts))
		if next.Before(entry.Event.ExpiresAt) {
			result = newDeliveryResult(entry, DeliveryRetrying, attempts)
			result.NextAttempt = next
			return result, nil
		}
	}
	return newDeliveryResult(entry, DeliveryDropped, attempts), nil
}
