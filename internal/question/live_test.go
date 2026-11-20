package question

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestClassifyLive(t *testing.T) {
	for _, tc := range []struct{ fixture, agent string }{
		{"claude-single-question.ansi", "claude"},
		{"claude-multi-select.ansi", "claude"},
		{"claude-named-session-task-panel.ansi", "claude"},
		{"claude-plan-submit-answers.ansi", "claude"},
		{"claude-single-approval.ansi", "claude"},
		{"omp-ask-status-below.ansi", "omp"},
		{"omp-ask-stock.ansi", "pi"},
		{"omp-tool-approval.ansi", "omp"},
		{"codex-single-question.ansi", "codex"},
		{"codex-first-question.ansi", "codex"},
		{"codex-final-approval.ansi", "codex"},
		{"qodercli-second-question-single.ansi", "qoder"},
		{"qodercli-submit-answers.ansi", "qoder"},
		{"opencode-single-question.ansi", "opencode"},
		{"opencode-many-questions-confirm.ansi", "opencode"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			frame := attentionFixture(t, tc.fixture)
			want := Classify(frame, tc.agent)
			got, ok := ClassifyLive(frame, tc.agent)
			if !ok || got.Kind != want.Kind || (got.Kind != AttentionQuestion && got.Kind != AttentionApproval) {
				t.Fatalf("got %+v (reliable=%t), want %s", got, ok, want.Kind)
			}
			if want.Kind == AttentionQuestion {
				if !reflect.DeepEqual(got.Interaction, want.Interaction) {
					t.Fatalf("got %+v, want %+v", got.Interaction, want.Interaction)
				}
			} else if ApprovalFingerprint(got) == "" || !reflect.DeepEqual(got.Options, want.Options) || got.ApprovalFocus != want.ApprovalFocus {
				t.Fatalf("got %+v, want options %v with focus %d", got, want.Options, want.ApprovalFocus)
			}
		})
	}
}

func TestClassifyLiveBoundedApprovals(t *testing.T) {
	for _, tc := range []struct{ agent, frame string }{
		{"hermes", hermesApprovalView},
		{"agy", agyApprovalView},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			want := Classify(tc.frame, tc.agent)
			got, ok := ClassifyLive(tc.frame, tc.agent)
			if !ok || got.Kind != AttentionApproval || ApprovalFingerprint(got) == "" ||
				!reflect.DeepEqual(got.Options, want.Options) || got.ApprovalFocus != want.ApprovalFocus {
				t.Fatalf("got %+v, want options %v with focus %d", got, want.Options, want.ApprovalFocus)
			}
			if stale, ok := ClassifyLive(tc.frame+"\nDone", tc.agent); !ok || stale.Kind != "" {
				t.Fatalf("accepted historical approval: %+v", stale)
			}
		})
	}
}

func TestClassifyLiveRejectsSupersededControls(t *testing.T) {
	ask := attentionFixture(t, "omp-ask-status-below.ansi")
	fixture := func(name string) string {
		data, err := os.ReadFile(filepath.Join("testdata", "live", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	for _, tc := range []struct{ name, agent, text string }{
		{"stale ask", "omp", fixture("omp-ask-stale-before-status.ansi")},
		{"stale approval", "omp", fixture("omp-tool-approval-stale-before-status.ansi")},
		{"trailing output after status", "omp", ask + "\nRecent output"},
		{"clipped ask", "omp", attentionFixture(t, "omp-partial-ask.ansi")},
		{"idle composer", "omp", ask + "\n╭──────╮\n│      │\n╰──────╯"},
		{"borrowed footer", "omp", replaceLiveRow(ask, "⏎ select · n note · ↑/↓ move · ⎋ cancel · Alt+H hide", "│ up/down navigate  enter select  esc cancel │")},
		{"displayed menu", "claude", "Choose a color\n❯ 1. Blue\n  2. Green\n" + claudeQuestionHints[0]},
		{"historical approval", "claude", attentionFixture(t, "claude-single-approval.ansi") + "\n❯ "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, agent := range []string{tc.agent, "pi"} {
				if got, ok := ClassifyLive(tc.text, agent); !ok || got.Kind != "" {
					t.Fatalf("%s: %+v (reliable=%t)", agent, got, ok)
				}
			}
		})
	}
}

func TestClassifyLiveClaudeWrappedHint(t *testing.T) {
	for _, tc := range []struct{ fixture, hint, wrapped string }{
		{"claude-single-question.ansi", claudeQuestionHints[0], "Enter to select · ↑/↓ to navigate · Esc to\ncancel"},
		{"claude-multi-select.ansi", claudeQuestionHints[0], "Enter to select ·\n↑/↓ to navigate · Esc to\ncancel"},
		{"claude-named-session-task-panel.ansi", claudeQuestionHints[0], "Enter to select · ↑/↓ to navigate · Esc to\ncancel"},
		{"claude-single-approval.ansi", claudeApprovalHint, "Esc to cancel · Tab to\namend"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			wide := attentionFixture(t, tc.fixture)
			want, _ := ClassifyLive(wide, "claude")
			narrow := replaceLiveRow(wide, tc.hint, tc.wrapped)
			if narrow == wide {
				t.Fatal("hint not split")
			}
			if got, ok := ClassifyLive(narrow, "claude"); !ok || got.Kind == "" || !reflect.DeepEqual(got, want) {
				t.Fatalf("got %+v, want %+v", got, want)
			}
			for _, output := range []string{"Done", "All done", "All done now"} {
				stale := replaceLiveRow(wide, tc.hint, tc.hint+"\n"+output)
				if got, ok := ClassifyLive(stale, "claude"); !ok || got.Kind != "" {
					t.Fatalf("accepted newer %q after complete hint: %+v", output, got)
				}
			}
		})
	}
}

func replaceLiveRow(frame, row, replacement string) string {
	lines := strings.Split(frame, "\n")
	for i, line := range lines {
		if cleanLine(line) == row {
			lines[i] = replacement
			break
		}
	}
	return strings.Join(lines, "\n")
}
