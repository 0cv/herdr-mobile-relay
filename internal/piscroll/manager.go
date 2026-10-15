package piscroll

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/panesize"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

type observation struct {
	target          Target
	evidence        Evidence
	epoch           uint64
	checked         time.Time
	reason          string
	invalidGeometry bool
}
type choice struct {
	identity  string
	selection string
	sequence  uint64
}

type Manager struct {
	mu       sync.Mutex
	resolver Resolver
	salt     string
	panes    map[string]observation
	choices  map[string]map[string]choice
	flights  map[string]bool
	slots    chan struct{}
	epoch    uint64
}

func New(resolver Resolver) *Manager {
	var salt [16]byte
	_, _ = rand.Read(salt[:])
	return &Manager{resolver: resolver, salt: hex.EncodeToString(salt[:]), panes: map[string]observation{}, choices: map[string]map[string]choice{}, flights: map[string]bool{}, slots: make(chan struct{}, 8)}
}

func (m *Manager) Begin(pane string) (func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.flights[pane] {
		return nil, errors.New("scroll busy")
	}
	select {
	case m.slots <- struct{}{}:
	default:
		return nil, errors.New("scroll capacity busy")
	}
	m.flights[pane] = true
	return func() { m.mu.Lock(); delete(m.flights, pane); <-m.slots; m.mu.Unlock() }, nil
}

func processIdentity(target Target, e Evidence) string {
	data, _ := json.Marshal([]any{target, e.Instance, e.Incarnation, e.PID})
	return string(data)
}

func (m *Manager) stateLocked(target Target, owner string) protocol.ScrollState {
	o := m.panes[target.Ref.PaneID]
	if o.target != target {
		o = observation{target: target, reason: "not_checked"}
	}
	selection := "auto"
	var sequence uint64
	if c, ok := m.choices[owner][target.Ref.PaneID]; ok && c.identity == processIdentity(target, o.evidence) {
		selection, sequence = c.selection, c.sequence
	}
	s := protocol.ScrollState{Selection: selection, Renderer: o.evidence.Renderer, Wheel: o.evidence.Wheel, Source: "unknown", Route: "local", Presentation: "history", Reason: o.reason, Rows: o.evidence.Grid.Rows, Columns: o.evidence.Grid.Columns, GeometryEpoch: o.epoch}
	if s.Renderer == "" {
		s.Renderer = "unknown"
	}
	if s.Wheel == "" {
		s.Wheel = "unknown"
	}
	if o.evidence.Incarnation != "" {
		s.Source = "pi_runtime"
	}
	if s.Renderer == "fullscreen" {
		s.Presentation = "live_screen"
	}
	if s.Reason == "" {
		s.Reason = "ui_unavailable"
		if s.Renderer == "regular" {
			s.Reason = "pi_regular"
		}
		if o.invalidGeometry {
			s.Reason = "geometry_unavailable"
		}
		if s.Renderer == "fullscreen" && s.Wheel == "sgr_cell" && s.Source == "pi_runtime" && o.evidence.Instance != "" && o.evidence.PID > 0 && s.Rows > 0 && s.Columns > 0 && s.Rows <= panesize.MaxCellDimension && s.Columns <= panesize.MaxCellDimension && !o.invalidGeometry {
			s.Available = true
			s.Reason = "pi_fullscreen"
			if selection == "auto" {
				s.Route = "application"
			}
		}
	}
	if selection == "local" {
		s.Reason = "local_view"
		s.Available = false
	}
	data, _ := json.Marshal([]any{m.salt, target, processIdentity(target, o.evidence), s, sequence, o.invalidGeometry})
	hash := sha256.Sum256(data)
	s.Revision = hex.EncodeToString(hash[:])
	return s
}

func (m *Manager) Snapshot(target Target, owner string) protocol.ScrollState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stateLocked(target, owner)
}

func (m *Manager) Refresh(ctx context.Context, target Target, owner string) protocol.ScrollState {
	m.mu.Lock()
	o := m.panes[target.Ref.PaneID]
	fresh := o.target == target && time.Since(o.checked) < 500*time.Millisecond
	measure := o.target != target || o.evidence.Grid.Rows == 0 || o.invalidGeometry
	m.mu.Unlock()
	if !fresh && IsPi(target.Agent) {
		if release, err := m.Begin(target.Ref.PaneID); err == nil {
			defer release()
			m.Validate(ctx, target, measure)
		}
	}
	return m.Snapshot(target, owner)
}

func (m *Manager) Validate(ctx context.Context, target Target, measure bool) error {
	e, err := m.resolver.Resolve(ctx, target, measure)
	m.mu.Lock()
	defer m.mu.Unlock()
	pane := target.Ref.PaneID
	old := m.panes[pane]
	identityChanged := old.target != target || processIdentity(target, old.evidence) != processIdentity(target, e)
	if err != nil {
		e = Evidence{}
	}
	if !measure && !identityChanged {
		e.Grid = old.evidence.Grid
	}
	geometryChanged := identityChanged || old.evidence.Grid != e.Grid || old.invalidGeometry
	if geometryChanged {
		m.epoch++
	}
	o := observation{target: target, evidence: e, epoch: old.epoch, checked: time.Now()}
	if geometryChanged {
		o.epoch = m.epoch
	}
	if err != nil {
		o.reason = "runtime_unavailable"
	}
	if identityChanged || err != nil {
		for owner, values := range m.choices {
			delete(values, pane)
			if len(values) == 0 {
				delete(m.choices, owner)
			}
		}
	}
	m.panes[pane] = o
	return err
}

func (m *Manager) SelectAtRevision(target Target, owner, selection, revision string) (protocol.ScrollState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if owner == "" || !IsPi(target.Agent) || (selection != "auto" && selection != "local") {
		return protocol.ScrollState{}, errors.New("unsupported scroll selection")
	}
	o := m.panes[target.Ref.PaneID]
	if revision == "" || m.stateLocked(target, owner).Revision != revision {
		return m.stateLocked(target, owner), errors.New("scroll evidence changed")
	}
	if o.target != target || o.evidence.Incarnation == "" {
		return m.stateLocked(target, owner), errors.New("Pi runtime unavailable")
	}
	if m.choices[owner] == nil {
		m.choices[owner] = map[string]choice{}
	}
	previous := m.choices[owner][target.Ref.PaneID]
	m.choices[owner][target.Ref.PaneID] = choice{identity: processIdentity(target, o.evidence), selection: selection, sequence: previous.sequence + 1}
	return m.stateLocked(target, owner), nil
}

func (m *Manager) ForgetOwner(owner string) { m.mu.Lock(); delete(m.choices, owner); m.mu.Unlock() }

func (m *Manager) InvalidateGeometry(pane string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.panes[pane]
	if !ok {
		return
	}
	m.epoch++
	o.epoch = m.epoch
	o.invalidGeometry = true
	o.checked = time.Time{}
	m.panes[pane] = o
}

func (m *Manager) Refuse(target Target) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o := m.panes[target.Ref.PaneID]
	o.reason = "delivery_unconfirmed"
	o.checked = time.Time{}
	m.panes[target.Ref.PaneID] = o
}

func Report(direction string, grid panesize.Grid) (string, error) {
	if direction != "up" && direction != "down" || grid.Rows < 1 || grid.Columns < 1 || grid.Rows > panesize.MaxCellDimension || grid.Columns > panesize.MaxCellDimension {
		return "", errors.New("invalid wheel intent or geometry")
	}
	button := 64
	if direction == "down" {
		button = 65
	}
	return fmt.Sprintf("\x1b[<%d;%d;%dM", button, max(1, (grid.Columns+1)/2), max(1, (grid.Rows+1)/2)), nil
}
