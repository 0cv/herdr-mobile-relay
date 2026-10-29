package sqliteexec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

type nativeExecutor struct{}

func newNative() *nativeExecutor {
	return &nativeExecutor{}
}

// marshalJSONObject encodes one result object the way sqlite3 -json does:
// without HTML escaping, so large HTML tool payloads stay under the same
// byte budget as the CLI backend.
func marshalJSONObject(object map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(object); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func readOnlyDSN(database string) (string, error) {
	path := filepath.Clean(strings.TrimSpace(database))
	if path == "" || path == "." {
		return "", fmt.Errorf("database path required")
	}
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		path = abs
	}
	// url.URL encodes spaces and reserved characters; scheme file + absolute
	// Path yields file:///... which modernc accepts for read-only opens.
	u := url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(path),
		RawQuery: "mode=ro&_pragma=query_only(1)",
	}
	return u.String(), nil
}

func normalizeJSONValue(value any) any {
	switch v := value.(type) {
	case nil:
		return nil
	case []byte:
		return string(v)
	default:
		return v
	}
}

// NormalizeJSONArray turns empty CLI stdout into a JSON array so callers can
// always json.Unmarshal into a slice.
func NormalizeJSONArray(raw []byte) []byte {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("[]")
	}
	return raw
}
