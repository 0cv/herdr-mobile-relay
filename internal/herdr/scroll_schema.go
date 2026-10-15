package herdr

import (
	"encoding/json"
	"slices"
	"strings"
)

type scrollWireSchema struct{ root map[string]any }

func (s scrollWireSchema) resolve(value any) (map[string]any, bool) {
	for depth := 0; depth < 32; depth++ {
		node, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		ref, exists := node["$ref"]
		if !exists {
			return node, true
		}
		if !schemaKeys(node, "$ref") {
			return nil, false
		}
		path, ok := ref.(string)
		if !ok || !strings.HasPrefix(path, "#/") {
			return nil, false
		}
		value = s.root
		for _, part := range strings.Split(path[2:], "/") {
			part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
			object, ok := value.(map[string]any)
			if !ok {
				return nil, false
			}
			value, ok = object[part]
			if !ok {
				return nil, false
			}
		}
	}
	return nil, false
}

func schemaKeys(node map[string]any, keys ...string) bool {
	for key := range node {
		if slices.Contains(keys, key) || slices.Contains([]string{"$schema", "$defs", "title", "description", "default", "$comment", "examples", "deprecated"}, key) {
			continue
		}
		return false
	}
	return true
}

func (s scrollWireSchema) object(value any, union bool) (map[string]any, []string, bool) {
	node, ok := s.resolve(value)
	if !ok || node["type"] != "object" {
		return nil, nil, false
	}
	keys := []string{"type", "properties", "required", "additionalProperties"}
	if union {
		keys = append(keys, "oneOf")
	}
	if !schemaKeys(node, keys...) {
		return nil, nil, false
	}
	if extra, exists := node["additionalProperties"]; exists {
		if _, ok := extra.(bool); !ok {
			return nil, nil, false
		}
	}
	props, ok := node["properties"].(map[string]any)
	if !ok {
		return nil, nil, false
	}
	var required []string
	if raw, exists := node["required"]; exists {
		names, ok := raw.([]any)
		if !ok {
			return nil, nil, false
		}
		for _, name := range names {
			text, ok := name.(string)
			if !ok {
				return nil, nil, false
			}
			required = append(required, text)
		}
	}
	return props, required, true
}

func (s scrollWireSchema) scalar(value any, kind string, nullable bool, fixed any) bool {
	node, ok := s.resolve(value)
	if !ok || !schemaKeys(node, "type", "enum", "const", "format", "minimum", "maximum") {
		return false
	}
	types := []string{}
	switch raw := node["type"].(type) {
	case string:
		types = append(types, raw)
	case []any:
		for _, t := range raw {
			name, ok := t.(string)
			if !ok {
				return false
			}
			types = append(types, name)
		}
	default:
		return false
	}
	if !slices.Contains(types, kind) {
		return false
	}
	for _, t := range types {
		if t != kind && (!nullable || t != "null") {
			return false
		}
	}
	if format, exists := node["format"]; exists {
		if kind != "integer" || format != "uint32" && format != "uint64" {
			return false
		}
	}
	for _, key := range []string{"minimum", "maximum"} {
		if bound, exists := node[key]; exists {
			n, ok := bound.(float64)
			if !ok || kind != "integer" {
				return false
			}
			if key == "minimum" && n > 0 || key == "maximum" && n < 65535 {
				return false
			}
		}
	}
	if constant, exists := node["const"]; exists {
		if fixed == nil || constant != fixed {
			return false
		}
	}
	if enumeration, exists := node["enum"]; exists {
		values, ok := enumeration.([]any)
		if !ok || fixed == nil {
			return false
		}
		found := false
		for _, v := range values {
			if v == fixed {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (s scrollWireSchema) variant(value any, discriminator, name string) (map[string]any, bool) {
	node, ok := s.resolve(value)
	if !ok || node["type"] == nil && !schemaKeys(node, "oneOf") {
		return nil, false
	}
	branches, ok := node["oneOf"].([]any)
	if !ok {
		return nil, false
	}
	var found map[string]any
	names := map[string]bool{}
	for _, branch := range branches {
		variant, ok := s.resolve(branch)
		if !ok {
			return nil, false
		}
		props, required, ok := s.object(variant, false)
		if !ok || !slices.Contains(required, discriminator) {
			return nil, false
		}
		tag, ok := s.resolve(props[discriminator])
		if !ok {
			return nil, false
		}
		text, ok := tag["const"].(string)
		if !ok || names[text] || !s.scalar(tag, "string", false, text) {
			return nil, false
		}
		names[text] = true
		if text == name {
			found = variant
		}
	}
	return found, found != nil
}

func (s scrollWireSchema) request(root any, method string, fields map[string]string, fixed map[string]any) bool {
	branch, ok := s.variant(root, "method", method)
	if !ok {
		return false
	}
	props, required, ok := s.object(branch, false)
	if !ok || len(props) != 2 || branch["additionalProperties"] == false || !slices.Contains(required, "method") || !slices.Contains(required, "params") || !s.scalar(props["method"], "string", false, method) {
		return false
	}
	for _, name := range required {
		if name != "method" && name != "params" {
			return false
		}
	}
	params, paramRequired, ok := s.object(props["params"], false)
	if !ok {
		return false
	}
	for _, name := range paramRequired {
		if _, ok := fields[name]; !ok {
			return false
		}
	}
	for name, kind := range fields {
		if !s.scalar(params[name], kind, method == "pane.process_info" || name == "lines", fixed[name]) {
			return false
		}
	}
	return true
}

func (s scrollWireSchema) result(root any, tag, field string) (map[string]any, bool) {
	branch, ok := s.variant(root, "type", tag)
	if !ok {
		return nil, false
	}
	props, required, ok := s.object(branch, false)
	if !ok || !slices.Contains(required, "type") || !s.scalar(props["type"], "string", false, tag) {
		return nil, false
	}
	if field == "" {
		return props, len(required) == 1 && len(props) == 1
	}
	if !slices.Contains(required, field) {
		return nil, false
	}
	payload, payloadRequired, ok := s.object(props[field], false)
	if field == "process_info" {
		ok = ok && slices.Contains(payloadRequired, "pane_id")
	}
	if field == "read" {
		ok = ok && slices.Contains(payloadRequired, "text") && slices.Contains(payloadRequired, "truncated")
	}
	return payload, ok
}

func qualifiesPiScrollWire(root map[string]any) bool {
	s := scrollWireSchema{root: root}
	schemas, ok := root["schemas"].(map[string]any)
	if !ok {
		return false
	}
	request := schemas["request"]
	requestProps, required, ok := s.object(request, true)
	requestNode, resolved := s.resolve(request)
	if !ok || !resolved || requestNode["additionalProperties"] == false || len(requestProps) != 1 || !slices.Contains(required, "id") || len(required) != 1 || !s.scalar(requestProps["id"], "string", false, nil) {
		return false
	}
	if !s.request(request, "pane.send_text", map[string]string{"pane_id": "string", "text": "string"}, nil) || !s.request(request, "pane.process_info", map[string]string{"pane_id": "string"}, nil) || !s.request(request, "pane.read", map[string]string{"pane_id": "string", "source": "string", "format": "string", "lines": "integer", "strip_ansi": "boolean"}, map[string]any{"source": "visible", "format": "ansi"}) {
		return false
	}
	responseProps, required, ok := s.object(schemas["success_response"], false)
	if !ok || !slices.Contains(required, "id") || !slices.Contains(required, "result") || !s.scalar(responseProps["id"], "string", false, nil) {
		return false
	}
	result := responseProps["result"]
	if _, ok := s.result(result, "ok", ""); !ok {
		return false
	}
	info, ok := s.result(result, "pane_process_info", "process_info")
	if !ok || !s.scalar(info["pane_id"], "string", false, nil) || !s.scalar(info["shell_pid"], "integer", true, nil) || !s.scalar(info["foreground_process_group_id"], "integer", true, nil) {
		return false
	}
	processes, ok := s.resolve(info["foreground_processes"])
	if !ok || processes["type"] != "array" || !schemaKeys(processes, "type", "items") {
		return false
	}
	process, processRequired, ok := s.object(processes["items"], false)
	if !ok || !slices.Contains(processRequired, "pid") || !slices.Contains(processRequired, "name") || !s.scalar(process["pid"], "integer", false, nil) || !s.scalar(process["name"], "string", false, nil) || !s.scalar(process["cwd"], "string", true, nil) || !s.scalar(process["cmdline"], "string", true, nil) {
		return false
	}
	argv, ok := s.resolve(process["argv"])
	if !ok || !schemaKeys(argv, "type", "items") {
		return false
	}
	types, ok := argv["type"].([]any)
	if !ok || len(types) != 2 || !slices.Contains(types, any("array")) || !slices.Contains(types, any("null")) || !s.scalar(argv["items"], "string", false, nil) {
		return false
	}
	read, ok := s.result(result, "pane_read", "read")
	return ok && s.scalar(read["text"], "string", false, nil) && s.scalar(read["truncated"], "boolean", false, nil)
}

func piScrollSchemaDocument(data []byte) (map[string]any, bool) {
	var root map[string]any
	if len(data) > 2*1024*1024 || json.Unmarshal(data, &root) != nil {
		return nil, false
	}
	return root, root != nil
}
