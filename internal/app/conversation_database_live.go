package app

import (
	"context"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
)

type databaseConversationRefresh interface {
	Refresh(context.Context) (conversation.BrowsePage, bool)
	NeedsReset() bool
}

func (s *Server) runDatabaseConversationFeed(feed *conversationFeed, watch databaseConversationRefresh) {
	capabilities := &conversation.SourceCapabilities{Source: "recorded_history"}
	var previous conversation.BrowsePage
	var stamp string
	var retryAt time.Time
	first := true
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !s.conversationCurrent(feed) || feed.ctx.Err() != nil {
			return
		}
		nextStamp, err := s.conversationM.ChangeToken(feed.ctx, conversationScope(feed.agent))
		if first || watch.NeedsReset() || err != nil || stamp != nextStamp || previous.State != conversation.BrowseReady && !time.Now().Before(retryAt) {
			page, reset := watch.Refresh(feed.ctx)
			if !s.conversationCurrent(feed) || feed.ctx.Err() != nil {
				return
			}
			reset = reset || !first && (page.SourceRevision != previous.SourceRevision || page.Available != previous.Available || page.State != previous.State || page.ReasonCode != previous.ReasonCode)
			if first || reset || len(page.Entries) > 0 {
				kind := "batch"
				if reset {
					kind = "reset"
					page.Entries = nil
				}
				feed.mu.Lock()
				sequence := feed.sequence + 1
				feed.mu.Unlock()
				feed.publish(conversationBatch{Kind: kind, SourceRevision: page.SourceRevision, Sequence: sequence, Entries: page.Entries, State: "recorded", Capabilities: capabilities, recordedEdits: page.RecordedEdits})
			}
			previous, stamp, first = page, nextStamp, false
			retryAt = time.Now().Add(5 * time.Second)
		}
		select {
		case <-feed.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (feed *conversationFeed) observeDatabasePage(clientID, subscription, cursor string, page conversation.BrowsePage) {
	if page.State != conversation.BrowseReady || !page.Available {
		return
	}
	feed.mu.Lock()
	defer feed.mu.Unlock()
	if feed.subscriptions[clientID] != subscription || feed.revision != page.SourceRevision {
		return
	}
	if feed.loaded == nil {
		feed.loaded = make(map[string]map[string]bool)
	}
	if feed.loaded[clientID] == nil || cursor == "" {
		feed.loaded[clientID] = make(map[string]bool)
	}
	for _, entry := range page.Entries {
		if len(feed.loaded[clientID]) >= 20000 {
			return
		}
		feed.loaded[clientID][entry.ID] = true
	}
}

func (feed *conversationFeed) databaseBatchLocked(clientID string, batch conversationBatch) conversationBatch {
	if feed.database == nil {
		return batch
	}
	if feed.loaded == nil {
		feed.loaded = make(map[string]map[string]bool)
	}
	if batch.Kind == "reset" {
		delete(feed.loaded, clientID)
		return batch
	}
	if batch.Kind != "batch" {
		return batch
	}
	if feed.loaded[clientID] == nil {
		feed.loaded[clientID] = make(map[string]bool)
	}
	entries := make([]conversation.Entry, 0, len(batch.Entries))
	for _, entry := range batch.Entries {
		if batch.recordedEdits[entry.ID] && !feed.loaded[clientID][entry.ID] {
			continue
		}
		if !feed.loaded[clientID][entry.ID] && len(feed.loaded[clientID]) >= 20000 {
			batch.Kind, batch.Entries = "reset", nil
			delete(feed.loaded, clientID)
			return batch
		}
		feed.loaded[clientID][entry.ID] = true
		entries = append(entries, entry)
	}
	batch.Entries = entries
	return batch
}
