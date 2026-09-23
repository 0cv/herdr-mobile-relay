package readiness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
)

type Expected struct{ Instance, Version, Revision, WebHash string }

func Verify(reader io.Reader, expected Expected) error {
	if expected.Instance == "" {
		return errors.New("expected relay instance is required")
	}
	decoder := json.NewDecoder(io.LimitReader(reader, 64*1024+1))
	value, err := uniqueValue(decoder, 0)
	if err != nil {
		return errors.New("invalid readiness response")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("invalid readiness response suffix")
	}
	response, ok := value.(map[string]any)
	if !ok {
		return errors.New("readiness response is not an object")
	}
	inventory, _ := response["inventory"].(map[string]any)
	if response["status"] != "ready" || inventory["state"] != "ready" || response["protocol"] != float64(protocol.Version) {
		return errors.New("relay inventory is not ready")
	}
	for key, want := range map[string]string{"instance": expected.Instance, "release_version": expected.Version, "revision": expected.Revision, "bundle_hash": expected.WebHash} {
		actual, ok := response[key].(string)
		if !ok || actual == "" || (want != "" && actual != want) {
			return fmt.Errorf("readiness %s does not match", key)
		}
	}
	return nil
}

func uniqueValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 16 {
		return nil, errors.New("excessive nesting")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, compound := token.(json.Delim)
	if !compound {
		return token, nil
	}
	switch delim {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, errors.New("invalid object key")
			}
			if _, exists := object[name]; exists {
				return nil, errors.New("duplicate object key")
			}
			value, err := uniqueValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[name] = value
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return nil, errors.New("invalid object ending")
		}
		return object, nil
	case '[':
		for decoder.More() {
			if _, err := uniqueValue(decoder, depth+1); err != nil {
				return nil, err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return nil, errors.New("invalid array ending")
		}
		return []any{}, nil
	default:
		return nil, errors.New("unexpected delimiter")
	}
}
