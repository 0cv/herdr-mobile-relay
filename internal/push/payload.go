package push

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/localize"
)

type Platform string

const (
	PlatformAndroidChromium Platform = "android_chromium"
	PlatformIOS             Platform = "ios"
	PlatformOther           Platform = "other"
)

var (
	ErrInvalidReference = errors.New("push_invalid_reference")
	ErrExpiredReference = errors.New("push_expired_reference")
	ErrStaleReference   = errors.New("push_stale_reference")
)

type ReferenceClaims struct {
	Key       PushEventKey `json:"key"`
	ExpiresAt time.Time    `json:"expires_at"`
}

type ReferenceSigner struct {
	key []byte
}

func loadOrCreateReferenceSigner(path string) (*ReferenceSigner, error) {
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != 32 {
			return nil, errors.New("push_invalid_reference_key")
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("protect push reference key: %w", err)
		}
		return &ReferenceSigner{key: key}, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read push reference key: %w", err)
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate push reference key: %w", err)
	}
	if err := atomicWrite(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("write push reference key: %w", err)
	}
	return &ReferenceSigner{key: key}, nil
}

func NewReferenceSigner(key []byte) (*ReferenceSigner, error) {
	if len(key) < 32 {
		return nil, errors.New("push_reference_key_too_short")
	}
	copyKey := make([]byte, len(key))
	copy(copyKey, key)
	return &ReferenceSigner{key: copyKey}, nil
}

func (s *ReferenceSigner) Sign(claims ReferenceClaims) (string, error) {
	if s == nil || len(s.key) == 0 {
		return "", ErrInvalidReference
	}
	if err := claims.Key.Validate(); err != nil || claims.ExpiresAt.IsZero() {
		return "", ErrInvalidReference
	}
	data, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("encode push reference: %w", err)
	}
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(data) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *ReferenceSigner) Verify(token string, now time.Time) (ReferenceClaims, error) {
	var claims ReferenceClaims
	parts := strings.Split(token, ".")
	if len(parts) != 2 || s == nil || len(s.key) == 0 {
		return claims, ErrInvalidReference
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return claims, ErrInvalidReference
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims, ErrInvalidReference
	}
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write(data)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return claims, ErrInvalidReference
	}
	if err := json.Unmarshal(data, &claims); err != nil || claims.Key.Validate() != nil {
		return ReferenceClaims{}, ErrInvalidReference
	}
	if !now.Before(claims.ExpiresAt) {
		return ReferenceClaims{}, ErrExpiredReference
	}
	return claims, nil
}

type PayloadRequest struct {
	Key       PushEventKey
	Locale    string
	Preview   PreviewMode
	ExpiresAt time.Time
	Retract   bool
}

type notificationAction struct {
	Action string `json:"action"`
	Title  string `json:"title"`
}

type Payload struct {
	Version    int                  `json:"v"`
	Category   Category             `json:"category"`
	Key        PushEventKey         `json:"key"`
	Title      string               `json:"title"`
	Body       string               `json:"body"`
	Tag        string               `json:"tag"`
	URL        string               `json:"url"`
	Actions    []notificationAction `json:"actions"`
	ActionRefs map[string]string    `json:"action_refs"`
	EventRef   string               `json:"event_ref"`
	Retract    bool                 `json:"retract,omitempty"`
}

func BuildPayload(request PayloadRequest, signer *ReferenceSigner) ([]byte, error) {
	if err := request.Key.Validate(); err != nil {
		return nil, err
	}
	if request.ExpiresAt.IsZero() {
		return nil, errors.New("push_expiry_required")
	}
	eventRef, err := signer.Sign(ReferenceClaims{Key: request.Key, ExpiresAt: request.ExpiresAt})
	if err != nil {
		return nil, err
	}
	title, body, err := localizedPayloadText(request.Locale, request.Key.Category, request.Preview, request.Retract)
	if err != nil {
		return nil, err
	}
	payload := Payload{
		Version:    1,
		Category:   request.Key.Category,
		Key:        request.Key,
		Title:      title,
		Body:       body,
		Tag:        notificationTag(request.Key),
		URL:        "./#push=" + eventRef,
		Actions:    []notificationAction{},
		ActionRefs: map[string]string{},
		EventRef:   eventRef,
		Retract:    request.Retract,
	}
	if request.Key.Category == CategoryUpdate || request.Key.Category == CategoryTest {
		payload.URL = "./#settings"
	}
	if request.Retract {
		payload.URL = "./"
		payload.EventRef = ""
		return json.Marshal(payload)
	}
	return json.Marshal(payload)
}

func localizedPayloadText(locale string, category Category, preview PreviewMode, retract bool) (string, string, error) {
	if retract {
		return "", "", nil
	}
	zh := localize.NormalizeLocale(locale) == localize.SimplifiedChinese
	switch preview {
	case PreviewHidden:
		if zh {
			return "Herdr 通知", "打开应用查看详情", nil
		}
		return "Herdr notification", "Open the app to view details", nil
	case PreviewQuestion:
		if zh {
			return "需要回复", "打开应用查看问题并回复", nil
		}
		return "Response needed", "Open the app to review and respond", nil
	case PreviewBrief:
		if category == CategoryFinished {
			if zh {
				return "任务已完成", "打开应用查看已完成的任务", nil
			}
			return "Agent finished", "Open the app to view the completed task", nil
		}
		if zh {
			return "简报已就绪", "打开应用查看简报", nil
		}
		return "Brief ready", "Open the app to view the brief", nil
	default:
		return "", "", errors.New("push_invalid_preview")
	}
}

func localizedApproveOnce(locale string) string {
	if localize.NormalizeLocale(locale) == localize.SimplifiedChinese {
		return "仅批准一次"
	}
	return "Approve once"
}

func notificationTag(key PushEventKey) string {
	data, _ := json.Marshal(key)
	digest := sha256.Sum256(data)
	return "herdr-" + hex.EncodeToString(digest[:16])
}
