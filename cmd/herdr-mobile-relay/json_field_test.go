package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/tailscale"
)

func TestRunJSONFieldEmitsOnlyValidatedScalar(t *testing.T) {
	var stdout bytes.Buffer
	code, err := runJSONField([]string{"bool", "ready"}, strings.NewReader(`{"ready":true}`), &stdout)
	if code != 0 || err != nil || stdout.String() != "true\n" {
		t.Fatalf("runJSONField() = (%d, %v, %q)", code, err, stdout.String())
	}

	stdout.Reset()
	code, err = runJSONField([]string{"string", "origin"}, strings.NewReader(`{"origin":"https://relay.example.test"}`), &stdout)
	if code != 0 || err != nil || stdout.String() != "https://relay.example.test\n" {
		t.Fatalf("string field = (%d, %v, %q)", code, err, stdout.String())
	}

	stdout.Reset()
	code, err = runJSONField([]string{"number", "serve_route_count"}, strings.NewReader(`{"serve_route_count":12}`), &stdout)
	if code != 0 || err != nil || stdout.String() != "12\n" {
		t.Fatalf("number field = (%d, %v, %q)", code, err, stdout.String())
	}

	stdout.Reset()
	code, err = runJSONField([]string{"bool", "ready"}, strings.NewReader(`{"message":{"ready":true}}`), &stdout)
	if code == 0 || err == nil || stdout.Len() != 0 {
		t.Fatalf("nested field was not refused before output: (%d, %v, %q)", code, err, stdout.String())
	}
}

func TestRunJSONFieldRejectsUsageAndOversizedInput(t *testing.T) {
	var stdout bytes.Buffer
	if code, err := runJSONField([]string{"bool"}, strings.NewReader(`{}`), &stdout); code == 0 || err == nil || stdout.Len() != 0 {
		t.Fatalf("invalid usage = (%d, %v, %q)", code, err, stdout.String())
	}

	stdout.Reset()
	large := `{"ready":true,"padding":"` + strings.Repeat("x", tailscale.MaxOutputBytes) + `"}`
	if code, err := runJSONField([]string{"bool", "ready"}, strings.NewReader(large), &stdout); code == 0 || err == nil || stdout.Len() != 0 {
		t.Fatalf("oversized input = (%d, %v, %q)", code, err, stdout.String())
	}
}
