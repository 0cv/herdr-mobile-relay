package hc

import "testing"

func TestSessionLabels(t *testing.T) {
	pid := 42
	for _, tt := range []struct {
		name, session, pid string
		foreground         *foreground
		want               string
	}{
		{"match", "abc", "42", &foreground{PID: 42}, "abc"},
		{"mismatch", "abc", "43", &foreground{PID: 42}, ""},
		{"missing pid", "abc", "", &foreground{PID: 42}, ""},
		{"missing session", "", "42", &foreground{PID: 42}, ""},
		{"no foreground", "abc", "42", nil, ""},
		{"zero", "abc", "0", &foreground{}, ""},
		{"invalid pid", "abc", "bad", &foreground{PID: 42}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := New("").pane(terminal{Address: "t1", PID: &pid, Foreground: tt.foreground, Labels: map[string]string{SessionLabel: tt.session, SessionPIDLabel: tt.pid}})
			if p.Session != tt.want || p.SessionRaw.Value != tt.want || p.SessionRaw.Kind != "" {
				t.Fatalf("%+v", p)
			}
		})
	}
}
