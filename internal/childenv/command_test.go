package childenv

import (
	"context"
	"os/exec"
	"reflect"
	"testing"
)

func TestWithoutReleaseCredentials(t *testing.T) {
	input := []string{"GH_TOKEN=fixture", "PATH=/bin", "GITHUB_TOKEN=fixture", "HERDR_GITHUB_TOKEN_FILE=/private/fixture", "HTTPS_PROXY=http://proxy.invalid", "CLAUDE_CONFIG_DIR=/agent", "GH_TOKEN=duplicate"}
	want := []string{"PATH=/bin", "HTTPS_PROXY=http://proxy.invalid", "CLAUDE_CONFIG_DIR=/agent"}
	if got := WithoutReleaseCredentials(input); !reflect.DeepEqual(got, want) {
		t.Fatal("unexpected filtered environment")
	}
	if got := WithoutReleaseCredentials(nil); got == nil {
		t.Fatal("empty environment must not inherit ambient credentials")
	}
	if len(input) != 7 || input[0] != "GH_TOKEN=fixture" {
		t.Fatal("input was mutated")
	}
}

func TestCommandsIsolateReleaseCredentials(t *testing.T) {
	t.Setenv("GH_TOKEN", "synthetic")
	t.Setenv("GITHUB_TOKEN", "synthetic")
	t.Setenv("HERDR_GITHUB_TOKEN_FILE", "/private/synthetic")
	t.Setenv("CHILDENV_TEST_VALUE", "preserved")
	for name, create := range map[string]func() *exec.Cmd{
		"command": func() *exec.Cmd {
			return Command("/bin/sh", "-c", `test -z "${GH_TOKEN+x}${GITHUB_TOKEN+x}${HERDR_GITHUB_TOKEN_FILE+x}" && test "$CHILDENV_TEST_VALUE" = preserved`)
		},
		"context": func() *exec.Cmd {
			return CommandContext(context.Background(), "/bin/sh", "-c", `test -z "${GH_TOKEN+x}${GITHUB_TOKEN+x}${HERDR_GITHUB_TOKEN_FILE+x}" && test "$CHILDENV_TEST_VALUE" = preserved`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := create().Run(); err != nil {
				t.Fatalf("child credential boundary failed: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CommandContext(ctx, "/bin/sh", "-c", "exit 0").Run(); err == nil {
		t.Fatal("cancelled child started")
	}
}
