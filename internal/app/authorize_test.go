package app

import (
	"encoding/json"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/coordinator"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func TestAuthorizeAuthenticatedIdentity(t *testing.T) {
	mutation := protocol.ActionMetadata{Operation: "send_input", Class: protocol.ActionMutating}
	filter := protocol.ActionMetadata{Operation: "send_filter_text", Class: protocol.ActionMutating}
	read := protocol.ActionMetadata{Operation: "read_pane", Class: protocol.ActionReadOnly}

	for _, test := range []struct {
		name          string
		identity      transport.AuthenticatedIdentity
		authenticated bool
		action        protocol.ActionMetadata
		deviceID      string
		wantDenied    bool
	}{
		{name: "reader mutation", identity: transport.AuthenticatedIdentity{Role: string(protocol.RoleReader)}, authenticated: true, action: mutation, wantDenied: true},
		{name: "reader read", identity: transport.AuthenticatedIdentity{Role: string(protocol.RoleReader)}, authenticated: true, action: read},
		{name: "reader filter denied", identity: transport.AuthenticatedIdentity{DeviceID: "device-current", Role: string(protocol.RoleReader)}, authenticated: true, action: filter, wantDenied: true},
		{name: "controller filter allowed", identity: transport.AuthenticatedIdentity{DeviceID: "device-current", Role: string(protocol.RoleController)}, authenticated: true, action: filter},
		{name: "reader self revoke", identity: transport.AuthenticatedIdentity{DeviceID: "device-current", Role: string(protocol.RoleReader)}, authenticated: true, action: protocol.ActionMetadata{Operation: "revoke_device", Class: protocol.ActionMutating}, deviceID: "device-current"},
		{name: "reader other revoke", identity: transport.AuthenticatedIdentity{DeviceID: "device-current", Role: string(protocol.RoleReader)}, authenticated: true, action: protocol.ActionMetadata{Operation: "revoke_device", Class: protocol.ActionMutating}, deviceID: "device-other", wantDenied: true},
		{name: "controller mutation", identity: transport.AuthenticatedIdentity{Role: string(protocol.RoleController)}, authenticated: true, action: mutation},
		{name: "local connection", action: mutation},
		{name: "unauthenticated push subscribe", action: protocol.ActionMetadata{Operation: "push_subscribe", Class: protocol.ActionMutating}, wantDenied: true},
		{name: "reader own-device push policy", identity: transport.AuthenticatedIdentity{DeviceID: "device-current", Role: string(protocol.RoleReader)}, authenticated: true, action: protocol.ActionMetadata{Operation: "push_policy_set", Class: protocol.ActionMutating}, deviceID: "spoofed-other"},
		{name: "unauthenticated push open", action: protocol.ActionMetadata{Operation: "push_open_ref", Class: protocol.ActionReadOnly}, wantDenied: true},
		{name: "reader push test", identity: transport.AuthenticatedIdentity{DeviceID: "device-current", Role: string(protocol.RoleReader)}, authenticated: true, action: protocol.ActionMetadata{Operation: "push_test_device", Class: protocol.ActionMutating}},
		{name: "unauthenticated push test", action: protocol.ActionMetadata{Operation: "push_test_device", Class: protocol.ActionMutating}, wantDenied: true},
		{name: "controller push test", identity: transport.AuthenticatedIdentity{DeviceID: "device-current", Role: string(protocol.RoleController)}, authenticated: true, action: protocol.ActionMetadata{Operation: "push_test_device", Class: protocol.ActionMutating}},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := authorizeAuthenticatedIdentity(test.identity, test.authenticated, test.action, test.deviceID)
			if (err != nil) != test.wantDenied {
				t.Fatalf("authorizeAuthenticatedIdentity() error = %#v, want denied %v", err, test.wantDenied)
			}
			if err != nil && (err.Code != protocol.ErrorReaderDenied || err.Args["operation"] != test.action.Operation) {
				t.Fatalf("authorizeAuthenticatedIdentity() error = %#v", err)
			}
		})
	}
}

func TestValidateExactPaneTargetBindsCurrentTerminalGenerationAndAgentSession(t *testing.T) {
	server := testServer()
	server.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", ServerSessionID: "primary", TerminalID: "terminal-1",
		Generation: 4, SessionID: "agent-session-1",
	}}, server.state.RevisionCounter())
	current, ok := server.state.Agent("pane-1")
	if !ok {
		t.Fatal("committed agent missing")
	}
	exact := protocol.TargetRef{
		ServerSessionID: current.ServerSessionID, PaneID: current.PaneID, TerminalID: current.TerminalID,
		Generation: current.Generation, AgentSessionID: current.SessionID,
	}
	if err := validateExactPaneTarget(server.state, protocol.Inbound{PaneID: "pane-1", Target: &exact}, true); err != nil {
		t.Fatalf("exact target rejected: %#v", err)
	}
	for name, mutate := range map[string]func(*protocol.TargetRef){
		"server session": func(target *protocol.TargetRef) { target.ServerSessionID = "other" },
		"pane":           func(target *protocol.TargetRef) { target.PaneID = "pane-2" },
		"terminal":       func(target *protocol.TargetRef) { target.TerminalID = "terminal-2" },
		"generation":     func(target *protocol.TargetRef) { target.Generation++ },
		"agent session":  func(target *protocol.TargetRef) { target.AgentSessionID = "agent-session-2" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := exact
			mutate(&changed)
			if err := validateExactPaneTarget(server.state, protocol.Inbound{PaneID: "pane-1", Target: &changed}, true); err == nil {
				t.Fatal("stale target was accepted")
			}
		})
	}
	if err := validateExactPaneTarget(server.state, protocol.Inbound{PaneID: "pane-1"}, true); err == nil {
		t.Fatal("authenticated pane command without a target was accepted")
	}
	if err := validateExactPaneTarget(server.state, protocol.Inbound{PaneID: "pane-1"}, false); err != nil {
		t.Fatalf("local pane command without a target was rejected: %#v", err)
	}
}

func TestPaneTargetValidatorsAgreeOnWhatIsCurrent(t *testing.T) {
	server := testServer()
	server.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", TerminalID: "terminal-1", Generation: 4, SessionID: "agent-session-1",
	}}, server.state.RevisionCounter())
	current, ok := server.state.Agent("pane-1")
	if !ok {
		t.Fatal("committed agent missing")
	}
	exact := protocol.TargetRef{
		ServerSessionID: protocol.PrimaryServerSessionID, PaneID: current.PaneID, TerminalID: current.TerminalID,
		Generation: current.Generation, AgentSessionID: current.SessionID,
	}
	validators := map[string]func(protocol.TargetRef) bool{
		"exact pane target": func(target protocol.TargetRef) bool {
			return validateExactPaneTarget(server.state, protocol.Inbound{PaneID: "pane-1", Target: &target}, true) == nil
		},
		"push target": server.pushTargetCurrent,
		"upload target": func(target protocol.TargetRef) bool {
			return server.validateUploadTarget(target) == nil
		},
	}
	cases := map[string]struct {
		mutate  func(*protocol.TargetRef)
		current bool
	}{
		"exact":                {func(*protocol.TargetRef) {}, true},
		"other server session": {func(target *protocol.TargetRef) { target.ServerSessionID = "other" }, false},
		"empty server session": {func(target *protocol.TargetRef) { target.ServerSessionID = "" }, false},
		"other pane":           {func(target *protocol.TargetRef) { target.PaneID = "pane-2" }, false},
		"empty terminal":       {func(target *protocol.TargetRef) { target.TerminalID = "" }, false},
		"other terminal":       {func(target *protocol.TargetRef) { target.TerminalID = "terminal-2" }, false},
		"older generation":     {func(target *protocol.TargetRef) { target.Generation-- }, false},
		"newer generation":     {func(target *protocol.TargetRef) { target.Generation++ }, false},
		"negative generation":  {func(target *protocol.TargetRef) { target.Generation = -1 }, false},
		"other agent session":  {func(target *protocol.TargetRef) { target.AgentSessionID = "agent-session-2" }, false},
	}
	for validatorName, validator := range validators {
		for caseName, tc := range cases {
			t.Run(validatorName+"/"+caseName, func(t *testing.T) {
				target := exact
				tc.mutate(&target)
				if got := validator(target); got != tc.current {
					t.Fatalf("current = %v, want %v", got, tc.current)
				}
			})
		}
	}
}

func TestValidateExactPaneTargetAllowsStaleOwnerCleanup(t *testing.T) {
	server := testServer()
	server.state.CommitInventory([]*coordinator.AgentState{{
		PaneID: "pane-1", ServerSessionID: "primary", TerminalID: "terminal-new",
		Generation: 5, SessionID: "agent-session-new",
	}}, server.state.RevisionCounter())
	stale := protocol.TargetRef{
		ServerSessionID: "primary", PaneID: "pane-1", TerminalID: "terminal-old",
		Generation: 4, AgentSessionID: "agent-session-old",
	}
	for _, action := range []string{"unwatch_pane", "release_pane_size", "cancel_speech"} {
		if err := validateExactPaneTarget(server.state, protocol.Inbound{
			Type: action, PaneID: "pane-1", Target: &stale,
		}, true); err != nil {
			t.Fatalf("%s rejected stale cleanup target: %#v", action, err)
		}
	}
	stale.PaneID = "pane-2"
	if err := validateExactPaneTarget(server.state, protocol.Inbound{
		Type: "unwatch_pane", PaneID: "pane-1", Target: &stale,
	}, true); err == nil {
		t.Fatal("cleanup accepted a different pane")
	}
}

// The agents broadcast deep-copies snapshots through JSON before projecting
// wire identity, and the phone can only echo what that copy advertises. The
// identity that survives the round trip must satisfy validateExactPaneTarget,
// or every command against an agent with a resolved session is rejected -
// exactly the shape of the field failure this test was written after: the
// internal SessionID is json:"-", so it silently vanished from the broadcast
// and phones echoed an empty agent_session_id forever.
func TestBroadcastAgentIdentitySatisfiesExactTargetValidation(t *testing.T) {
	server := testServer()
	agent := &coordinator.AgentState{
		PaneID: "pane-1", RawPaneID: "pane-1", TerminalID: "terminal-1",
		Agent: "omp", Status: "working",
		Session: "/home/user/.omp/agent/sessions/-work/2026-08-30T19-57-25-194Z_x.jsonl",
	}
	server.resolveAgentSessionName(agent)
	server.state.CommitInventory([]*coordinator.AgentState{agent}, server.state.RevisionCounter())

	data, err := json.Marshal(server.state.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var wire []*coordinator.AgentState
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	server.projectAgentResources(wire)
	advertised := wire[0]
	if advertised.AgentSessionID == "" {
		t.Fatal("agents broadcast lost the agent session identity")
	}
	echoed := protocol.TargetRef{
		ServerSessionID: advertised.ServerSessionID,
		PaneID:          advertised.RawPaneID,
		TerminalID:      advertised.TerminalID,
		Generation:      advertised.Generation,
		AgentSessionID:  advertised.AgentSessionID,
	}
	if err := validateExactPaneTarget(server.state, protocol.Inbound{PaneID: "pane-1", Target: &echoed}, true); err != nil {
		t.Fatalf("broadcast identity rejected by exact-target validation: %#v", err)
	}
}
