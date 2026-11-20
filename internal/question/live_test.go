package question

import (
	"reflect"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/question/testfixture"
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
	approval := attentionFixture(t, "omp-tool-approval.ansi")
	for _, tc := range []struct{ name, agent, text string }{
		{"stale ask", "omp", testfixture.StaleOMP(t, ask)},
		{"stale approval", "omp", testfixture.StaleOMP(t, approval)},
		{"trailing output after status", "omp", ask + "\nRecent output"},
		{"clipped ask", "omp", attentionFixture(t, "omp-partial-ask.ansi")},
		{"idle composer", "omp", ask + "\n╭──────╮\n│      │\n╰──────╯"},
		{"borrowed footer", "omp", testfixture.ReplaceRow(t, ask, "⏎ select · n note · ↑/↓ move · ⎋ cancel · Alt+H hide", "│ up/down navigate  enter select  esc cancel │")},
		{"displayed menu", "claude", "Choose a color\n❯ 1. Blue\n  2. Green\n" + claudeQuestionHints[0]},
		{"historical approval", "claude", attentionFixture(t, "claude-single-approval.ansi") + "\n❯ "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agents := []string{tc.agent}
			if tc.agent == "omp" {
				agents = append(agents, "pi")
			}
			for _, agent := range agents {
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
			narrow := testfixture.ReplaceRow(t, wide, tc.hint, tc.wrapped)
			got, ok := ClassifyLive(narrow, "claude")
			if !ok || got.Kind == "" || got.Kind != want.Kind {
				t.Fatalf("got %+v, want %s", got, want.Kind)
			}
			if got.Kind == AttentionQuestion {
				if got.Interaction.ID != want.Interaction.ID || got.Interaction.Focus != want.Interaction.Focus ||
					!reflect.DeepEqual(got.Interaction.Options, want.Interaction.Options) {
					t.Fatalf("got %+v, want %+v", got.Interaction, want.Interaction)
				}
			} else if ApprovalFingerprint(got) != ApprovalFingerprint(want) ||
				got.ApprovalFocus != want.ApprovalFocus || !reflect.DeepEqual(got.Options, want.Options) {
				t.Fatalf("got %+v, want approval options %v at %d", got, want.Options, want.ApprovalFocus)
			}
		})
	}
}

func TestClassifyLiveRejectsTextAfterCompleteApprovalFooter(t *testing.T) {
	frame := attentionFixture(t, "claude-single-approval.ansi")
	for _, output := range []string{"Done", "All done", "All done now"} {
		stale := testfixture.ReplaceRow(t, frame, claudeApprovalHint, claudeApprovalHint+"\n"+output)
		if got, ok := ClassifyLive(stale, "claude"); !ok || got.Kind != "" {
			t.Fatalf("accepted newer %q after complete hint: %+v", output, got)
		}
	}
	frame = attentionFixture(t, "claude-named-session-task-panel.ansi")
	hint := claudeQuestionHints[0]
	stale := testfixture.ReplaceRow(t, frame, hint, hint+"\nDone")
	if got, ok := ClassifyLive(stale, "claude"); !ok || got.Kind != "" {
		t.Fatalf("accepted text between the question and task panel: %+v", got)
	}
}
