package main

import (
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/gatewaywire"
)

func TestLoadOptionsAllowedRelayIDs(t *testing.T) {
	id, err := gatewaywire.DeriveRelayID("first")
	if err != nil {
		t.Fatal(err)
	}
	other, err := gatewaywire.DeriveRelayID("second")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, raw string
		want      int
		invalid   bool
	}{
		{"unset", "", 0, false},
		{"comma and whitespace", id + ", \t" + other + "\n" + id, 2, false},
		{"short id", "bad", 0, true},
		{"invalid alphabet", strings.Repeat("!", 22), 0, true},
		{"invalid among valid", id + ",bad", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HERDR_GATEWAY_ALLOWED_RELAY_IDS", tc.raw)
			opts, err := loadOptions()
			if tc.invalid {
				if err == nil || !strings.Contains(err.Error(), "HERDR_GATEWAY_ALLOWED_RELAY_IDS") {
					t.Fatalf("error = %v, want named validation error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(opts.AllowedRelayIDs) != tc.want {
				t.Fatalf("allow-list length = %d, want %d", len(opts.AllowedRelayIDs), tc.want)
			}
			if tc.want > 0 && (!opts.AllowedRelayIDs[id] || !opts.AllowedRelayIDs[other]) {
				t.Fatal("allow-list lost an id")
			}
		})
	}
}
