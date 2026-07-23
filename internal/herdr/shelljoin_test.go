package herdr

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func FuzzShellJoinRoundTrip(f *testing.F) {
	f.Add("", "plain", "single'quote")
	f.Add("line\nbreak", "$(touch /tmp/never)", "semi;colon")
	f.Add("spaces and\ttabs", `double"quote`, `back\slash`)

	f.Fuzz(func(t *testing.T, first, second, third string) {
		values := []string{first, second, third}
		for _, value := range values {
			if strings.ContainsRune(value, 0) {
				t.Skip("POSIX argv cannot contain NUL")
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		command := "set -- " + ShellJoin(values) + `; printf '%s\000' "$@"`
		output, err := exec.CommandContext(ctx, "sh", "-c", command).Output()
		if err != nil {
			t.Fatalf("round-trip shell command: %v", err)
		}
		want := []byte(first + "\x00" + second + "\x00" + third + "\x00")
		if !bytes.Equal(output, want) {
			t.Fatalf("round trip = %q, want %q", output, want)
		}
	})
}
