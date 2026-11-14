package app

import (
	"context"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
)

type databaseConversationRefresh interface {
	Refresh(context.Context) (conversation.BrowsePage, bool)
	NeedsReset() bool
}

func (s *Server) runDatabaseConversationFeed(feed *conversationFeed, watch databaseConversationRefresh) {
	capabilities := &conversation.SourceCapabilities{Source: "recorded_history"}
	var previous conversation.BrowsePage
	s.pollRecordedConversation(feed, watch.NeedsReset, func(first bool) (conversationBatch, bool, bool, bool) {
		page, reset := watch.Refresh(feed.ctx)
		reset = reset || !first && (page.SourceRevision != previous.SourceRevision || page.Available != previous.Available || page.State != previous.State || page.ReasonCode != previous.ReasonCode)
		previous = page
		kind := "batch"
		entries := page.Entries
		if reset {
			kind = "reset"
			entries = nil
		}
		publish := first || reset || len(page.Entries) > 0
		return conversationBatch{Kind: kind, SourceRevision: page.SourceRevision, Entries: entries, State: "recorded", Capabilities: capabilities, recordedEdits: page.RecordedEdits}, publish, page.State == conversation.BrowseReady, true
	})
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
