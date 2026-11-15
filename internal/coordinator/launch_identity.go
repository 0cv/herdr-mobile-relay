package coordinator

type LaunchIdentity struct {
	Generation int64
	TerminalID string
	SessionID  string
	Valid      bool
}

type launchSnapshot struct {
	generations map[string]int64
	terminals   map[string]string
}

type sessionDiscovery struct {
	generation int64
	terminalID string
	sessionID  string
}

func (s *State) beginLaunch() launchSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot := launchSnapshot{generations: make(map[string]int64, len(s.generation)), terminals: make(map[string]string, len(s.panes))}
	for pane, generation := range s.generation {
		snapshot.generations[pane] = generation
	}
	for pane, rec := range s.panes {
		snapshot.terminals[pane] = rec.agent.TerminalID
	}
	return snapshot
}

func (s *State) launchIdentityCurrent(pane string, identity LaunchIdentity) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !identity.Valid || s.generation[pane] != identity.Generation {
		return ErrPaneReplaced
	}
	if rec := s.panes[pane]; rec != nil && identity.TerminalID != "" && rec.agent.TerminalID != identity.TerminalID {
		return ErrPaneReplaced
	}
	return nil
}

func (s *State) finishLaunch(snapshot launchSnapshot, pane string) LaunchIdentity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	baseline := snapshot.generations[pane]
	generation := s.generation[pane]
	identity := LaunchIdentity{Generation: baseline}
	rec := s.panes[pane]
	if rec != nil {
		identity.TerminalID = rec.agent.TerminalID
		identity.SessionID = rec.agent.SessionID
	}
	if generation == baseline {
		identity.Valid = true
		return identity
	}
	if rec == nil || rec.sessionDiscovery == nil {
		return identity
	}
	discovery := rec.sessionDiscovery
	if discovery.generation != baseline || generation != baseline+1 ||
		discovery.terminalID != rec.agent.TerminalID || discovery.sessionID != rec.agent.SessionID {
		return identity
	}
	if terminal, existed := snapshot.terminals[pane]; existed && terminal != discovery.terminalID {
		return identity
	}
	identity.Generation = generation
	identity.Valid = true
	return identity
}
