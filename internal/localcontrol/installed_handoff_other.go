//go:build !linux && !darwin

package localcontrol

import (
	"context"
	"errors"

	"github.com/0cv/herdr-mobile-relay/internal/config"
)

// Unsupported hosts have no filesystem policy or fallback implementation.
type InstalledHandoffOutcome string

const (
	InstalledHandoffUnpublished InstalledHandoffOutcome = "unpublished"
	InstalledHandoffPublished   InstalledHandoffOutcome = "published"
	InstalledHandoffUncertain   InstalledHandoffOutcome = "uncertain"
)

var errUnsupportedHandoff = errors.New("installed handoff unsupported")

type InstalledHandoff struct{}

func (InstalledHandoff) String() string     { return "installed handoff [redacted]" }
func (h InstalledHandoff) GoString() string { return h.String() }
func PrepareInstalledHandoff(context.Context, config.InstalledConfig, InstalledCredentials) (InstalledHandoff, error) {
	return InstalledHandoff{}, errUnsupportedHandoff
}
func (InstalledHandoff) SocketPath(context.Context) (string, error) { return "", errUnsupportedHandoff }
func (InstalledHandoff) PublishDiscovery(context.Context) (InstalledHandoffOutcome, error) {
	return InstalledHandoffUnpublished, errUnsupportedHandoff
}
func (InstalledHandoff) Close() error { return nil }

type InstalledHandoffSnapshot struct{}

func (InstalledHandoffSnapshot) String() string                    { return "installed handoff snapshot [redacted]" }
func (s InstalledHandoffSnapshot) GoString() string                { return s.String() }
func (InstalledHandoffSnapshot) Credentials() InstalledCredentials { return InstalledCredentials{} }
func (InstalledHandoffSnapshot) SocketPath() string                { return "" }
func ReadInstalledHandoff(context.Context, config.InstalledConfig) (InstalledHandoffSnapshot, error) {
	return InstalledHandoffSnapshot{}, errUnsupportedHandoff
}
