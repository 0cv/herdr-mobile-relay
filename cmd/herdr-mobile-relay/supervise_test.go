package main

import (
	"strings"
	"testing"
	"time"
)

func TestSuperviseRejectsInvalidInvocationBeforeStartingACommand(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--"},
		{"--", ""},
		{"--grace"},
		{"--grace", "99ms", "--", "touch", "should-not-exist"},
		{"--grace", "31s", "--", "touch", "should-not-exist"},
		{"--grace", "0", "--", "touch", "should-not-exist"},
		{"--grace", "1s", "--grace", "2s", "--", "touch", "should-not-exist"},
		{"--unknown", "--", "touch", "should-not-exist"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, err := runSupervise(args, nil, nil, nil)
			if code != 2 || err == nil {
				t.Fatalf("runSupervise(%q) = (%d, %v), want usage error", args, code, err)
			}
		})
	}
}

func TestSuperviseGraceBounds(t *testing.T) {
	for _, test := range []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{value: "100ms", want: 100 * time.Millisecond, ok: true},
		{value: "30s", want: 30 * time.Second, ok: true},
		{value: "99ms"},
		{value: "30.001s"},
		{value: "forever"},
	} {
		t.Run(test.value, func(t *testing.T) {
			options, err := parseSuperviseArgs([]string{"--grace", test.value, "--", "echo", "argument"})
			if test.ok {
				if err != nil || options.grace != test.want {
					t.Fatalf("parse = (%+v, %v), want grace %s", options, err, test.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted out-of-range grace %q", test.value)
			}
		})
	}
}
