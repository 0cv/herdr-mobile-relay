package conversation

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const maxBrowseCursorBytes = 2048

var errBrowseCursorExpired = errors.New("conversation cursor expired")
var errBrowseCursorInvalid = errors.New("conversation cursor is invalid")

type browseCursor struct {
	Version      int    `json:"v"`
	Mode         string `json:"mode"`
	Scope        string `json:"scope"`
	Revision     string `json:"revision"`
	SnapshotID   string `json:"snapshot_id,omitempty"`
	JobID        string `json:"job_id,omitempty"`
	ChainID      string `json:"chain_id,omitempty"`
	Segment      *int   `json:"segment,omitempty"`
	ChainOffset  bool   `json:"chain_offset,omitempty"`
	Boundary     string `json:"boundary,omitempty"`
	RangeStart   string `json:"range_start,omitempty"`
	RangeEnd     string `json:"range_end,omitempty"`
	RangeDigest  string `json:"range_digest,omitempty"`
	NativeBefore string `json:"native_before,omitempty"`
	ExpiresAt    int64  `json:"expires_at"`
}

func newBrowseSigningKey() ([32]byte, error) {
	var key [32]byte
	_, err := rand.Read(key[:])
	return key, err
}

func browseScopeID(scope BrowseScope) string {
	normalized := BrowseScope{
		Provider:        normalizedAgent(scope.Provider),
		CWD:             strings.TrimSpace(scope.CWD),
		SessionID:       strings.TrimSpace(scope.SessionID),
		PaneID:          strings.TrimSpace(scope.PaneID),
		ServerSessionID: strings.TrimSpace(scope.ServerSessionID),
		TerminalID:      strings.TrimSpace(scope.TerminalID),
		Generation:      scope.Generation,
	}
	data, _ := json.Marshal(normalized)
	digest := sha256.Sum256(data)
	return hexBrowse(digest[:])
}

func hexBrowse(value []byte) string {
	const alphabet = "0123456789abcdef"
	result := make([]byte, len(value)*2)
	for index, item := range value {
		result[index*2] = alphabet[item>>4]
		result[index*2+1] = alphabet[item&0x0f]
	}
	return string(result)
}

func encodeBrowseCursor(key [32]byte, cursor browseCursor) (string, error) {
	cursor.Version = 1
	data, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(data)
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write(data)
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	token := "hb1." + payload + "." + signature
	if len(token) > maxBrowseCursorBytes {
		return "", errors.New("conversation cursor is too large")
	}
	return token, nil
}

func decodeBrowseCursor(key [32]byte, token string, scope BrowseScope) (browseCursor, error) {
	if len(token) == 0 || len(token) > maxBrowseCursorBytes {
		return browseCursor{}, errBrowseCursorInvalid
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "hb1" || parts[1] == "" || parts[2] == "" {
		return browseCursor{}, errBrowseCursorInvalid
	}
	data, err := decodeBrowseBase64(parts[1])
	if err != nil || len(data) == 0 {
		return browseCursor{}, errBrowseCursorInvalid
	}
	supplied, err := decodeBrowseBase64(parts[2])
	if err != nil {
		return browseCursor{}, errBrowseCursorInvalid
	}
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write(data)
	if !hmac.Equal(supplied, mac.Sum(nil)) {
		return browseCursor{}, errBrowseCursorInvalid
	}
	var cursor browseCursor
	if json.Unmarshal(data, &cursor) != nil || cursor.Version != 1 || cursor.Mode == "" || cursor.Scope == "" || cursor.ExpiresAt <= 0 {
		return browseCursor{}, errBrowseCursorInvalid
	}
	if cursor.Scope != browseScopeID(scope) {
		return browseCursor{}, errBrowseCursorInvalid
	}
	if !time.Now().Before(time.Unix(cursor.ExpiresAt, 0)) {
		return browseCursor{}, errBrowseCursorExpired
	}
	if !validBrowseCursorText(cursor.Mode, 32) || !validBrowseCursorText(cursor.Revision, 128) ||
		!validBrowseCursorText(cursor.SnapshotID, 128) || !validBrowseCursorText(cursor.JobID, 128) ||
		!validBrowseCursorText(cursor.ChainID, 128) || !validBrowseCursorText(cursor.RangeDigest, 128) ||
		!validBrowseCursorText(cursor.NativeBefore, 256) {
		return browseCursor{}, errBrowseCursorInvalid
	}
	if (cursor.ChainID == "") != (cursor.Segment == nil) || cursor.Segment != nil && (*cursor.Segment < 0 || *cursor.Segment >= claudeContinuationMaxSegments) || cursor.ChainOffset && cursor.ChainID == "" {
		return browseCursor{}, errBrowseCursorInvalid
	}
	if cursor.Mode == "chain" {
		if cursor.ChainID == "" || cursor.Segment == nil || cursor.SnapshotID == "" || cursor.JobID != "" {
			return browseCursor{}, errBrowseCursorInvalid
		}
	} else if cursor.ChainID != "" || cursor.Segment != nil || cursor.ChainOffset {
		return browseCursor{}, errBrowseCursorInvalid
	}
	for _, value := range []string{cursor.Boundary, cursor.RangeStart, cursor.RangeEnd} {
		if value != "" {
			if _, err := parseBrowseOffset(value); err != nil {
				return browseCursor{}, errBrowseCursorInvalid
			}
		}
	}
	return cursor, nil
}

func decodeBrowseBase64(value string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return nil, errBrowseCursorInvalid
	}
	return decoded, nil
}

func validBrowseCursorText(value string, max int) bool {
	return len(value) <= max && !strings.ContainsAny(value, "\x00\r\n")
}

func browseOffset(value int64) string {
	return strconv.FormatInt(value, 10)
}

func parseBrowseOffset(value string) (int64, error) {
	if value == "" || strings.HasPrefix(value, "-") || len(value) > 20 {
		return 0, errBrowseCursorInvalid
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, errBrowseCursorInvalid
	}
	return parsed, nil
}

func browseCursorError(err error) (string, string) {
	switch {
	case errors.Is(err, errBrowseCursorExpired):
		return "cursor_expired", "This history cursor has expired. Start browsing again from the latest messages."
	case errors.Is(err, errBrowseCursorInvalid):
		return "invalid_cursor", "This history cursor is invalid for the requested conversation."
	default:
		return "invalid_cursor", fmt.Sprintf("This history cursor is invalid: %v", err)
	}
}
