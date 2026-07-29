package panedelta

import (
	"fmt"
	"strings"
	"testing"
)

func TestBuildReconstructsTerminalFrames(t *testing.T) {
	tests := []struct {
		name     string
		previous string
		current  string
	}{
		{
			name:     "append",
			previous: "one\ntwo\nthree\nfour\n",
			current:  "one\ntwo\nthree\nfour\nfive\n",
		},
		{
			name:     "rolling history",
			previous: "one\ntwo\nthree\nfour\nfive\n",
			current:  "two\nthree\nfour\nfive\nsix\n",
		},
		{
			name:     "middle rewrite",
			previous: "one\ntwo\nthree\nfour\nfive\n",
			current:  "one\ntwo\nTHREE\nfour\nfive\n",
		},
		{
			name:     "unicode and missing final newline",
			previous: "α\nβ\nγ\nδ\n",
			current:  "α\nβ\nγ\n🐑",
		},
		{
			name:     "empty",
			previous: "one\ntwo\nthree\n",
			current:  "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			segments := Build(test.previous, test.current)
			actual, ok := Apply(test.previous, segments)
			if !ok {
				t.Fatalf("delta was invalid: %#v", segments)
			}
			if actual != test.current {
				t.Fatalf("reconstructed frame = %q, want %q; segments=%#v", actual, test.current, segments)
			}
		})
	}
}

func TestEfficientRecognizesSmallAppendToLargeFrame(t *testing.T) {
	var previous strings.Builder
	for index := 0; index < 1_000; index++ {
		fmt.Fprintf(&previous, "terminal history row %04d with repeated context\n", index)
	}
	current := previous.String() + "one new row\n"
	segments := Build(previous.String(), current)
	if !Efficient(segments, current) {
		t.Fatalf("small append was not considered efficient: %#v", segments)
	}
	actual, ok := Apply(previous.String(), segments)
	if !ok || actual != current {
		t.Fatal("efficient delta did not reconstruct the frame")
	}
}

func TestApplyRejectsInvalidCopyRange(t *testing.T) {
	if _, ok := Apply("one\ntwo\n", []Segment{{CopyStart: 2, CopyLines: 2}}); ok {
		t.Fatal("out-of-range copy was accepted")
	}
}
