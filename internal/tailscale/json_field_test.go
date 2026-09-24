package tailscale

import (
	"strings"
	"testing"
)

func TestExtractJSONFieldUsesExactTopLevelTypedValues(t *testing.T) {
	tests := []struct {
		name string
		data string
		key  string
		kind string
		want string
	}{
		{"true", `{"serve_inspected":true}`, "serve_inspected", "bool", "true"},
		{"false", `{"serve_inspected":false}`, "serve_inspected", "bool", "false"},
		{"decoded string", `{"origin":"https:\/\/relay.example.test"}`, "origin", "string", "https://relay.example.test"},
		{"integer", `{"serve_route_count":1}`, "serve_route_count", "number", "1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ExtractJSONField([]byte(test.data), test.key, test.kind)
			if err != nil || got != test.want {
				t.Fatalf("ExtractJSONField() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestExtractJSONFieldRejectsAmbiguousOrInvalidDocuments(t *testing.T) {
	tests := []struct {
		name string
		data string
		key  string
		kind string
	}{
		{"missing", `{"other":true}`, "ready", "bool"},
		{"wrong bool type", `{"ready":"true"}`, "ready", "bool"},
		{"null bool", `{"ready":null}`, "ready", "bool"},
		{"trailing value", `{"ready":true} {}`, "ready", "bool"},
		{"malformed", `{"ready":tru}`, "ready", "bool"},
		{"nested shadow", `{"message":{"ready":true}}`, "ready", "bool"},
		{"whitespace is key data", `{"re ady":true}`, "ready", "bool"},
		{"quoted lookalike", `{"message":"\"ready\":true"}`, "ready", "bool"},
		{"duplicate key", `{"ready":true,"ready":false}`, "ready", "bool"},
		{"escaped duplicate key", `{"ready":true,"\u0072eady":false}`, "ready", "bool"},
		{"case-conflicting duplicate", `{"ready":true,"READY":false}`, "ready", "bool"},
		{"invalid number form", `{"count":1e0}`, "count", "number"},
		{"negative number", `{"count":-1}`, "count", "number"},
		{"fractional number", `{"count":1.5}`, "count", "number"},
		{"string control character", `{"run_id":"line\nbreak"}`, "run_id", "string"},
		{"unsupported kind", `{"ready":true}`, "ready", "float"},
		{"empty key", `{"ready":true}`, "", "bool"},
		{"oversized key", `{"ready":true}`, strings.Repeat("k", 257), "bool"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got, err := ExtractJSONField([]byte(test.data), test.key, test.kind); err == nil {
				t.Fatalf("ExtractJSONField() = %q, nil; want refusal", got)
			}
		})
	}
}

func TestExtractJSONFieldEnforcesDocumentBounds(t *testing.T) {
	deep := `{"ready":true,"deep":` + strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34) + `}`
	if got, err := ExtractJSONField([]byte(deep), "ready", "bool"); err == nil {
		t.Fatalf("deep document returned %q", got)
	}

	large := `{"ready":true,"padding":"` + strings.Repeat("x", MaxOutputBytes) + `"}`
	if got, err := ExtractJSONField([]byte(large), "ready", "bool"); err == nil {
		t.Fatalf("oversized document returned %q", got)
	}
}
