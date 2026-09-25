package tailscale

import (
	"encoding/json"
	"testing"
)

const configRouteEntry = `{"TCP":{"443":{"HTTPS":true}},"Web":{"relay.tailnet.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8375"}}}}}`

func TestSessionConfigPreservesFullPinnedServeSchemaWhenRemovingOneSession(t *testing.T) {
	input := `{"TCP":{"443":{"HTTPS":true}},"Web":{"relay.tailnet.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8375"}}}},"Services":{"svc:foreign":{"TCP":{"80":{"TCPForward":"127.0.0.1:8000","ProxyProtocol":1}},"Tun":true}},"AllowFunnel":{"foreign.tailnet.ts.net:443":false},"Foreground":{"ours":` + configRouteEntry + `,"foreign":{"TCP":{"443":{"HTTP":true}},"Web":{"foreign.tailnet.ts.net:443":{"Handlers":{"/":{"Text":"kept"}}}}}}}`
	if _, _, err := parseFullServeConfig([]byte(input)); err != nil {
		t.Fatalf("full recognized schema rejected: %v", err)
	}
	updated, changed, err := removeForegroundSession([]byte(input), "ours")
	if err != nil || !changed {
		t.Fatalf("removeForegroundSession changed=%t err=%v", changed, err)
	}
	if _, _, err := parseFullServeConfig(updated); err != nil {
		t.Fatalf("updated full schema rejected: %v", err)
	}
	var before, after map[string]json.RawMessage
	if json.Unmarshal([]byte(input), &before) != nil || json.Unmarshal(updated, &after) != nil {
		t.Fatal("test config did not decode")
	}
	for _, key := range []string{"TCP", "Web", "Services", "AllowFunnel"} {
		if !sameJSON(before[key], after[key]) {
			t.Errorf("foreign top-level %s changed", key)
		}
	}
	var beforeForeground, afterForeground map[string]json.RawMessage
	_ = json.Unmarshal(before["Foreground"], &beforeForeground)
	_ = json.Unmarshal(after["Foreground"], &afterForeground)
	if _, ok := afterForeground["ours"]; ok {
		t.Fatal("owned foreground key remains")
	}
	if !sameJSON(beforeForeground["foreign"], afterForeground["foreign"]) {
		t.Fatal("independent foreground session changed")
	}
}

func TestSessionConfigRejectsUnknownDuplicateAliasAndNullSchemaValues(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "unknown top-level", body: `{"Future":{}}`},
		{name: "unknown nested service", body: `{"Services":{"svc:x":{"Future":true}}}`},
		{name: "case alias", body: `{"TCP":{"443":{"HTTPS":true,"https":false}}}`},
		{name: "duplicate", body: `{"TCP":{},"TCP":{}}`},
		{name: "null TCP map", body: `{"TCP":null}`},
		{name: "null handler", body: `{"TCP":{"443":null}}`},
		{name: "wrong scalar", body: `{"Services":{"svc:x":{"Tun":"false"}}}`},
		{name: "wrong array", body: `{"Web":{"relay.tailnet.ts.net:443":{"Handlers":{"/":{"AcceptAppCaps":"x"}}}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := parseFullServeConfig([]byte(test.body)); err == nil {
				t.Fatalf("invalid config accepted: %s", test.body)
			}
		})
	}
	deep := `{}`
	for i := 0; i < 40; i++ {
		deep = `{"Foreground":{"nested":` + deep + `}}`
	}
	if _, _, err := parseFullServeConfig([]byte(deep)); err == nil {
		t.Fatal("over-depth full Serve schema accepted")
	}
}

func TestSessionConfigTreatsOnlySourceNullOrEmptyObjectAsInitialAbsence(t *testing.T) {
	for _, body := range []string{"null", "{}", " { } \n"} {
		if !isEmptyServeConfig([]byte(body)) {
			t.Errorf("empty source config refused: %q", body)
		}
	}
	for _, body := range []string{`{"TCP":{}}`, `{"Foreground":{}}`, `[]`, `false`} {
		if isEmptyServeConfig([]byte(body)) {
			t.Errorf("non-canonical config accepted as initial absence: %q", body)
		}
	}
}
