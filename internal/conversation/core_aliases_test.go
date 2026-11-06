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
	if normalizedAgent("grok-build") != normalizedAgent("grok") {
		t.Fatal("Grok reader alias missing")
	}
	reader, home := testReader(t)
	normalGrokSession().write(t, grokUpdatesPath(home, grokCWD, testSessionID))
	page, err := reader.ReadFor("grok-build", grokCWD, testSessionID, "", 80)
	if err != nil || !page.Available || page.Total != 3 {
		t.Fatalf("core Grok page=%+v err=%v", page, err)
	}
}
