package app

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/pibridge"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/slashcmd"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

const conversationFeedBytes = 2 * 1024 * 1024

func supportsConversationActivity(agent string) bool {
	return conversation.RecordedSupported(agent)
}

type conversationBatch struct {
	SubscriptionID string                           `json:"subscription_id,omitempty"`
	Kind           string                           `json:"kind"`
	SourceRevision string                           `json:"source_revision"`
	Sequence       uint64                           `json:"sequence"`
	Entries        []conversation.Entry             `json:"entries"`
	Remove         []string                         `json:"remove"`
	State          string                           `json:"state"`
	Capabilities   *conversation.SourceCapabilities `json:"capabilities"`
	recordedEdits  map[string]bool
}
type conversationFeed struct {
	mu            sync.Mutex
	ctx           context.Context
	cancel        context.CancelFunc
	target        protocol.TargetRef
	agent         *coordinator.AgentState
	identity      *pibridge.Identity
	clients       map[string]time.Time
	subscriptions map[string]string
	waiting       map[string]chan struct{}
	ready         chan struct{}
	readyOnce     sync.Once
	notify        chan struct{}
	revision      string
	sequence      uint64
	batches       []conversationBatch
	bytes         int
	failure       bool
	capabilities  *conversation.SourceCapabilities
	database      *conversation.DatabaseWatch
	loaded        map[string]map[string]bool
}

func conversationScope(agent *coordinator.AgentState) conversation.BrowseScope {
	return conversation.BrowseScope{Provider: agent.Agent, CWD: agent.Cwd, ForegroundCWD: agent.ForegroundCwd, SessionID: agent.SessionID, PaneID: agent.PaneID, ServerSessionID: agent.ServerSessionID, TerminalID: agent.TerminalID, Generation: agent.Generation, ProcessIdentity: agent.ConversationProcess, SelectionReason: agent.ConversationReason, InferredSession: agent.ConversationInferred, SelectionSource: agent.ConversationSource}
}
func (s *Server) conversationCurrent(feed *conversationFeed) bool {
	current, ok := s.state.Agent(feed.target.PaneID)
	return ok && s.kimiSelectionCurrent(feed.ctx, feed.agent) && validateExactPaneTarget(s.state, protocol.Inbound{PaneID: feed.target.PaneID, Target: &feed.target}, true) == nil && sameConversationTuple(feed.agent, current)
}
func (s *Server) activityIdentity(ctx context.Context, target protocol.TargetRef) (pibridge.Identity, error) {
	instance, err := pibridge.Instance(s.cfg.SocketPath)
	if err != nil {
		return pibridge.Identity{}, err
	}
	info, err := s.herdrC.PaneProcessInfoFresh(ctx, target.PaneID)
	if err != nil {
		return pibridge.Identity{}, err
	}
	pid, err := foregroundRoot(info, target.PaneID)
	return pibridge.Identity{Instance: instance, Pane: target.PaneID, Session: target.AgentSessionID, PID: pid}, err
}
func (s *Server) acquireConversationFeed(client *transport.ClientConn, inbound protocol.Inbound, agent *coordinator.AgentState) (*conversationFeed, error) {
	if inbound.Target == nil || validateExactPaneTarget(s.state, inbound, true) != nil {
		return nil, errors.New("exact conversation target required")
	}
	keyBytes, _ := json.Marshal(struct {
		Target protocol.TargetRef
		Scope  conversation.BrowseScope
	}{*inbound.Target, conversationScope(agent)})
	key := string(keyBytes)
	s.conversationLiveMu.Lock()
	defer s.conversationLiveMu.Unlock()
	if s.conversationFeeds == nil {
		s.conversationFeeds = make(map[string]*conversationFeed)
		s.conversationClients = make(map[string]*conversationFeed)
	}
	if old := s.conversationClients[client.ID()]; old != nil && (old != s.conversationFeeds[key] || inbound.Cursor == "") {
		old.mu.Lock()
		delete(old.clients, client.ID())
		delete(old.subscriptions, client.ID())
		delete(old.loaded, client.ID())
		if pending := old.waiting[client.ID()]; pending != nil {
			close(pending)
			delete(old.waiting, client.ID())
		}
		empty := len(old.clients) == 0
		old.mu.Unlock()
		if old != s.conversationFeeds[key] && empty {
			old.cancel()
		}
	}
	feed := s.conversationFeeds[key]
	if feed != nil && feed.ctx.Err() != nil {
		delete(s.conversationFeeds, key)
		feed = nil
	}
	if feed == nil {
		for oldKey, old := range s.conversationFeeds {
			if old.ctx.Err() != nil {
				delete(s.conversationFeeds, oldKey)
			}
		}
		if len(s.conversationFeeds) >= 8 {
			return nil, errors.New("conversation source limit")
		}
		ctx, cancel := context.WithCancel(context.Background())
		copyAgent := *agent
		feed = &conversationFeed{ctx: ctx, cancel: cancel, target: *inbound.Target, agent: &copyAgent, clients: make(map[string]time.Time), subscriptions: make(map[string]string), waiting: make(map[string]chan struct{}), ready: make(chan struct{}), notify: make(chan struct{})}
		s.conversationFeeds[key] = feed
		go s.runConversationFeed(feed)
	}
	feed.mu.Lock()
	defer feed.mu.Unlock()
	if _, exists := feed.clients[client.ID()]; !exists && len(feed.clients) >= 8 {
		return nil, errors.New("conversation viewer limit")
	}
	if feed.subscriptions[client.ID()] == "" {
		feed.subscriptions[client.ID()] = rand.Text()
	}
	feed.clients[client.ID()] = time.Now()
	s.conversationClients[client.ID()] = feed
	return feed, nil
}
func (s *Server) stopConversationFeed(clientID, subscription string) {
	s.conversationLiveMu.Lock()
	defer s.conversationLiveMu.Unlock()
	feed := s.conversationClients[clientID]
	if feed == nil {
		return
	}
	feed.mu.Lock()
	if subscription != "" && feed.subscriptions[clientID] != subscription {
		feed.mu.Unlock()
		return
	}
	delete(s.conversationClients, clientID)
	delete(feed.clients, clientID)
	delete(feed.subscriptions, clientID)
	delete(feed.loaded, clientID)
	if pending := feed.waiting[clientID]; pending != nil {
		close(pending)
		delete(feed.waiting, clientID)
	}
	empty := len(feed.clients) == 0
	feed.mu.Unlock()
	if empty {
		feed.cancel()
	}
}
func (feed *conversationFeed) publish(batch conversationBatch) {
	feed.mu.Lock()
	defer feed.mu.Unlock()
	if batch.SourceRevision != "" && feed.revision != "" && batch.SourceRevision != feed.revision {
		feed.batches = nil
		feed.bytes = 0
	}
	if batch.Entries == nil {
		batch.Entries = []conversation.Entry{}
	}
	if batch.Remove == nil {
		batch.Remove = []string{}
	}
	feed.revision = batch.SourceRevision
	feed.sequence = batch.Sequence
	feed.capabilities = batch.Capabilities
	encoded, _ := json.Marshal(batch)
	if len(encoded) > conversationFeedBytes {
		batch.Kind = "reset"
		batch.Entries = nil
		batch.Remove = nil
		encoded, _ = json.Marshal(batch)
	}
	feed.batches = append(feed.batches, batch)
	feed.bytes += len(encoded)
	for len(feed.batches) > 128 || feed.bytes > conversationFeedBytes {
		old, _ := json.Marshal(feed.batches[0])
		feed.bytes -= len(old)
		feed.batches = feed.batches[1:]
	}
	close(feed.notify)
	feed.notify = make(chan struct{})
	feed.readyOnce.Do(func() { close(feed.ready) })
}
func (s *Server) runConversationFeed(feed *conversationFeed) {
	defer feed.cancel()
	defer func() {
		feed.mu.Lock()
		feed.failure = true
		feed.batches = nil
		feed.bytes = 0
		close(feed.notify)
		feed.notify = make(chan struct{})
		feed.mu.Unlock()
	}()
	defer feed.readyOnce.Do(func() { close(feed.ready) })
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-feed.ctx.Done():
				return
			case <-ticker.C:
				feed.mu.Lock()
				for id, seen := range feed.clients {
					if time.Since(seen) > 30*time.Second {
						delete(feed.clients, id)
						delete(feed.subscriptions, id)
						delete(feed.loaded, id)
					}
				}
				empty := len(feed.clients) == 0
				feed.mu.Unlock()
				if empty || !s.conversationCurrent(feed) {
					feed.cancel()
					return
				}
			}
		}
	}()
	if slashcmd.IsPi("", feed.agent.Agent) {
		ctx, cancel := context.WithTimeout(feed.ctx, 2*time.Second)
		identity, err := s.activityIdentity(ctx, feed.target)
		cancel()
		if err == nil {
			received := false
			err = pibridge.ActivityStream(feed.ctx, identity, func(frame pibridge.ActivityFrame) error {
				if !s.conversationCurrent(feed) {
					return errors.New("conversation target replaced")
				}
				checkCtx, stop := context.WithTimeout(feed.ctx, 2*time.Second)
				current, currentErr := s.activityIdentity(checkCtx, feed.target)
				stop()
				if currentErr != nil || current != identity {
					return errors.New("conversation process replaced")
				}
				if frame.Kind == "heartbeat" {
					return nil
				}
				if frame.Kind == "snapshot" && frame.Page != nil {
					feed.mu.Lock()
					feed.identity = &identity
					feed.revision = frame.Page.SourceRevision
					feed.sequence = frame.Page.Sequence
					feed.capabilities = frame.Page.Capabilities
					feed.mu.Unlock()
					received = true
					feed.readyOnce.Do(func() { close(feed.ready) })
					return nil
				}
				feed.mu.Lock()
				previous := feed.sequence
				revision := feed.revision
				feed.mu.Unlock()
				if frame.SourceRevision == revision && frame.Sequence != previous+1 {
					return errors.New("conversation sequence gap")
				}
				feed.publish(conversationBatch{Kind: frame.Kind, SourceRevision: frame.SourceRevision, Sequence: frame.Sequence, Entries: frame.Entries, Remove: frame.Remove, State: frame.State, Capabilities: frame.Capabilities})
				return nil
			})
			if received {
				feed.mu.Lock()
				feed.failure = true
				close(feed.notify)
				feed.notify = make(chan struct{})
				feed.mu.Unlock()
				return
			}
			_ = err
		}
	}
	s.runRecordedConversationFeed(feed)
}
func (s *Server) runRecordedConversationFeed(feed *conversationFeed) {
	if watch := s.conversationB.NewDatabaseWatch(conversationScope(feed.agent)); watch != nil {
		feed.mu.Lock()
		feed.database = watch
		feed.mu.Unlock()
		s.runDatabaseConversationFeed(feed, watch)
		return
	}
	capabilities := &conversation.SourceCapabilities{Source: "recorded_history", ActiveBranch: conversation.IsKimi(feed.agent.Agent)}
	var previous conversation.BrowsePage
	var stamp string
	var retryAt time.Time
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	first := true
	for {
		if !s.conversationCurrent(feed) || feed.ctx.Err() != nil {
			return
		}
		nextStamp, tokenErr := s.conversationM.ChangeToken(feed.ctx, conversationScope(feed.agent))
		if first || tokenErr != nil || nextStamp != stamp || previous.State != conversation.BrowseReady && !time.Now().Before(retryAt) {
			page, readErr := s.conversationB.ReadPage(feed.ctx, conversation.BrowseRequest{Scope: conversationScope(feed.agent), Limit: 200})
			if readErr == nil && s.conversationCurrent(feed) {
				feed.mu.Lock()
				seq := feed.sequence + 1
				feed.mu.Unlock()
				kind := "batch"
				changed := []conversation.Entry{}
				old := make(map[string]string, len(previous.Entries))
				for _, entry := range previous.Entries {
					raw, _ := json.Marshal(entry)
					old[entry.ID] = string(raw)
				}
				overlap := false
				for _, entry := range page.Entries {
					raw, _ := json.Marshal(entry)
					if prior, ok := old[entry.ID]; ok {
						overlap = true
						if prior != string(raw) {
							changed = append(changed, entry)
						}
					} else {
						changed = append(changed, entry)
					}
				}
				if !first && ((len(previous.Entries) > 0 && !overlap && len(page.Entries) > 0) || page.SourceRevision != previous.SourceRevision || page.State != previous.State || (page.State != conversation.BrowseReady && (len(changed) > 0 || len(page.Entries) != len(previous.Entries))) || page.Available != previous.Available || page.ReasonCode != previous.ReasonCode || page.Reason != previous.Reason || page.Diagnostics != previous.Diagnostics || recordedWindowRemoved(previous, page) || recordedPlanChanged(previous, page)) {
					kind = "reset"
					changed = nil
				}
				if first || kind == "reset" || len(changed) > 0 {
					feed.publish(conversationBatch{Kind: kind, SourceRevision: page.SourceRevision, Sequence: seq, Entries: changed, Remove: []string{}, State: "recorded", Capabilities: capabilities})
				}
				previous = page
				stamp = nextStamp
				first = false
				retryAt = time.Now().Add(5 * time.Second)
			}
		}
		select {
		case <-feed.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (s *Server) conversationFeedPage(client *transport.ClientConn, inbound protocol.Inbound, agent *coordinator.AgentState) (conversation.BrowsePage, error) {
	feed, err := s.acquireConversationFeed(client, inbound, agent)
	if err != nil {
		return conversation.BrowsePage{}, err
	}
	feed.mu.Lock()
	subscription := feed.subscriptions[client.ID()]
	feed.mu.Unlock()
	select {
	case <-client.Context().Done():
		return conversation.BrowsePage{}, client.Context().Err()
	case <-feed.ready:
	case <-time.After(3 * time.Second):
		return conversation.BrowsePage{}, errors.New("conversation synchronizing")
	}
	if feed.ctx.Err() != nil {
		return conversation.BrowsePage{}, errors.New("conversation source closed")
	}
	feed.mu.Lock()
	identity := feed.identity
	seq := feed.sequence
	capabilities := feed.capabilities
	feed.mu.Unlock()
	if identity != nil {
		ctx, cancel := context.WithTimeout(client.Context(), 2*time.Second)
		defer cancel()
		page, err := pibridge.ActivityPage(ctx, *identity, inbound.Cursor, inbound.Limit)
		if err != nil {
			return conversation.BrowsePage{}, err
		}
		current, err := s.activityIdentity(ctx, feed.target)
		if err != nil || current != *identity || !s.conversationCurrent(feed) {
			return conversation.BrowsePage{}, errors.New("conversation process replaced")
		}
		feed.mu.Lock()
		currentSubscription := feed.subscriptions[client.ID()]
		feed.mu.Unlock()
		if currentSubscription != subscription {
			return conversation.BrowsePage{}, errors.New("conversation subscription changed")
		}
		page.SubscriptionID = subscription
		return page, nil
	}
	if strings.HasPrefix(inbound.Cursor, "pa:") {
		return conversation.BrowsePage{}, errors.New("runtime conversation unavailable")
	}
	page, err := s.conversationB.ReadPage(client.Context(), conversation.BrowseRequest{Scope: conversationScope(agent), Cursor: inbound.Cursor, Limit: inbound.Limit, Retry: inbound.Retry})
	feed.mu.Lock()
	currentSubscription := feed.subscriptions[client.ID()]
	watch := feed.database
	feed.mu.Unlock()
	if err == nil && watch != nil && s.conversationCurrent(feed) && currentSubscription == subscription {
		watch.Observe(page, inbound.Cursor == "")
		feed.observeDatabasePage(client.ID(), subscription, inbound.Cursor, page)
	}
	if currentSubscription != subscription || !s.conversationCurrent(feed) {
		return conversation.BrowsePage{}, errors.New("conversation subscription changed")
	}
	page.SubscriptionID = subscription
	page.Sequence = seq
	page.Capabilities = capabilities
	page.LiveState = "recorded"
	return page, err
}
func (s *Server) handleConversationUpdates(client *transport.ClientConn, inbound protocol.Inbound) {
	action := "get_conversation_updates"
	s.conversationLiveMu.Lock()
	feed := s.conversationClients[client.ID()]
	s.conversationLiveMu.Unlock()
	if feed == nil || inbound.Target == nil || feed.target != *inbound.Target || !s.conversationCurrent(feed) {
		s.sendCommandResult(client, inbound.RequestID, action, false, "failed", "Conversation subscription is unavailable", inbound.PaneID, nil)
		return
	}
	feed.mu.Lock()
	if inbound.SubscriptionID == "" || feed.subscriptions[client.ID()] != inbound.SubscriptionID {
		feed.mu.Unlock()
		s.sendCommandResult(client, inbound.RequestID, action, false, "failed", "Conversation subscription changed", inbound.PaneID, nil)
		return
	}
	if feed.waiting[client.ID()] != nil {
		feed.mu.Unlock()
		s.sendCommandResult(client, inbound.RequestID, action, false, "failed", "Conversation request already pending", inbound.PaneID, nil)
		return
	}
	cancelled := make(chan struct{})
	feed.waiting[client.ID()] = cancelled
	feed.clients[client.ID()] = time.Now()
	feed.mu.Unlock()
	defer func() {
		feed.mu.Lock()
		if feed.waiting[client.ID()] == cancelled {
			delete(feed.waiting, client.ID())
		}
		feed.mu.Unlock()
	}()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		feed.mu.Lock()
		batch := conversationBatch{Kind: "heartbeat", SourceRevision: feed.revision, Sequence: feed.sequence, Entries: []conversation.Entry{}, Remove: []string{}, Capabilities: feed.capabilities}
		if inbound.SourceRevision != feed.revision || len(feed.batches) > 0 && inbound.After+1 < feed.batches[0].Sequence {
			batch.Kind = "reset"
		} else {
			for _, item := range feed.batches {
				if item.Sequence > inbound.After {
					batch = item
					break
				}
			}
		}
		failed := feed.failure
		notify := feed.notify
		feed.mu.Unlock()
		ready := batch.Kind != "heartbeat" || failed
		if !ready {
			select {
			case <-client.Context().Done():
				return
			case <-cancelled:
				return
			case <-feed.ctx.Done():
				failed = true
			case <-timeout.C:
				ready = true
			case <-notify:
				continue
			}
		}
		if failed || !s.conversationCurrent(feed) || s.authorizeDeviceAction(client, protocol.ActionMetadata{Operation: action, Class: protocol.ActionReadOnly}, "") != nil {
			s.sendCommandResult(client, inbound.RequestID, action, false, "failed", "Conversation source disconnected", inbound.PaneID, nil)
			return
		}
		if ready {
			feed.mu.Lock()
			currentSubscription := feed.subscriptions[client.ID()]
			if currentSubscription == inbound.SubscriptionID {
				batch = feed.databaseBatchLocked(client.ID(), batch)
			}
			feed.mu.Unlock()
			if currentSubscription != inbound.SubscriptionID {
				return
			}
			batch.SubscriptionID = inbound.SubscriptionID
			s.sendCommandResult(client, inbound.RequestID, action, true, "completed", "", inbound.PaneID, batch)
			return
		}
	}
}
