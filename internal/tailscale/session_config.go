package tailscale

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"

	"tailscale.com/ipn"
)

var pinnedServeConfigType = reflect.TypeOf(ipn.ServeConfig{})

// parseFullServeConfig validates the complete pinned upstream schema without
// projecting it through the deliberately narrow observational ParseServe.
// Raw members are retained by callers when selectively editing a key.
func parseFullServeConfig(data []byte) (map[string]json.RawMessage, bool, error) {
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		if err := strictJSON(data); err != nil {
			return nil, false, err
		}
		return map[string]json.RawMessage{}, true, nil
	}
	root, err := object(data)
	if err != nil {
		return nil, false, err
	}
	if err := validatePinnedJSONValue(data, pinnedServeConfigType, 0); err != nil {
		return nil, false, err
	}
	var decoded ipn.ServeConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, false, ErrNotJSON
	}
	return root, false, nil
}

func validatePinnedJSONValue(data []byte, typ reflect.Type, depth int) error {
	if depth > 32 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return ErrNotJSON
	}
	switch typ.Kind() {
	case reflect.Pointer:
		return validatePinnedJSONValue(data, typ.Elem(), depth+1)
	case reflect.Struct:
		fields, err := object(data)
		if err != nil {
			return err
		}
		allowed := make(map[string]reflect.Type, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if field.PkgPath != "" {
				continue
			}
			tag := field.Tag.Get("json")
			name := strings.Split(tag, ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			allowed[name] = field.Type
		}
		for name, raw := range fields {
			fieldType, ok := allowed[name]
			if !ok {
				return fmt.Errorf("%w: unsupported Serve field", ErrNotJSON)
			}
			if err := validatePinnedJSONValue(raw, fieldType, depth+1); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		members, err := object(data)
		if err != nil {
			return err
		}
		for key, raw := range members {
			if err := validatePinnedMapKey(key, typ.Key()); err != nil {
				return err
			}
			if err := validatePinnedJSONValue(raw, typ.Elem(), depth+1); err != nil {
				return err
			}
		}
		return nil
	case reflect.Slice, reflect.Array:
		trimmed := bytes.TrimSpace(data)
		if len(trimmed) == 0 || trimmed[0] != '[' {
			return ErrNotJSON
		}
		var values []json.RawMessage
		if json.Unmarshal(data, &values) != nil {
			return ErrNotJSON
		}
		for _, raw := range values {
			if err := validatePinnedJSONValue(raw, typ.Elem(), depth+1); err != nil {
				return err
			}
		}
		return nil
	case reflect.String:
		var value string
		if json.Unmarshal(data, &value) != nil {
			return ErrNotJSON
		}
		return nil
	case reflect.Bool:
		var value bool
		if json.Unmarshal(data, &value) != nil {
			return ErrNotJSON
		}
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value := reflect.New(typ)
		if json.Unmarshal(data, value.Interface()) != nil {
			return ErrNotJSON
		}
		return nil
	default:
		return fmt.Errorf("%w: unsupported pinned Serve schema type", ErrNotJSON)
	}
}

func validatePinnedMapKey(value string, typ reflect.Type) error {
	switch typ.Kind() {
	case reflect.String:
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		parsed, err := strconv.ParseUint(value, 10, typ.Bits())
		if err != nil || strconv.FormatUint(parsed, 10) != value {
			return ErrNotJSON
		}
		return nil
	default:
		return ErrNotJSON
	}
}

func serveRouteConfig(route Route) (json.RawMessage, error) {
	if route.Listener != "HTTPS" || route.Handler != "Proxy" || route.Path != "/" || route.Session != "" || route.Port < 1 || route.Port > 65535 {
		return nil, ErrNotJSON
	}
	host, port, err := canonicalHostPort(net.JoinHostPort(route.Host, strconv.Itoa(route.Port)))
	if err != nil || host != route.Host || port != route.Port || !validLoopbackBackend(route.Backend) {
		return nil, ErrNotJSON
	}
	config := ipn.ServeConfig{
		TCP: map[uint16]*ipn.TCPPortHandler{uint16(route.Port): {HTTPS: true}},
		Web: map[ipn.HostPort]*ipn.WebServerConfig{
			ipn.HostPort(net.JoinHostPort(route.Host, strconv.Itoa(route.Port))): {
				Handlers: map[string]*ipn.HTTPHandler{"/": {Proxy: route.Backend}},
			},
		},
	}
	data, err := json.Marshal(config)
	if err != nil {
		return nil, ErrNotJSON
	}
	return data, nil
}

func validLoopbackBackend(backend string) bool {
	if !strings.HasPrefix(backend, "http://127.0.0.1:") || strings.ContainsAny(strings.TrimPrefix(backend, "http://127.0.0.1:"), "/?#@\\ \t\r\n") {
		return false
	}
	port := strings.TrimPrefix(backend, "http://127.0.0.1:")
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535 && strconv.Itoa(n) == port
}

func canonicalJSON(data []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, ErrNotJSON
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, ErrNotJSON
	}
	return encoded, nil
}

func sameJSON(a, b []byte) bool {
	ca, errA := canonicalJSON(a)
	cb, errB := canonicalJSON(b)
	return errA == nil && errB == nil && bytes.Equal(ca, cb)
}

func removeForegroundSession(data []byte, session string) ([]byte, bool, error) {
	root, _, err := parseFullServeConfig(data)
	if err != nil {
		return nil, false, err
	}
	foregroundRaw, exists := root["Foreground"]
	if !exists {
		return nil, false, nil
	}
	var sessions map[string]json.RawMessage
	if json.Unmarshal(foregroundRaw, &sessions) != nil || sessions == nil {
		return nil, false, ErrNotJSON
	}
	if _, exists := sessions[session]; !exists {
		return nil, false, nil
	}
	delete(sessions, session)
	updatedForeground, err := json.Marshal(sessions)
	if err != nil {
		return nil, false, ErrNotJSON
	}
	root["Foreground"] = updatedForeground
	updated, err := json.Marshal(root)
	if err != nil {
		return nil, false, ErrNotJSON
	}
	return updated, true, nil
}

func foregroundEntry(data []byte, session string) (json.RawMessage, bool, error) {
	root, _, err := parseFullServeConfig(data)
	if err != nil {
		return nil, false, err
	}
	foregroundRaw, exists := root["Foreground"]
	if !exists {
		return nil, false, nil
	}
	var sessions map[string]json.RawMessage
	if json.Unmarshal(foregroundRaw, &sessions) != nil || sessions == nil {
		return nil, false, ErrNotJSON
	}
	value, exists := sessions[session]
	return value, exists, nil
}

func isEmptyServeConfig(data []byte) bool {
	root, absent, err := parseFullServeConfig(data)
	return err == nil && (absent || len(root) == 0)
}
