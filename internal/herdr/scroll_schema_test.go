package herdr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func scrollSchemaFixture(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("../../tests/fixtures/herdr-pi-scroll-schema.json")
	if err != nil {
		t.Fatal(err)
	}
	root, ok := piScrollSchemaDocument(data)
	if !ok {
		t.Fatal("invalid schema fixture")
	}
	return root
}

func schemaFixtureNode(root map[string]any, path ...string) map[string]any {
	var value any = root
	for _, name := range path {
		value = value.(map[string]any)[name]
	}
	return value.(map[string]any)
}

func TestPiScrollSchemaRefusesIncompatibleWire(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"method-names-only", func(r map[string]any) {
			req := schemaFixtureNode(r, "schemas", "request")
			for _, b := range req["oneOf"].([]any) {
				delete(b.(map[string]any)["properties"].(map[string]any), "params")
			}
		}},
		{"missing-params-definition", func(r map[string]any) {
			delete(schemaFixtureNode(r, "schemas", "request", "$defs"), "PaneSendTextParams")
		}},
		{"wrong-pane-type", func(r map[string]any) {
			schemaFixtureNode(r, "schemas", "request", "$defs", "PaneSendTextParams", "properties", "pane_id")["type"] = "integer"
		}},
		{"wrong-text-type", func(r map[string]any) {
			schemaFixtureNode(r, "schemas", "request", "$defs", "PaneSendTextParams", "properties", "text")["type"] = "boolean"
		}},
		{"restricted-text", func(r map[string]any) {
			schemaFixtureNode(r, "schemas", "request", "$defs", "PaneSendTextParams", "properties", "text")["enum"] = []any{"ordinary text"}
		}},
		{"extra-required-param", func(r map[string]any) {
			schemaFixtureNode(r, "schemas", "request", "$defs", "PaneSendTextParams")["required"] = []any{"pane_id", "text", "mode"}
		}},
		{"cyclic-reference", func(r map[string]any) {
			schemaFixtureNode(r, "schemas", "request", "$defs")["PaneSendTextParams"] = map[string]any{"$ref": "#/schemas/request/$defs/PaneSendTextParams"}
		}},
		{"remote-reference", func(r map[string]any) {
			schemaFixtureNode(r, "schemas", "request", "$defs")["PaneSendTextParams"] = map[string]any{"$ref": "https://example.invalid/params"}
		}},
		{"reference-conflicting-sibling", func(r map[string]any) {
			schemaFixtureNode(r, "schemas", "request", "$defs")["PaneSendTextParams"] = map[string]any{"$ref": "#/schemas/request/$defs/PaneProcessInfoParams", "type": "integer"}
		}},
		{"request-id-type", func(r map[string]any) {
			schemaFixtureNode(r, "schemas", "request", "properties", "id")["type"] = "integer"
		}},
		{"envelope-denies-method", func(r map[string]any) { schemaFixtureNode(r, "schemas", "request")["additionalProperties"] = false }},
		{"ambiguous-request-union", func(r map[string]any) {
			req := schemaFixtureNode(r, "schemas", "request")
			req["oneOf"] = append(req["oneOf"].([]any), map[string]any{"type": "object", "properties": map[string]any{}})
		}},
		{"wrong-visible-source", func(r map[string]any) {
			schemaFixtureNode(r, "schemas", "request", "$defs", "ReadSource")["enum"] = []any{"recent"}
		}},
		{"wrong-ansi-format", func(r map[string]any) {
			schemaFixtureNode(r, "schemas", "request", "$defs", "ReadFormat")["enum"] = []any{"text"}
		}},
		{"wrong-lines-type", func(r map[string]any) {
			schemaFixtureNode(r, "schemas", "request", "$defs", "PaneReadParams", "properties", "lines")["type"] = "string"
		}},
		{"wrong-strip-ansi-type", func(r map[string]any) {
			schemaFixtureNode(r, "schemas", "request", "$defs", "PaneReadParams", "properties", "strip_ansi")["type"] = "integer"
		}},
		{"missing-response", func(r map[string]any) { delete(schemaFixtureNode(r, "schemas"), "success_response") }},
		{"response-id-type", func(r map[string]any) {
			schemaFixtureNode(r, "schemas", "success_response", "properties", "id")["type"] = "integer"
		}},
		{"wrong-ok-tag", func(r map[string]any) {
			s := scrollWireSchema{r}
			branch, _ := s.variant(schemaFixtureNode(r, "schemas", "success_response", "$defs", "ResponseResult"), "type", "ok")
			branch["properties"].(map[string]any)["type"].(map[string]any)["const"] = "accepted"
		}},
		{"ok-not-object", func(r map[string]any) {
			s := scrollWireSchema{r}
			branch, _ := s.variant(schemaFixtureNode(r, "schemas", "success_response", "$defs", "ResponseResult"), "type", "ok")
			branch["type"] = "string"
		}},
		{"ok-extra-required-field", func(r map[string]any) {
			s := scrollWireSchema{r}
			branch, _ := s.variant(schemaFixtureNode(r, "schemas", "success_response", "$defs", "ResponseResult"), "type", "ok")
			branch["required"] = []any{"type", "accepted"}
		}},
		{"result-conflicting-union", func(r map[string]any) {
			schemaFixtureNode(r, "schemas", "success_response", "$defs", "ResponseResult")["allOf"] = []any{false}
		}},
		{"process-shell-type", func(r map[string]any) {
			schemaFixtureNode(r, "schemas", "success_response", "$defs", "PaneProcessInfo", "properties", "shell_pid")["type"] = "string"
		}},
		{"process-group-type", func(r map[string]any) {
			schemaFixtureNode(r, "schemas", "success_response", "$defs", "PaneProcessInfo", "properties", "foreground_process_group_id")["type"] = "string"
		}},
		{"process-list-type", func(r map[string]any) {
			schemaFixtureNode(r, "schemas", "success_response", "$defs", "PaneProcessInfo", "properties", "foreground_processes")["type"] = "object"
		}},
		{"process-pid-type", func(r map[string]any) {
			schemaFixtureNode(r, "schemas", "success_response", "$defs", "PaneProcessInfoProcess", "properties", "pid")["type"] = "string"
		}},
		{"process-argv-type", func(r map[string]any) {
			schemaFixtureNode(r, "schemas", "success_response", "$defs", "PaneProcessInfoProcess", "properties", "argv")["items"] = map[string]any{"type": "integer"}
		}},
		{"read-text-type", func(r map[string]any) {
			schemaFixtureNode(r, "schemas", "success_response", "$defs", "PaneReadResult", "properties", "text")["type"] = "array"
		}},
		{"read-truncated-type", func(r map[string]any) {
			schemaFixtureNode(r, "schemas", "success_response", "$defs", "PaneReadResult", "properties", "truncated")["type"] = "integer"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := scrollSchemaFixture(t)
			tc.change(root)
			dir := t.TempDir()
			bin := filepath.Join(dir, "herdr")
			data, err := json.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s' '"+string(data)+"'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			client := NewClient(bin, filepath.Join(dir, "selected.sock"))
			defer client.Close()
			got := client.piScrollSchemaEvidence(context.Background(), ServerStatus{InstalledClientVersion: "herdr 0.9.3", ServerVersion: "0.9.3", ServerProtocol: 22, ServerProtocolKnown: true})
			if got.State != FeatureUnsupported || got.Reason != "schema_incompatible_wire" || client.SupportsPiScrollSocket() {
				t.Fatalf("incompatible wire qualified: %+v", got)
			}
		})
	}
}

func TestPiScrollSchemaResolvesChainedReferences(t *testing.T) {
	root := scrollSchemaFixture(t)
	defs := schemaFixtureNode(root, "schemas", "request", "$defs")
	defs["RawParams"] = defs["PaneSendTextParams"]
	defs["PaneSendTextParams"] = map[string]any{"$ref": "#/schemas/request/$defs/RawParams"}
	if !qualifiesPiScrollWire(root) {
		t.Fatal("qualified chained definitions refused")
	}
}
