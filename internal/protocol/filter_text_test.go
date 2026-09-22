package protocol

import "testing"

func TestFilterTextActionMetadata(t *testing.T) {
	action, ok := ClassifyAction("send_filter_text")
	if !ok || action.Class != ActionMutating || !action.RequiresProtocol || !action.Coordinated || !action.Audited {
		t.Fatalf("filter action metadata: %+v, known=%v", action, ok)
	}
}
