package conversation

import "testing"

func TestCoreAgentAliases(t *testing.T) {
	for _, name := range []string{"qodercli", "kimi-code", "grok-build"} {
		if !Supported(name) {
			t.Errorf("%s unsupported", name)
		}
	}
	if !IsKimi("kimi-code") {
		t.Fatal("core Kimi not recognized")
	}
	reader, home := testReader(t)
	normalGrokSession().write(t, grokUpdatesPath(home, grokCWD, testSessionID))
	entries, err := reader.ReadLatest("grok-build", ProjectContext{CWD: grokCWD}, testSessionID, 80)
	if err != nil || len(entries) != 3 {
		t.Fatalf("core Grok entries=%+v err=%v", entries, err)
	}
}
