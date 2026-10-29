package session

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
	"github.com/0cv/herdr-mobile-relay/internal/conversationlife"
)

// NewInstalledResolverWithReader uses the selected Reader location and the
// service's single owner; no independent root or ordinary title open exists.
func NewInstalledResolverWithReader(ctx context.Context, owner *conversationlife.Owner, reader *conversation.Reader) (*Resolver, error) {
	if owner == nil || reader == nil || reader.InstalledOwner() != owner {
		return nil, conversationlife.ErrRefused
	}
	var result *Resolver
	err := owner.Do(ctx, func(call *conversationlife.Call) error {
		if err := call.Context().Err(); err != nil {
			return err
		}
		result = &Resolver{cache: make(map[string]cacheEntry), reader: reader, owner: owner}
		return call.Context().Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (r *Resolver) SessionNameWithProjectContext(ctx context.Context, agent string, project conversation.ProjectContext, sessionID string) (name string, err error) {
	if r == nil || r.owner == nil || r.reader == nil || r.reader.InstalledOwner() != r.owner {
		return "", conversationlife.ErrRefused
	}
	err = r.owner.Do(ctx, func(call *conversationlife.Call) error {
		var e error
		name, e = r.sessionNameIn(call, agent, project, sessionID)
		return e
	})
	if err != nil {
		return "", err
	}
	return name, nil
}

func (r *Resolver) SessionNameWithProjectIn(parent *conversationlife.Call, agent string, project conversation.ProjectContext, sessionID string) (name string, err error) {
	if r == nil || r.owner == nil || r.reader == nil || r.reader.InstalledOwner() != r.owner {
		return "", conversationlife.ErrRefused
	}
	err = r.owner.DoIn(parent, func(call *conversationlife.Call) error {
		var e error
		name, e = r.sessionNameIn(call, agent, project, sessionID)
		return e
	})
	if err != nil {
		return "", err
	}
	return name, nil
}

func (r *Resolver) sessionNameIn(parent *conversationlife.Call, agent string, project conversation.ProjectContext, sessionID string) (name string, err error) {
	err = r.owner.DoIn(parent, func(call *conversationlife.Call) error {
		ctx := call.Context()
		project = conversation.NormalizeProjectContext(agent, project)
		sessionID = strings.TrimSpace(sessionID)
		if err := ctx.Err(); err != nil {
			return err
		}
		if sessionID == "" {
			return nil
		}
		lower := strings.ToLower(strings.TrimSpace(agent))
		key := sessionCacheKey(lower, project, sessionID)
		location, err := r.reader.LocateWithProjectIn(call, agent, project, sessionID)
		if err != nil {
			return err
		}
		if location.Path == "" {
			return nil
		}
		now := time.Now()
		r.mu.Lock()
		cached, hit := r.cache[key]
		r.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		if hit && cached.location == location && now.Before(cached.expires) {
			name = cached.name
			return nil
		}
		if isHermesSessionAgent(lower) {
			name = location.Title
		} else {
			index := strings.Contains(lower, "codex")
			data, err := r.reader.ReadSelectedContentIn(call, location, index)
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			reader := bytes.NewReader(data)
			switch {
			case isOMPSessionAgent(lower):
				name = extractOMPSessionTitleReader(reader)
			case isPiSessionAgent(lower):
				name = extractPiSessionTitleReader(reader)
			case strings.Contains(lower, "qoder"), strings.Contains(lower, "claude"):
				name = extractTitleReader(reader)
			case index:
				name = codexIndexThreadNameReader(reader, sessionID)
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		done := ctx.Done()
		expiry := now.Add(cacheTTL)
		published := false
		defer func() {
			cancelled := ctx.Err() != nil
			if published && cancelled {
				r.mu.Lock()
				if entry, ok := r.cache[key]; ok && entry.expires == expiry {
					delete(r.cache, key)
				}
				r.mu.Unlock()
			}
		}()
		r.mu.Lock()
		select {
		case <-done:
		default:
			r.cache[key] = cacheEntry{name: name, location: location, expires: expiry}
			published = true
		}
		r.mu.Unlock()
		return ctx.Err()
	})
	if err != nil {
		return "", errors.Join(err, parent.Context().Err())
	}
	return name, nil
}
