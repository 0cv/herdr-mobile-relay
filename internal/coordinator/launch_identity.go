package coordinator

type LaunchIdentity struct {
	Generation uint64
	TerminalID string
	SessionID  string
	Valid      bool
}

type launchSnapshot struct {
	generations map[string]uint64
	terminals   map[string]string
}

type sessionDiscovery struct {
	generation uint64
	terminalID string
	sessionID  string
}

func (s *State) beginLaunch() launchSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot := launchSnapshot{generations: make(map[string]uint64, len(s.generation)), terminals: make(map[string]string, len(s.agents))}
	for pane, generation := range s.generation {
		snapshot.generations[pane] = uint64(generation)
	}
	for pane, agent := range s.agents {
		snapshot.terminals[pane] = agent.TerminalID
	}
	return snapshot
}

func (s *State) launchIdentityCurrent(pane string, identity LaunchIdentity) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !identity.Valid || uint64(s.generation[pane]) != identity.Generation {
		return ErrPaneReplaced
	}
	if agent := s.agents[pane]; agent != nil && identity.TerminalID != "" && agent.TerminalID != identity.TerminalID {
		return ErrPaneReplaced
	}
	return nil
}

func (s *State) finishLaunch(snapshot launchSnapshot, pane string) LaunchIdentity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	baseline := snapshot.generations[pane]
	generation := uint64(s.generation[pane])
	identity := LaunchIdentity{Generation: baseline}
	agent := s.agents[pane]
	if agent != nil {
		identity.TerminalID = agent.TerminalID
		identity.SessionID = agent.SessionID
	}
	if generation == baseline {
		identity.Valid = true
		return identity
	}
	discovery, found := s.sessionDiscoveries[pane]
	if !found || agent == nil || discovery.generation != baseline || generation != baseline+1 ||
		discovery.terminalID != agent.TerminalID || discovery.sessionID != agent.SessionID {
		return identity
	}
	if terminal, existed := snapshot.terminals[pane]; existed && terminal != discovery.terminalID {
		return identity
	}
	identity.Generation = generation
	identity.Valid = true
	return identity
}
