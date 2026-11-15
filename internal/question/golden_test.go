package question

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/golden from the current parser output")

type goldenInput struct {
	name  string
	text  string
	agent string
}

func goldenAgent(name string) string {
	for _, prefix := range []string{"claude", "codex", "omp", "opencode", "pi", "hermes", "agy"} {
		if strings.HasPrefix(name, prefix) {
			return prefix
		}
	}
	if strings.HasPrefix(name, "qoder") {
		return "qoder"
	}
	return "claude"
}

func goldenInputs(t *testing.T) []goldenInput {
	t.Helper()
	var inputs []goldenInput
	entries, err := os.ReadDir(filepath.Join("testdata", "attention"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".ansi")
		inputs = append(inputs, goldenInput{name, attentionFixture(t, entry.Name()), goldenAgent(name)})
	}
	idle, err := filepath.Glob(filepath.Join("..", "copyresponse", "testdata", "*-direct-real.ansi"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range idle {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		name := "idle-" + strings.TrimSuffix(filepath.Base(path), ".ansi")
		inputs = append(inputs, goldenInput{name, string(content), goldenAgent(filepath.Base(path))})
	}
	for _, view := range []struct{ name, text string }{
		{"agyApprovalView", agyApprovalView},
		{"codexApprovalView", codexApprovalView},
		{"claudeApprovalView", claudeApprovalView},
		{"claudeBoxedApprovalView", claudeBoxedApprovalView},
		{"claudePermissionView", claudePermissionView},
		{"codexSubagentApprovalView", codexSubagentApprovalView},
		{"qoderApprovalView", qoderApprovalView},
		{"ompPlanApprovalView", ompPlanApprovalView},
		{"hermesApprovalView", hermesApprovalView},
		{"ompToolApprovalView", ompToolApprovalView},
		{"ompWriteApprovalView", ompWriteApprovalView},
		{"multiQuestionView", multiQuestionView},
		{"claudeFirstQuestionView", claudeFirstQuestionView},
		{"claudeAskQuestionView", claudeAskQuestionView},
		{"ompReviewView", ompReviewView},
		{"codexQuestionView", codexQuestionView},
		{"codexFinalQuestionView", codexFinalQuestionView},
		{"codexSingleQuestionView", codexSingleQuestionView},
		{"qoderQuestionView", qoderQuestionView},
		{"qoderMultiQuestionView", qoderMultiQuestionView},
		{"qoderReviewView", qoderReviewView},
		{"claudeStaleNoteQuestionView", claudeStaleNoteQuestionView},
	} {
		inputs = append(inputs, goldenInput{view.name, view.text, goldenAgent(view.name)})
	}
	return inputs
}

func goldenHermesInputs() []goldenInput {
	longCommand := strings.Replace(
		hermesApprovalView,
		"│   4. Deny                                                  │",
		"│   4. Deny                                                  │\n│   5. Show full command                                      │",
		1,
	)
	focused := strings.Replace(
		strings.Replace(longCommand, "│ ❯ 1. Allow once                                            │", "│   1. Allow once                                            │", 1),
		"│   5. Show full command                                      │",
		"│ ❯ 5. Show full command                                      │",
		1,
	)
	return []goldenInput{
		{"hermes-show-full-command", longCommand, "hermes"},
		{"hermes-show-full-command-focused", focused, "hermes"},
	}
}

func goldenInteraction(b *strings.Builder, label string, interaction *Interaction) {
	if interaction == nil {
		fmt.Fprintf(b, "%s: nil\n", label)
		return
	}
	fmt.Fprintf(b, "%s: %+v\n", label, *interaction)
	encoded, err := json.Marshal(interaction)
	if err != nil {
		fmt.Fprintf(b, "%s.json: error %v\n", label, err)
		return
	}
	fmt.Fprintf(b, "%s.json: %s\n", label, encoded)
}

func goldenRun(b *strings.Builder, title, text, agent string) {
	fmt.Fprintf(b, "=== %s\n", title)
	fmt.Fprintf(b, "LayoutHint: %v\n", LayoutHint(text))
	goldenInteraction(b, "Parse", Parse(text, agent))
	classification := Classify(text, agent)
	interaction := classification.Interaction
	classification.Interaction = nil
	fmt.Fprintf(b, "Classify: %+v\n", classification)
	goldenInteraction(b, "Classify.Interaction", interaction)
	classification.Interaction = interaction
	fmt.Fprintf(b, "ApprovalFingerprint: %q\n", ApprovalFingerprint(classification))
}

func goldenEntry(input goldenInput) string {
	var b strings.Builder
	goldenRun(&b, input.name+" agent="+input.agent, input.text, input.agent)
	goldenRun(&b, input.name+" agent=hermes", input.text, "hermes")
	goldenRun(&b, input.name+" agent=agy", input.text, "agy")
	crlf := strings.ReplaceAll(strings.ReplaceAll(input.text, "\r\n", "\n"), "\n", "\r\n")
	goldenRun(&b, input.name+" agent="+input.agent+" crlf", crlf, input.agent)
	return b.String()
}

func goldenHermesEntry(input goldenInput) string {
	var b strings.Builder
	goldenRun(&b, input.name+" agent=hermes", input.text, "hermes")
	return b.String()
}

func TestGolden(t *testing.T) {
	dir := filepath.Join("testdata", "golden")
	want := map[string]string{}
	for _, input := range goldenInputs(t) {
		want[input.name+".txt"] = goldenEntry(input)
	}
	for _, input := range goldenHermesInputs() {
		want[input.name+".txt"] = goldenHermesEntry(input)
	}
	if *update {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, content := range want {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk, expected []string
	for _, entry := range entries {
		onDisk = append(onDisk, entry.Name())
	}
	for name := range want {
		expected = append(expected, name)
	}
	slices.Sort(onDisk)
	slices.Sort(expected)
	if !slices.Equal(onDisk, expected) {
		t.Fatalf("golden files = %v, want %v (run with -update only for an intended behaviour change)", onDisk, expected)
	}
	for _, name := range expected {
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != want[name] {
			t.Errorf("%s differs from the recorded output", name)
		}
	}
}
