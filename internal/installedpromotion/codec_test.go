package installedpromotion

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/config"
)

func TestSelectionCodec(t *testing.T) {
	c := config.Config{}
	v := reflect.ValueOf(&c).Elem()
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString("private-\"\\\n-é")
		case reflect.Int:
			f.SetInt(int64(i + 1))
		case reflect.Float64:
			f.SetFloat(math.Copysign(0, -1))
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Slice:
			f.Set(reflect.ValueOf([]string{"private-one", "private-two"}))
		}
	}
	gen, pin := strings.Repeat("1", 32), strings.Repeat("2", 64)
	data, e := encode(c, gen, pin)
	if e != nil {
		t.Fatal("encode refused")
	}
	t.Run("complete", func(t *testing.T) {
		if !schema() {
			t.Fatal("schema drift")
		}
		got, _, e := decode(data)
		if e != nil || !reflect.DeepEqual(c, got) || math.Float64bits(got.PollInterval) != math.Float64bits(c.PollInterval) {
			t.Fatal("lossy snapshot")
		}
	})
	for _, tc := range []struct {
		name   string
		change func([]byte) []byte
	}{
		{"duplicate", func(b []byte) []byte { return append([]byte(`{"Version":1,`), b[1:]...) }},
		{"case", func(b []byte) []byte { return []byte(strings.Replace(string(b), `"Version"`, `"version"`, 1)) }},
		{"unknown", func(b []byte) []byte { return append([]byte(`{"Unknown":0,`), b[1:]...) }},
		{"missing", func(b []byte) []byte { return []byte(strings.Replace(string(b), `"Version":1,`, "", 1)) }},
		{"version", func(b []byte) []byte { return []byte(strings.Replace(string(b), `"Version":1`, `"Version":2`, 1)) }},
		{"type", func(b []byte) []byte { return []byte(strings.Replace(string(b), `"Version":1`, `"Version":"1"`, 1)) }},
		{"trailing", func(b []byte) []byte { return append(b, '0') }},
		{"utf8", func(b []byte) []byte { return append(b, 0xff) }},
		{"nested-duplicate", func(b []byte) []byte {
			return []byte(strings.Replace(string(b), `"Config":{`, `"Config":{"Host":"other",`, 1))
		}},
		{"nested-null", func(b []byte) []byte {
			var w capsule
			json.Unmarshal(b, &w)
			w.Config["Port"] = json.RawMessage("null")
			out, _ := json.Marshal(w)
			return out
		}},
		{"oversize", func(b []byte) []byte { return []byte(strings.Repeat("x", maxBytes+1)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _, e := decode(tc.change(append([]byte(nil), data...))); e == nil || !reflect.DeepEqual(got, config.Config{}) {
				t.Fatal("malformed payload released")
			}
		})
	}
	for _, tc := range []struct {
		name   string
		change func(*config.Config)
	}{
		{"nonfinite", func(c *config.Config) { c.PollInterval = math.Inf(1) }},
		{"invalid-string", func(c *config.Config) { c.Token = "\xff" }},
		{"long-string", func(c *config.Config) { c.WebRoot = strings.Repeat("x", 4097) }},
		{"long-list", func(c *config.Config) { c.AllowedOrigins = make([]string, 129) }},
		{"total-bound", func(c *config.Config) {
			c.AllowedOrigins = make([]string, 128)
			for i := range c.AllowedOrigins {
				c.AllowedOrigins[i] = strings.Repeat("x", 4096)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := c
			tc.change(&bad)
			if _, e := encode(bad, gen, pin); e == nil {
				t.Fatal("unbounded input accepted")
			}
		})
	}
	t.Run("nil-empty", func(t *testing.T) {
		for _, slice := range [][]string{nil, {}} {
			c.AllowedOrigins = slice
			c.GatewayURLs = slice
			b, e := encode(c, gen, pin)
			if e != nil {
				t.Fatal(e)
			}
			got, _, e := decode(b)
			if e != nil || !reflect.DeepEqual(got, c) {
				t.Fatal("slice shape changed")
			}
		}
	})
	t.Run("redaction", func(t *testing.T) {
		for _, value := range []any{capsule{Anchor: "secret-canary"}, Reference{"secret-canary", "secret-canary"}, ErrRefused, ErrUncertain} {
			if strings.Contains(fmt.Sprintf("%v %+v %#v", value, value, value), "secret-canary") {
				t.Fatal("private format leaked")
			}
		}
	})
}
