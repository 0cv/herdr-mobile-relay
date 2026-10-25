package app

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
)

type databaseRefreshResult struct {
	sequence  uint64
	revision  string
	available bool
	state     conversation.BrowseState
	reason    string
}

type databaseRefreshObserver struct {
	watch   databaseConversationRefresh
	feed    *conversationFeed
	mu      sync.Mutex
	results [128]databaseRefreshResult
}

func (o *databaseRefreshObserver) NeedsReset() bool { return o.watch.NeedsReset() }

func (o *databaseRefreshObserver) Refresh(ctx context.Context) (conversation.BrowsePage, bool) {
	page, reset := o.watch.Refresh(ctx)
	o.feed.mu.Lock()
	sequence := o.feed.sequence + 1
	o.feed.mu.Unlock()
	o.mu.Lock()
	o.results[sequence%uint64(len(o.results))] = databaseRefreshResult{sequence, page.SourceRevision, page.Available, page.State, page.ReasonCode}
	o.mu.Unlock()
	return page, reset
}

func (o *databaseRefreshObserver) result(batch conversationBatch) (databaseRefreshResult, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	result := o.results[batch.Sequence%uint64(len(o.results))]
	if result.sequence != batch.Sequence || result.revision != batch.SourceRevision {
		return result, fmt.Errorf("refresh/publication mismatch: refresh=%+v batch=%+v", result, batch)
	}
	return result, nil
}

func (r databaseRefreshResult) unavailable() bool {
	return !r.available && r.state == conversation.BrowseReady && r.reason == "source_unavailable"
}

func waitRecordedDatabaseBatch(t *testing.T, feed *conversationFeed, sequence *uint64, deadline <-chan time.Time) conversationBatch {
	t.Helper()
	for {
		feed.mu.Lock()
		for _, batch := range feed.batches {
			if batch.Sequence > *sequence {
				*sequence = batch.Sequence
				feed.mu.Unlock()
				return batch
			}
		}
		notify := feed.notify
		feed.mu.Unlock()
		select {
		case <-notify:
		case <-feed.ctx.Done():
			t.Fatal("recorded database feed canceled")
		case <-deadline:
			t.Fatal("recorded database update timed out")
		}
	}
}

func waitDatabaseUnavailable(t *testing.T, feed *conversationFeed, observed *databaseRefreshObserver, sequence *uint64) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		batch := waitRecordedDatabaseBatch(t, feed, sequence, deadline.C)
		if batch.Kind != "reset" {
			t.Fatalf("kind %s, want reset: %+v", batch.Kind, batch)
		}
		result, err := observed.result(batch)
		if err != nil {
			t.Fatal(err)
		}
		if result.unavailable() {
			return
		}
	}
}

type databaseRefreshStub struct {
	page       conversation.BrowsePage
	reset      bool
	needsReset bool
	ctx        context.Context
	calls      int
}

func (s *databaseRefreshStub) NeedsReset() bool { return s.needsReset }
func (s *databaseRefreshStub) Refresh(ctx context.Context) (conversation.BrowsePage, bool) {
	s.ctx = ctx
	s.calls++
	return s.page, s.reset
}

func TestDatabaseRefreshObserver(t *testing.T) {
	feed := &conversationFeed{ctx: t.Context(), notify: make(chan struct{}), ready: make(chan struct{})}
	stub := &databaseRefreshStub{}
	observer := &databaseRefreshObserver{watch: stub, feed: feed}
	for _, test := range []struct {
		name        string
		page        conversation.BrowsePage
		reset       bool
		unavailable bool
	}{
		{"old ready", conversation.BrowsePage{Available: true, State: conversation.BrowseReady, SourceRevision: "old"}, true, false},
		{"empty query error", conversation.BrowsePage{State: conversation.BrowseReady, ReasonCode: "query_failed"}, false, false},
		{"wrong state", conversation.BrowsePage{State: conversation.BrowsePreparing, ReasonCode: "source_unavailable"}, true, false},
		{"available", conversation.BrowsePage{Available: true, State: conversation.BrowseReady, ReasonCode: "source_unavailable"}, true, false},
		{"unavailable", conversation.BrowsePage{State: conversation.BrowseReady, ReasonCode: "source_unavailable"}, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			stub.page, stub.reset, stub.needsReset = test.page, test.reset, test.reset
			page, reset := observer.Refresh(t.Context())
			if !reflect.DeepEqual(page, stub.page) || reset != stub.reset || observer.NeedsReset() != stub.needsReset || stub.ctx != t.Context() {
				t.Fatal("observer changed the refresh request or result")
			}
			batch := conversationBatch{Kind: "reset", Sequence: feed.sequence + 1, SourceRevision: page.SourceRevision}
			feed.publish(batch)
			result, err := observer.result(batch)
			if err != nil || result.unavailable() != test.unavailable {
				t.Fatalf("observation=%+v err=%v", result, err)
			}
			batch.SourceRevision = "wrong"
			if _, err := observer.result(batch); err == nil {
				t.Fatal("accepted mismatched revision")
			}
			batch.Sequence += 128
			if _, err := observer.result(batch); err == nil {
				t.Fatal("accepted overwritten sequence")
			}
		})
	}
	if stub.calls != 5 {
		t.Fatal("observer made extra refresh requests")
	}
}

func TestDatabaseRefreshObserverUnpublished(t *testing.T) {
	feed := &conversationFeed{ctx: t.Context(), notify: make(chan struct{}), ready: make(chan struct{})}
	stub := &databaseRefreshStub{page: conversation.BrowsePage{State: conversation.BrowseReady, ReasonCode: "query_failed"}}
	observer := &databaseRefreshObserver{watch: stub, feed: feed}
	observer.Refresh(t.Context())
	stub.page = conversation.BrowsePage{State: conversation.BrowseReady, ReasonCode: "source_unavailable"}
	observer.Refresh(t.Context())
	batch := conversationBatch{Kind: "reset", Sequence: 1}
	feed.publish(batch)
	stub.page = conversation.BrowsePage{Available: true, State: conversation.BrowseReady}
	observer.Refresh(t.Context())
	result, err := observer.result(batch)
	if err != nil || !result.unavailable() || stub.calls != 3 {
		t.Fatalf("unpublished refresh replaced a published outcome: %+v, %v", result, err)
	}
}

func TestDatabaseUnavailableObservation(t *testing.T) {
	feed := &conversationFeed{ctx: t.Context(), notify: make(chan struct{}), ready: make(chan struct{})}
	stub := &databaseRefreshStub{}
	observer := &databaseRefreshObserver{watch: stub, feed: feed}
	for _, page := range []conversation.BrowsePage{
		{Available: true, State: conversation.BrowseReady, SourceRevision: "old"},
		{State: conversation.BrowseReady, ReasonCode: "query_failed"},
		{State: conversation.BrowseReady, ReasonCode: "source_unavailable"},
		{State: conversation.BrowseReady, ReasonCode: "source_unavailable"},
	} {
		stub.page = page
		observer.Refresh(t.Context())
		feed.publish(conversationBatch{Kind: "reset", Sequence: feed.sequence + 1, SourceRevision: page.SourceRevision})
	}
	var sequence uint64
	waitDatabaseUnavailable(t, feed, observer, &sequence)
	if sequence != 3 || feed.sequence != 4 {
		t.Fatalf("observer consumed duplicate unavailable: consumed=%d published=%d", sequence, feed.sequence)
	}
}
