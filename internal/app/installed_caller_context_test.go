//go:build herdr_sqlite_logical_test && herdr_installed_inert_test && !herdr_installed_hosted_test && !herdr_tailscale_test && !herdr_tailscale_cli_fixture_binary

package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/activity"
	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
)

// This calls the actual server poller-title and request-activity helpers with
// the constructed installed Reader/Resolver. Root discovery runs inside the
// Reader's counted, same-owner call, not a context-recording stand-in.
func TestInstalledServerCallerCancellationRetiresDiscovery(t *testing.T) {
	for _, kind := range []string{"poller_title", "activity_response", "slash_location", "finished_response"} {
		for _, mode := range []string{"caller_cancel", "owner_retire"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				root := IsolateInstalledFixture(t)
				owner := conversationlife.New()
				reader, browser, resolver, err := acquireInstalledConversation(context.Background(), root, filepath.Join(root, "cache"), owner)
				if err != nil {
					owner.Begin()
					owner.Join()
					t.Fatal(err)
				}
				entered := make(chan context.Context, 1)
				retired := make(chan error, 1)
				restore, err := reader.HoldInstalledRootsForFixture(func(ctx context.Context, _, _ string) ([]string, error) {
					entered <- ctx
					select {
					case <-ctx.Done():
						retired <- ctx.Err()
						return nil, ctx.Err()
					case <-time.After(10 * time.Second):
						retired <- errors.New("admitted discovery did not retire")
						return nil, errors.New("admitted discovery did not retire")
					}
				})
				if err != nil {
					_ = browser.Close()
					owner.Begin()
					owner.Join()
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				started := false
				defer func() {
					cancel()
					if started {
						<-done
					}
					_ = browser.Close()
					owner.Begin()
					owner.Join()
					restore()
				}()
				// Only the installed app's already acquired components are needed by
				// these server callers. Do not construct an ordinary service here.
				s := &Server{state: coordinator.NewState(nil), conversationOwner: owner, conversationM: reader, sessions: resolver}
				const sessionID = "123e4567-e89b-12d3-a456-426614174321"
				agent := &coordinator.AgentState{Agent: "pi", Session: sessionID}
				s.activityView = []activity.Entry{{Kind: "finished", Agent: "pi", Session: sessionID}}
				var result []activity.Entry
				var response string
				var callErr error
				go func() {
					defer close(done)
					switch kind {
					case "poller_title":
						callErr = s.resolveAgentSessionName(ctx, agent)
					case "activity_response":
						result, callErr = s.recentActivities(ctx, 1)
					case "slash_location":
						_, callErr = s.slashLocation(ctx, agent.Agent, conversation.ProjectContext{}, sessionID)
					case "finished_response":
						response, callErr = s.captureFinishedPane(ctx, "pane-1", agent.Agent, "", sessionID)
					}
				}()
				started = true
				select {
				case admitted := <-entered:
					if admitted == ctx || admitted.Err() != nil {
						t.Fatalf("discovery did not enter a live installed call: %v", admitted.Err())
					}
				case <-time.After(10 * time.Second):
					t.Fatal("server caller did not reach installed discovery")
				}
				if mode == "owner_retire" {
					if ctx.Err() != nil {
						t.Fatalf("caller canceled before owner retirement: %v", ctx.Err())
					}
					owner.Begin()
				} else {
					cancel()
				}
				select {
				case err := <-retired:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("admitted discovery cancellation = %v", err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("admitted discovery did not observe cancellation")
				}
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Fatal("server caller did not retire")
				}
				if mode == "owner_retire" && ctx.Err() != nil {
					t.Fatalf("owner retirement canceled the original caller: %v", ctx.Err())
				}
				if !errors.Is(callErr, context.Canceled) || agent.SessionName != "" || agent.Session != sessionID || len(result) != 0 || response != "" {
					t.Fatalf("terminal call = %v; title=%q session=%q activities=%v response=%q", callErr, agent.SessionName, agent.Session, result, response)
				}
			})
		}
	}
}
