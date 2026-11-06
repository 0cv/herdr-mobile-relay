package hc

import (
	"context"
	"sync"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/panesize"
)

type sizedPane struct {
	terminalID                  string
	attach                      *attachment
	leases                      map[string]panesize.Lease
	baselineRows, columns, rows int
	resizedAt                   time.Time
}

type PaneSizer struct {
	mu     sync.Mutex
	client *Client
	panes  map[string]*sizedPane
	gone   map[string]bool
	closed bool
	now    func() time.Time
}

func NewPaneSizer(client *Client) *PaneSizer {
	return &PaneSizer{client: client, panes: make(map[string]*sizedPane), gone: make(map[string]bool), now: time.Now}
}

func (m *PaneSizer) Available() bool { return m != nil && m.client != nil }

func (s *sizedPane) target(now time.Time) (int, int) {
	cols, rows := 0, 0
	for _, l := range s.leases {
		if !l.ExpiresAt.After(now) {
			continue
		}
		if cols == 0 || l.Columns < cols {
			cols = l.Columns
		}
		if l.Rows > 0 && (rows == 0 || l.Rows < rows) {
			rows = l.Rows
		}
	}
	// Core attaches always specify both dimensions: width-only leases retain
	// the acquisition height, rather than following later desktop height changes.
	if rows == 0 {
		rows = s.baselineRows
	}
	return cols, rows
}

func (m *PaneSizer) drop(id string, gone bool) {
	if s := m.panes[id]; s != nil {
		s.attach.Close()
		delete(m.panes, id)
	}
	if gone {
		m.gone[id] = true
	}
}

func (m *PaneSizer) validate(ctx context.Context, id string, s *sizedPane) error {
	if s.attach.ended() {
		m.drop(id, true)
		return panesize.ErrTerminalGone
	}
	p, err := m.client.PaneGet(ctx, id)
	if herdr.IsPaneNotFound(err) || (err == nil && p.TerminalID != s.terminalID) {
		m.drop(id, true)
		return panesize.ErrTerminalGone
	}
	return err
}

func (m *PaneSizer) reconcile(id string, s *sizedPane) error {
	cols, rows := s.target(m.now())
	if cols == 0 {
		m.drop(id, false)
		return nil
	}
	if cols == s.columns && rows == s.rows {
		return nil
	}
	if err := s.attach.Resize(cols, rows); err != nil {
		m.drop(id, true)
		return panesize.ErrTerminalGone
	}
	s.columns, s.rows, s.resizedAt = cols, rows, m.now()
	return nil
}

func (m *PaneSizer) Acquire(ctx context.Context, clientID, paneID string, columns, rows int) (int, int, error) {
	if !m.Available() {
		return 0, 0, panesize.ErrUnavailable
	}
	if clientID == "" || paneID == "" {
		return 0, 0, panesize.ErrInvalidLease
	}
	if columns < panesize.MinColumns || columns > panesize.MaxColumns {
		return 0, 0, panesize.ErrInvalidColumns
	}
	if rows != 0 && (rows < panesize.MinRows || rows > panesize.MaxRows) {
		return 0, 0, panesize.ErrInvalidRows
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, 0, panesize.ErrClosed
	}
	if ctx.Err() != nil {
		return 0, 0, panesize.ErrLeaseOwnerGone
	}
	if m.gone[paneID] {
		delete(m.gone, paneID)
		return 0, 0, panesize.ErrTerminalGone
	}
	opCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	s := m.panes[paneID]
	if s != nil {
		if err := m.validate(opCtx, paneID, s); err != nil {
			delete(m.gone, paneID)
			return 0, 0, err
		}
	} else {
		t, err := m.client.getTerminal(opCtx, paneID)
		if err != nil {
			return 0, 0, err
		}
		if t.Rows <= 0 || t.Rows > 65535 {
			return 0, 0, panesize.ErrSizeUnavailable
		}
		s = &sizedPane{terminalID: m.client.pane(*t).TerminalID, baselineRows: t.Rows, columns: t.Cols, rows: t.Rows, leases: make(map[string]panesize.Lease)}
	}
	s.leases[clientID] = panesize.Lease{Columns: columns, Rows: rows, ExpiresAt: m.now().Add(panesize.LeaseTTL)}
	cols, height := s.target(m.now())
	if s.attach == nil {
		a, err := m.client.attach(opCtx, paneID, cols, height)
		if err != nil {
			return 0, 0, err
		}
		s.attach = a
		if cols != s.columns || height != s.rows {
			s.resizedAt = m.now()
		}
		s.columns, s.rows = cols, height
		m.panes[paneID] = s
	} else if err := m.reconcile(paneID, s); err != nil {
		delete(m.gone, paneID)
		return 0, 0, err
	}
	if ctx.Err() != nil {
		delete(s.leases, clientID)
		_ = m.reconcile(paneID, s)
		return 0, 0, panesize.ErrLeaseOwnerGone
	}
	return cols, height, nil
}

func (m *PaneSizer) Release(ctx context.Context, clientID, paneID string, immediate bool) error {
	if clientID == "" || paneID == "" {
		return panesize.ErrInvalidLease
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gone[paneID] {
		delete(m.gone, paneID)
		return panesize.ErrTerminalGone
	}
	s := m.panes[paneID]
	if s == nil {
		return nil
	}
	opCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := m.validate(opCtx, paneID, s); err != nil {
		delete(m.gone, paneID)
		return err
	}
	if l, ok := s.leases[clientID]; ok {
		if immediate {
			delete(s.leases, clientID)
		} else {
			if end := m.now().Add(panesize.ReleaseGrace); l.ExpiresAt.After(end) {
				l.ExpiresAt = end
				s.leases[clientID] = l
			}
		}
	}
	return m.reconcile(paneID, s)
}

func (m *PaneSizer) ReleaseClient(ctx context.Context, clientID string) error {
	if clientID == "" {
		return panesize.ErrInvalidLease
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.panes {
		if _, ok := s.leases[clientID]; !ok {
			continue
		}
		delete(s.leases, clientID)
		if cols, _ := s.target(m.now()); cols == 0 {
			m.drop(id, false)
			continue
		}
		opCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if m.validate(opCtx, id, s) == nil {
			_ = m.reconcile(id, s)
		}
		cancel()
	}
	return nil
}

func (m *PaneSizer) sweep(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.panes {
		if s.attach.ended() {
			m.drop(id, true)
			continue
		}
		for owner, l := range s.leases {
			if !l.ExpiresAt.After(m.now()) {
				delete(s.leases, owner)
			}
		}
		if cols, _ := s.target(m.now()); cols == 0 {
			m.drop(id, false)
			continue
		}
		opCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if m.validate(opCtx, id, s) == nil {
			_ = m.reconcile(id, s)
		}
		cancel()
	}
}

func (m *PaneSizer) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			m.sweep(ctx)
		}
	}
}

func (m *PaneSizer) Shutdown(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for id := range m.panes {
		m.drop(id, false)
	}
	return nil
}

func (m *PaneSizer) ReadGeometry(clientID, paneID string) (int, int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.panes[paneID]
	if m.closed || s == nil {
		return 0, 0, false
	}
	if s.attach.ended() {
		m.drop(paneID, true)
		return 0, 0, false
	}
	if l, ok := s.leases[clientID]; !ok || !l.ExpiresAt.After(m.now()) {
		return 0, 0, false
	}
	cols, rows := s.target(m.now())
	return cols, rows, true
}

func (m *PaneSizer) ResizedWithin(paneID string, window time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.panes[paneID]
	return !m.closed && s != nil && !s.resizedAt.IsZero() && m.now().Sub(s.resizedAt) < window
}
