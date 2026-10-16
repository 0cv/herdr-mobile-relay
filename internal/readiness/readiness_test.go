package readiness

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVerifyGatewayHealth(t *testing.T) {
	valid := `{"status":"ok","readiness":"ready","transport":"gateway","inventory":{"state":"ready","stale":false},"instance":"instance","release_version":"version","revision":"revision","bundle_hash":"web","bundle_version":"version","bundle_revision":"revision","protocol":3,"gateway":{"enabled":true,"registered":true,"relay_id":"fixture-id"}}`
	expected := Expected{Instance: "instance", Version: "version", Revision: "revision", WebHash: "web"}
	if err := VerifyGatewayHealth(strings.NewReader(valid), expected); err != nil {
		t.Fatal(err)
	}
	for name, response := range map[string]string{
		"malformed":        `{`,
		"not object":       `[]`,
		"trailing":         valid + `{}`,
		"oversized":        valid + strings.Repeat(" ", 64*1024),
		"too deep":         strings.Replace(valid, `"fixture-id"`, strings.Repeat("[", 18)+`0`+strings.Repeat("]", 18), 1),
		"duplicate":        strings.Replace(valid, `"status":"ok"`, `"status":"unknown","status":"ok"`, 1),
		"nested duplicate": strings.Replace(valid, `"registered":true`, `"registered":false,"registered":true`, 1),
		"status":           strings.Replace(valid, `"status":"ok"`, `"status":"ready"`, 1),
		"readiness":        strings.Replace(valid, `"readiness":"ready"`, `"readiness":"unknown"`, 1),
		"transport":        strings.Replace(valid, `"transport":"gateway"`, `"transport":"cloudflare"`, 1),
		"inventory state":  strings.Replace(valid, `"state":"ready"`, `"state":"error"`, 1),
		"stale":            strings.Replace(valid, `"stale":false`, `"stale":true`, 1),
		"missing stale":    strings.Replace(valid, `,"stale":false`, ``, 1),
		"stale type":       strings.Replace(valid, `"stale":false`, `"stale":"false"`, 1),
		"enabled":          strings.Replace(valid, `"enabled":true`, `"enabled":false`, 1),
		"registered":       strings.Replace(valid, `"registered":true`, `"registered":false`, 1),
		"missing flag":     strings.Replace(valid, `"registered":true,`, ``, 1),
		"unknown flag":     strings.Replace(valid, `"registered":true`, `"registered":null`, 1),
		"flag type":        strings.Replace(valid, `"registered":true`, `"registered":"true"`, 1),
		"missing id":       strings.Replace(valid, `"relay_id":"fixture-id"`, `"relay_id":""`, 1),
		"id type":          strings.Replace(valid, `"relay_id":"fixture-id"`, `"relay_id":17`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if VerifyGatewayHealth(strings.NewReader(response), expected) == nil {
				t.Fatal("accepted invalid gateway health")
			}
		})
	}
	for _, key := range []string{"instance", "release_version", "revision", "bundle_hash", "bundle_version", "bundle_revision", "protocol"} {
		for _, replacement := range []any{nil, false, 42, "foreign"} {
			t.Run(key+" wrong value", func(t *testing.T) {
				var response map[string]any
				if err := json.Unmarshal([]byte(valid), &response); err != nil {
					t.Fatal(err)
				}
				response[key] = replacement
				body, err := json.Marshal(response)
				if err != nil {
					t.Fatal(err)
				}
				if VerifyGatewayHealth(strings.NewReader(string(body)), expected) == nil {
					t.Fatal("accepted foreign or incorrectly typed identity")
				}
			})
		}
	}
	for _, incomplete := range []Expected{
		{}, {Instance: "instance"}, {Instance: "instance", Version: "version", Revision: "revision"},
	} {
		if VerifyGatewayHealth(strings.NewReader(valid), incomplete) == nil {
			t.Fatal("accepted incomplete expected identity")
		}
	}
}

func TestVerifyReadiness(t *testing.T) {
	valid := `{"status":"ready","inventory":{"state":"ready","agents":[]},"instance":"instance","release_version":"version","revision":"revision","bundle_hash":"web","protocol":3}`
	expected := Expected{Instance: "instance", Version: "version", Revision: "revision", WebHash: "web"}
	if err := Verify(strings.NewReader(valid), expected); err != nil {
		t.Fatal(err)
	}
	for name, response := range map[string]string{
		"degraded":       strings.Replace(valid, `"state":"ready"`, `"state":"degraded"`, 1),
		"wrong instance": strings.Replace(valid, `"instance":"instance"`, `"instance":"foreign"`, 1),
		"wrong revision": strings.Replace(valid, `"revision":"revision"`, `"revision":"old"`, 1),
		"wrong bundle":   strings.Replace(valid, `"bundle_hash":"web"`, `"bundle_hash":"old"`, 1),
		"duplicate":      strings.Replace(valid, `"status":"ready"`, `"status":"unavailable","status":"ready"`, 1),
		"string spoof":   `{"diagnostic":` + `"status:ready,instance:instance,revision:revision"}`,
		"trailing":       valid + `{}`,
		"wrong type":     strings.Replace(valid, `"instance":"instance"`, `"instance":42`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if Verify(strings.NewReader(response), expected) == nil {
				t.Fatal("accepted invalid readiness")
			}
		})
	}
	if Verify(strings.NewReader(valid), Expected{}) == nil {
		t.Fatal("accepted unspecified instance")
	}
}

func TestVerifyReadinessRejectsOverLimitValidPrefix(t *testing.T) {
	valid := `{"status":"ready","inventory":{"state":"ready"},"instance":"instance","release_version":"version","revision":"revision","bundle_hash":"web","protocol":3}`
	expected := Expected{Instance: "instance", Version: "version", Revision: "revision", WebHash: "web"}
	const limit = 64 * 1024
	// The exact limit is allowed; limit+1 must be rejected even when the
	// document is otherwise valid. Invalid data after the reader limit is
	// the original truncation counterexample, not an ordinary suffix check.
	atLimit := valid + strings.Repeat(" ", limit-len(valid))
	if err := Verify(strings.NewReader(atLimit), expected); err != nil {
		t.Fatal("refused a valid readiness body at the size boundary")
	}
	for name, body := range map[string]string{
		"one byte over":            atLimit + " ",
		"invalid after truncation": atLimit + " !",
		"much larger valid prefix": atLimit + strings.Repeat(" ", limit) + "!",
	} {
		t.Run(name, func(t *testing.T) {
			if err := Verify(strings.NewReader(body), expected); err == nil {
				t.Fatal("accepted an over-limit readiness body with a valid prefix")
			}
		})
	}
}
