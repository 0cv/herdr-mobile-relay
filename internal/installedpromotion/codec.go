// Package installedpromotion stores private selection data, never service authority.
package installedpromotion

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/0cv/herdr-mobile-relay/internal/config"
)

const maxBytes = 128 * 1024

var ErrRefused = errors.New("installed selection refused")
var ErrUncertain = errors.New("installed selection persistence uncertain; retain reference")

// Reference is an immutable generation plus independently retained exact-byte digest.
// Neither identifier is a path, credential, or readiness claim.
type Reference struct {
	Generation string
	Digest     string
}

func (Reference) String() string     { return "installed selection reference [redacted]" }
func (r Reference) GoString() string { return r.String() }

// Schema additions require explicit review; reflection cannot silently drop fields.
const fields = "Host Port PluginPort Token InstanceID AllowedOrigins WebRoot HerdrBin SocketPath PollInterval RuntimeDir LogFormat LogLevel ReleaseRoot ServiceName Transport TailscaleOrigin ExternalHTTPSOrigin PhoneAppOrigin TailscaleBin TailscaleCLIOrigin TailscaleCLIStateRoot TailscaleCLICoordinationRoot TailscaleCLIDevelopmentRoot TailscaleCLIScope TailscaleCLIBin PairingSocketPath ManagedRunID ControlRunID GatewayURL GatewayURLs GatewayRegistrationToken GatewaySelection WebRTCUDPPort ForceRelayTransport PortMappingEnabled RearmBootstrap CacheDir ConfigHome DataHome"

type capsule struct {
	Version      int
	Generation   string
	Anchor       string
	Installation string
	Pin          string
	Config       map[string]json.RawMessage
}

func (capsule) String() string     { return "installed selection payload [redacted]" }
func (c capsule) GoString() string { return c.String() }
func hexID(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func schema() bool {
	t := reflect.TypeOf(config.Config{})
	names := strings.Fields(fields)
	if t.NumField() != len(names) {
		return false
	}
	for i, n := range names {
		if t.Field(i).Name != n {
			return false
		}
	}
	return true
}
func bounded(c config.Config) bool {
	if !schema() {
		return false
	}
	v := reflect.ValueOf(c)
	total := 0
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.String:
			if !utf8.ValidString(f.String()) || f.Len() > 4096 {
				return false
			}
			total += f.Len()
		case reflect.Slice:
			if f.Len() > 128 {
				return false
			}
			for j := 0; j < f.Len(); j++ {
				s := f.Index(j).String()
				if len(s) > 4096 || !utf8.ValidString(s) {
					return false
				}
				total += len(s)
			}
		case reflect.Float64:
			if math.IsNaN(f.Float()) || math.IsInf(f.Float(), 0) {
				return false
			}
		case reflect.Int, reflect.Bool:
		default:
			return false
		}
		if total > maxBytes/2 {
			return false
		}
	}
	return true
}
func encode(c config.Config, generation, pin string) ([]byte, error) {
	if !bounded(c) || !hexID(generation, 32) || !hexID(pin, 64) {
		return nil, ErrRefused
	}
	w := capsule{1, generation, c.ConfigHome, c.InstanceID, pin, make(map[string]json.RawMessage)}
	v := reflect.ValueOf(c)
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		var value any = f.Interface()
		if f.Kind() == reflect.Int {
			value = f.Int()
		} // LogLevel must remain an exact integer.
		b, e := json.Marshal(value)
		if e != nil {
			return nil, ErrRefused
		}
		w.Config[t.Field(i).Name] = b
	}
	b, e := json.Marshal(w)
	if e != nil || len(b) > maxBytes {
		return nil, ErrRefused
	}
	return b, nil
}

// Only the canonical schema encoding is accepted. Exact re-encoding also rejects
// duplicate/case/unknown/missing fields, null scalars, alternate types and trailing
// data throughout both objects, without permissive JSON normalization.
func decode(data []byte) (config.Config, capsule, error) {
	var c config.Config
	var w capsule
	if len(data) == 0 || len(data) > maxBytes || !utf8.Valid(data) || !schema() || json.Unmarshal(data, &w) != nil || w.Version != 1 {
		return c, capsule{}, ErrRefused
	}
	v := reflect.ValueOf(&c).Elem()
	t := v.Type()
	if len(w.Config) != v.NumField() {
		return c, capsule{}, ErrRefused
	}
	for i := 0; i < v.NumField(); i++ {
		raw, ok := w.Config[t.Field(i).Name]
		if !ok {
			return config.Config{}, capsule{}, ErrRefused
		}
		f := v.Field(i)
		if f.Kind() == reflect.Int {
			var n int64
			if json.Unmarshal(raw, &n) != nil || f.OverflowInt(n) {
				return config.Config{}, capsule{}, ErrRefused
			}
			f.SetInt(n)
		} else if json.Unmarshal(raw, f.Addr().Interface()) != nil {
			return config.Config{}, capsule{}, ErrRefused
		}
	}
	canonical, e := encode(c, w.Generation, w.Pin)
	if e != nil || !bytes.Equal(data, canonical) {
		return config.Config{}, capsule{}, ErrRefused
	}
	return c, w, nil
}
func digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
