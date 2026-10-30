package conversation

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestUnavailablePageWireFixture(t *testing.T) {
	data, err := os.ReadFile("../../contracts/fixtures/conversation_unavailable.json")
	if err != nil {
		t.Fatal(err)
	}
	page := BrowsePage{ReasonCode: "source_missing", Reason: "Conversation source is missing.", Entries: []Entry{}, State: "ready", Mode: "recent"}
	serialized, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(serialized, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Go wire differs from shared fixture\ngot: %s\nwant: %s", serialized, data)
	}
	page.ReasonCode = ""
	serialized, err = json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	var absent map[string]any
	if err := json.Unmarshal(serialized, &absent); err != nil {
		t.Fatal(err)
	}
	if _, ok := absent["reason_code"]; ok {
		t.Fatal("empty reason_code must be omitted")
	}
}
