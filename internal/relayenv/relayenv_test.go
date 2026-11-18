package relayenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEnvFile(t *testing.T, content string) {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "relay.env")
	if err := os.WriteFile(filename, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_RELAY_ENV", filename)
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "")
}

func TestReadAcceptsLiteralForms(t *testing.T) {
	tests := map[string]struct {
		line string
		want string
	}{
		"plain":                    {"KEY=value", "value"},
		"plain_with_comment":       {"KEY=value # note", "value"},
		"single_quoted":            {"KEY='a value'", "a value"},
		"single_quoted_dollar":     {"KEY='$HOME'", "$HOME"},
		"single_quoted_comment":    {"KEY='a # b' # note", "a # b"},
		"double_quoted":            {`KEY="a value"`, "a value"},
		"double_quoted_comment":    {`KEY="a # b" # note`, "a # b"},
		"export":                   {"export KEY='value'", "value"},
		"empty":                    {"KEY=", ""},
		"surrounding_space":        {"  KEY=value  ", "value"},
		"last_assignment_wins":     {"KEY=first\nKEY=second", "second"},
		"comment_line_is_skipped":  {"# KEY=other\nKEY=value", "value"},
		"unrelated_dollar_ignored": {"OTHER=$X\nKEY=value", "value"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			writeEnvFile(t, test.line+"\n")
			values, err := Read("KEY")
			if err != nil {
				t.Fatal(err)
			}
			if values["KEY"] != test.want {
				t.Fatalf("KEY = %q, want %q", values["KEY"], test.want)
			}
		})
	}
}

func TestReadReturnsOnlyRequestedKeys(t *testing.T) {
	writeEnvFile(t, "A=1\nB=2\n")
	values, err := Read("A")
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values["A"] != "1" {
		t.Fatalf("values = %#v", values)
	}
}

func TestReadRejectsVariableReferenceNamingKey(t *testing.T) {
	for name, line := range map[string]string{
		"plain":         "KEY=$OTHER",
		"braced":        "KEY=${OTHER}",
		"double_quoted": `KEY="$OTHER"`,
		"command":       "KEY=$(date)",
	} {
		t.Run(name, func(t *testing.T) {
			writeEnvFile(t, line+"\n")
			_, err := Read("KEY")
			if err == nil || !strings.Contains(err.Error(), "KEY") {
				t.Fatalf("error = %v, want one naming KEY", err)
			}
		})
	}
}

func TestReadRejectsMalformedQuotes(t *testing.T) {
	for name, line := range map[string]string{
		"unterminated":  "KEY='abc",
		"trailing_text": "KEY='abc' def",
	} {
		t.Run(name, func(t *testing.T) {
			writeEnvFile(t, line+"\n")
			if _, err := Read("KEY"); err == nil {
				t.Fatal("malformed value was accepted")
			}
		})
	}
}

func TestReadMissingFileYieldsNoValues(t *testing.T) {
	t.Setenv("HERDR_RELAY_ENV", filepath.Join(t.TempDir(), "absent.env"))
	values, err := Read("KEY")
	if err != nil || len(values) != 0 {
		t.Fatalf("values = %#v, err = %v", values, err)
	}
}

func TestPath(t *testing.T) {
	t.Setenv("HERDR_RELAY_ENV", "")
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "")
	if got := Path(); got != "" {
		t.Fatalf("Path() = %q, want empty", got)
	}
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", "/plugin")
	if got := Path(); got != "/plugin/relay.env" {
		t.Fatalf("Path() = %q", got)
	}
	t.Setenv("HERDR_RELAY_ENV", "/explicit/relay.env")
	if got := Path(); got != "/explicit/relay.env" {
		t.Fatalf("Path() = %q", got)
	}
}
