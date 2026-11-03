package herdr

import (
	"context"
	"fmt"
)

type TopologyStream interface {
	Next(ctx context.Context) (TopologySnapshot, error)
	Close() error
}

type topologyStream struct {
	stream   *EventStream
	cache    *SessionCache
	buffered []Event
}

func (c *Client) Events(ctx context.Context) (TopologySnapshot, TopologyStream, error) {
	events := NewEventClient(c.socketPath)
	events.SetWorkspaceReorderedCapability(
		c.ShouldAttemptWorkspaceReordered,
		c.NoteWorkspaceReorderedSupported,
		c.NoteWorkspaceReorderedUnsupported,
	)
	events.SetWorkspaceReorderedReset(c.InvalidateLiveCapabilities)
	stream, snapshot, buffered, err := events.Bootstrap(ctx)
	if err != nil {
		return TopologySnapshot{}, nil, err
	}
	cache := NewSessionCache(snapshot)
	return cache.Snapshot(), &topologyStream{stream: stream, cache: cache, buffered: buffered}, nil
}

func (s *topologyStream) Next(ctx context.Context) (TopologySnapshot, error) {
	for {
		var event Event
		if len(s.buffered) > 0 {
			event = s.buffered[0]
			s.buffered = s.buffered[1:]
		} else {
			var err error
			event, err = s.stream.Next(ctx)
			if err != nil {
				return TopologySnapshot{}, err
			}
		}
		changed, err := s.cache.Apply(event)
		if err != nil {
			return TopologySnapshot{}, fmt.Errorf("Herdr topology event %s decode failed: %w", event.Event, err)
		}
		if changed {
			return s.cache.Snapshot(), nil
		}
	}
}

func (s *topologyStream) Close() error {
	return s.stream.Close()
}
