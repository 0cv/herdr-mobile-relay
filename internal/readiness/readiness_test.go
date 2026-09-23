package readiness

import (
	"strings"
	"testing"
)

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
