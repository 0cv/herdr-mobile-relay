package coordinator

import (
	"os"
	"strings"
	"testing"
)

func TestAgentStartWithoutWiringDoesNotMutate(t *testing.T) {
	for _, missing := range []string{"lifecycle", "profiles", "lifecycle profiles"} {
		t.Run(missing, func(t *testing.T) {
			f := newLaunchFixture(t)
			d := f.dispatcher
			switch missing {
			case "lifecycle":
				d.lifecycle = nil
			case "profiles":
				d.profiles = nil
			case "lifecycle profiles":
				d.lifecycle.profiles = nil
			}
			before := d.state.TopologyGeneration()
			for _, input := range []struct{ name, prompt, want string }{
				{"", "", "required"},
				{"proj", strings.Repeat("x", promptMaxChars+1), "longer"},
				{"proj", "hello", "not configured"},
			} {
				result := d.Handle(f.ctx, map[string]any{"action": "agent_start", "request_id": "unwired", "profile_id": "claude", "name": input.name, "cwd": f.cwd, "prompt": input.prompt})
				if result.OK || !strings.Contains(result.Error, input.want) || result.PaneID != "" {
					t.Fatalf("result = %+v, want %q", result, input.want)
				}
			}
			for _, path := range []string{f.calls, f.ownership} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("unexpected mutation at %s: %v", path, err)
				}
			}
			if d.state.TopologyGeneration() != before {
				t.Fatal("unwired launch published topology")
			}
		})
	}
}
