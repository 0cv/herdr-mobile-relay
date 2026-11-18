package coordinator

import (
	"reflect"
	"testing"
)

func TestBroadcastCommandResultMessage(t *testing.T) {
	for _, publicError := range []string{"", "request failed"} {
		for _, data := range []any{nil, map[string]any{"pane_id": "pane-2"}} {
			result := &CommandResult{
				RequestID: "req-1",
				Action:    "agent_start",
				OK:        publicError == "",
				Phase:     "completed",
				Error:     publicError,
				PaneID:    "pane-1",
				Data:      data,
			}
			want := map[string]any{
				"type":       "command_result",
				"request_id": "req-1",
				"action":     "agent_start",
				"ok":         publicError == "",
				"phase":      "completed",
				"error":      publicError,
				"pane_id":    "pane-1",
			}
			if data != nil {
				want["data"] = data
			}
			var got any
			d := &Dispatcher{broadcast: func(message any) { got = message }}
			d.broadcastResult(result)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("broadcast = %#v, want %#v", got, want)
			}
			if message := CommandResultMessage(result); !reflect.DeepEqual(message, want) {
				t.Fatalf("message = %#v, want %#v", message, want)
			}
		}
	}
}
