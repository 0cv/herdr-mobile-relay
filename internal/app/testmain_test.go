package app

import (
	"fmt"
	"os"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/p1fixture"
)

func TestMain(m *testing.M) {
	if err := p1fixture.PrepareEnvironment(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
