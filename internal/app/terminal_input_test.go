package app

import (
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func TestManualTerminalTextUsesCentralMutationAdmission(t *testing.T) {
	action, known := protocol.ClassifyAction("send_terminal_text")
	if !known || action.Class != protocol.ActionMutating || !action.Coordinated || !action.Audited || !action.RequiresProtocol {
		t.Fatalf("unsafe action metadata: %+v", action)
	}
	for _, role := range []protocol.DeviceRole{protocol.RoleReader, protocol.RoleController} {
		err := authorizeAuthenticatedIdentity(transport.AuthenticatedIdentity{Role: string(role)}, true, action, "")
		if (err != nil) != (role == protocol.RoleReader) {
			t.Fatalf("role %s admission = %#v", role, err)
		}
	}
}
