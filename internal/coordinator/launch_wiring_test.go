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
				result := d.Handle(f.ctx, map[string]any{"type": "agent_start", "request_id": "unwired", "profile_id": "claude", "name": input.name, "cwd": f.cwd, "prompt": input.prompt})
				if result.OK || !strings.Contains(result.Error, input.want) || result.PaneID != "" {
					t.Fatalf("result = %+v, want %q", result, input.want)
				}
			}
			if calls := f.client.recorded(); len(calls) != 0 {
				t.Fatalf("unexpected Herdr calls: %+v", calls)
			}
			if _, err := os.Stat(f.ownership); !os.IsNotExist(err) {
				t.Fatalf("unexpected mutation at %s: %v", f.ownership, err)
			}
			if d.state.TopologyGeneration() != before {
				t.Fatal("unwired launch published topology")
			}
		})
	}
}
