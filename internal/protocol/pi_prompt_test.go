package protocol

import "testing"

func TestPiPromptActionsAreSeparateAndFailClosed(t *testing.T) {
	read, ok := ClassifyAction("get_pi_prompt_capability")
	if !ok || read.Class != ActionReadOnly || read.Coordinated || read.Audited {
		t.Fatalf("bad capability metadata: %+v", read)
	}
	send, ok := ClassifyAction("send_pi_prompt_unconfirmed")
	if !ok || send.Class != ActionMutating || !send.Coordinated || !send.Audited || !send.RequiresProtocol {
		t.Fatalf("bad submit metadata: %+v", send)
	}
	request, err := DecodeMap(map[string]any{"type": "send_pi_prompt_unconfirmed", "protocol": Version, "text": "hello", "pi_prompt_lease": map[string]any{"token": "synthetic-token"}})
	if err != nil || !Compatible(request) || len(request.PiPromptLease) == 0 {
		t.Fatal("lease was not decoded")
	}
	request.Protocol = 0
	if Compatible(request) {
		t.Fatal("bridge mutation bypassed protocol admission")
	}
}
