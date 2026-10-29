package app

import (
	"context"
	"os"
	"path/filepath"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
	"github.com/0cv/herdr-mobile-relay/internal/session"
	"github.com/0cv/herdr-mobile-relay/internal/sqliteexec"
)

// The Run/outer wrapper pair collects each cleanup witness once, after browser
// work and resources have physically retired. Join alone is not finalization.
func (s *Server) collectInstalledConversation() (result error) {
	if s == nil || s.conversationOwner == nil {
		return nil
	}
	s.conversationOwner.Begin()
	s.conversationOwner.Join()
	s.conversationCollect.Do(func() { result = s.conversationOwner.Witnesses() })
	return result
}

// Installed setup and STRICT service share this acquisition. Environment is
// selection data only; the executor's concrete lifetime comes from owner.
func acquireInstalledConversation(ctx context.Context, home, cacheDir string, owner *conversationlife.Owner) (*conversation.Reader, *conversation.Browser, *session.Resolver, error) {
	backend := sqliteexec.ParseBackend(os.Getenv(sqliteexec.EnvBackend))
	openCode, err := sqliteexec.NewInstalled(ctx, owner, backend, "sqlite3")
	if err != nil {
		return nil, nil, nil, err
	}
	hermes, err := sqliteexec.NewInstalled(ctx, owner, backend, "sqlite3")
	if err != nil {
		return nil, nil, nil, err
	}
	reader, err := conversation.NewInstalledReader(ctx, home, owner, openCode, hermes)
	if err != nil {
		return nil, nil, nil, err
	}
	cacheRoot := ""
	if cacheDir != "" {
		cacheRoot = filepath.Join(cacheDir, "conversation-history")
	}
	browser, err := conversation.NewInstalledBrowser(ctx, reader, owner, cacheRoot, conversation.DefaultBrowserOptions())
	if err != nil {
		return nil, nil, nil, err
	}
	resolver, err := session.NewInstalledResolverWithReader(ctx, owner, reader)
	if err != nil {
		_ = browser.Close()
		return nil, nil, nil, err
	}
	return reader, browser, resolver, nil
}
