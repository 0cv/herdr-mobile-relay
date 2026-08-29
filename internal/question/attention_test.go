package question

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const codexApprovalView = `
• I need to run the checks.
─ Worked for 2s ─

Would you like to run this command?
$ make check
❯ 1. Approve
  2. Reject
Enter to select · Esc to cancel
`

const claudeApprovalView = `
Do you want to proceed?
Bash command
$ npm test
❯ 1. Yes
  2. Yes, and remember this choice
  3. No
Esc to cancel · Enter to confirm
`

const claudePermissionView = `
Claude needs your permission to use Bash.
$ npm test
❯ 1. Allow once
  2. Reject
Esc to cancel · Enter to confirm
`

const codexSubagentApprovalView = `
Approve all pending agents?
❯ 1. Approve all pending
  2. Configure individually
  3. Exit (cancel subagents)
`

const qoderApprovalView = `
Allow this command?
$ go test ./...
❯ 1. Allow
  2. Reject
Tab/←→ switch · Enter select · Esc cancel
`

const ompPlanApprovalView = `
╭─ Plan Review ──────────────────────────────────────────────╮
│  3-Day Itinerary Template (Fri evening – Sun)              │
│  Applies to whichever option is chosen.                    │
├────────────────────────────────────────────────────────────┤
│ Plan mode - next step                                      │
│ continue with  ◂   default   OPUS  FABLE  SONNET  LU… │
│   ↳ GPT-5.6-Sol                                            │
│  Approve and execute                                      │
│   Approve and compact context                              │
│   Approve and keep context (~59k / 1m)                     │
│   Refine plan                                              │
├────────────────────────────────────────────────────────────┤
│ ↑↓ scroll · ⇧ faster · pgup/pgdn · g/G ends · a annotate … │
╰────────────────────────────────────────────────────────────╯
`

const ompToolApprovalView = `
╭─ Allow tool: bash ─────────────────────────────────────────────────────╮
│                                                                        │
│ Command: touch /tmp/push-approval-test                                 │
│                                                                        │
│   Approve                                                             │
│    Deny                                                                │
│                                                                        │
│ up/down navigate  enter select  esc cancel                             │
│                                                                        │
╰────────────────────────────────────────────────────────────────────────╯
`

const ompWriteApprovalView = `
╭─ Allow tool: write ────────────────────────────────────────────────────╮
│                                                                        │
│ Path: /tmp/variant-check.txt                                           │
│ Content:                                                               │
│ hello                                                                  │
│                                                                        │
│   Approve                                                             │
│    Deny                                                                │
│                                                                        │
│ up/down navigate  enter select  esc cancel                             │
│                                                                        │
╰────────────────────────────────────────────────────────────────────────╯
`

func TestClassifyLiveApprovalsByAgent(t *testing.T) {
	tests := []struct {
		name    string
		agent   string
		content string
		want    []string
	}{
		{"codex tool", "codex", codexApprovalView, []string{"Approve", "Reject"}},
		{"codex subagents", "codex", codexSubagentApprovalView, []string{"Approve all pending", "Configure individually", "Exit (cancel subagents)"}},
		{"claude proceed", "claude", claudeApprovalView, []string{"Yes", "Yes, and remember this choice", "No"}},
		{"claude permission", "claude", claudePermissionView, []string{"Allow once", "Reject"}},
		{"qoder allow", "qodercli", qoderApprovalView, []string{"Allow", "Reject"}},
		{"omp plan review", "omp", ompPlanApprovalView, []string{
			"Approve and execute", "Approve and compact context",
			"Approve and keep context (~59k / 1m)", "Refine plan",
		}},
		{"omp tool approval", "omp", ompToolApprovalView, []string{"Approve", "Deny"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Classify(test.content, test.agent)
			if got.Kind != AttentionApproval || !reflect.DeepEqual(got.Options, test.want) {
				t.Fatalf("classification = %+v, want approval options %#v", got, test.want)
			}
		})
	}
}

func TestClassifyOMPPlanApprovalFocus(t *testing.T) {
	first := Classify(ompPlanApprovalView, "omp")
	if first.Kind != AttentionApproval || first.ApprovalFocus != 0 {
		t.Fatalf("classification = %+v, want focus 0", first)
	}
	moved := strings.Replace(ompPlanApprovalView, "\uf054 Approve and execute", "  Approve and execute", 1)
	moved = strings.Replace(moved, "  Refine plan", "❯ Refine plan", 1)
	shifted := Classify(moved, "omp")
	if shifted.Kind != AttentionApproval || shifted.ApprovalFocus != 3 {
		t.Fatalf("shifted classification = %+v, want focus 3", shifted)
	}
	stale := strings.Replace(ompPlanApprovalView, "Plan mode - next step", "Plan mode was here", 1)
	if got := Classify(stale, "omp"); got.Kind == AttentionApproval {
		t.Fatalf("stale classification = %+v, want no approval", got)
	}
}

func TestClassifyOMPToolApproval(t *testing.T) {
	t.Run("bash dialog with focus on Approve", func(t *testing.T) {
		got := Classify(ompToolApprovalView, "omp")
		if got.Kind != AttentionApproval || got.ApprovalFocus != 0 ||
			got.Command != "touch /tmp/push-approval-test" {
			t.Fatalf("classification = %+v, want focus 0 and the dialog's command", got)
		}
	})
	t.Run("focus marker on Deny", func(t *testing.T) {
		moved := strings.Replace(ompToolApprovalView, "\uf054 Approve", "  Approve", 1)
		moved = strings.Replace(moved, "   Deny", " \uf054 Deny", 1)
		got := Classify(moved, "omp")
		if got.Kind != AttentionApproval || got.ApprovalFocus != 1 {
			t.Fatalf("classification = %+v, want focus 1", got)
		}
	})
	t.Run("wrapped command folds into the command, not the options", func(t *testing.T) {
		wrapped := strings.Replace(ompToolApprovalView,
			"│ Command: touch /tmp/push-approval-test                                 │",
			"│ Command: touch /tmp/push-approval-test-with-a-name-so-wide-it-wraps-on │\n│   to-the-next-row                                                      │", 1)
		got := Classify(wrapped, "omp")
		if got.Kind != AttentionApproval ||
			!reflect.DeepEqual(got.Options, []string{"Approve", "Deny"}) {
			t.Fatalf("classification = %+v, want the two real controls only", got)
		}
	})
	t.Run("status footer below a live dialog", func(t *testing.T) {
		got := Classify(ompToolApprovalView+"\n J 5h 10% w 42% · omp\n codexS 5h 4% │ cache till 6:27pm\n", "omp")
		if got.Kind != AttentionApproval {
			t.Fatalf("classification = %+v, want approval", got)
		}
	})
	t.Run("scrolled-away dialog with trailing content", func(t *testing.T) {
		stale := ompToolApprovalView + "\n● Ran tool\n  one\n  two\n  three\n  four\n  five\n  six\n"
		if got := Classify(stale, "omp"); got.Kind == AttentionApproval {
			t.Fatalf("classification = %+v, want no approval", got)
		}
	})
	t.Run("dismissed dialog with a completed-turn line", func(t *testing.T) {
		dismissed := ompToolApprovalView + "\n● Ran tool\nWorked for 12s\n"
		if got := Classify(dismissed, "omp"); got.Kind == AttentionApproval {
			t.Fatalf("classification = %+v, want no approval", got)
		}
	})
	t.Run("non-control rows under the focus marker", func(t *testing.T) {
		prose := strings.Replace(ompToolApprovalView, "\uf054 Approve", "\uf054 This tool edits files", 1)
		prose = strings.Replace(prose, "   Deny", "   outside the workspace", 1)
		if got := Classify(prose, "omp"); got.Kind == AttentionApproval {
			t.Fatalf("classification = %+v, want no approval", got)
		}
	})
	t.Run("detail rows flush against the controls", func(t *testing.T) {
		flush := strings.Replace(ompWriteApprovalView,
			"│ hello                                                                  │\n│                                                                        │\n",
			"│ hello                                                                  │\n", 1)
		if got := Classify(flush, "omp"); got.Kind == AttentionApproval {
			t.Fatalf("classification = %+v, want refusal of the undelimited menu", got)
		}
	})
	t.Run("write dialog surfaces detail rows as the command", func(t *testing.T) {
		got := Classify(ompWriteApprovalView, "omp")
		if got.Kind != AttentionApproval || got.ApprovalFocus != 0 ||
			!reflect.DeepEqual(got.Options, []string{"Approve", "Deny"}) ||
			got.Command != "Path: /tmp/variant-check.txt Content: hello" {
			t.Fatalf("classification = %+v, want the two controls and the detail rows", got)
		}
	})
}

func TestClassifyStructuredQuestionsBeforeApprovalMenus(t *testing.T) {
	for _, test := range []struct {
		agent   string
		content string
	}{
		{"codex", codexQuestionView},
		{"claude", claudeFirstQuestionView},
		{"omp", claudeAskQuestionView},
		{"qoder", qoderQuestionView},
		{"qoder", qoderReviewView},
	} {
		got := Classify(test.content, test.agent)
		if got.Kind != AttentionQuestion || got.Interaction == nil || len(got.Options) != 0 {
			t.Fatalf("%s classification = %+v, want structured question", test.agent, got)
		}
	}
}

func TestClassifyOrdinaryQuestionsAndNumberedPlansAsChat(t *testing.T) {
	content := `
• Hello! What would you like to work on next?

  1. Add parser fixtures.
  2. Verify the backend transition.
  3. Run the browser tests.

─ Worked for 8s ─

›
gpt-5.6-sol xhigh · ~/project · main · Context 30% used
`
	got := Classify(content, "codex")
	if got.Kind != AttentionChat || len(got.Options) != 0 || got.Interaction != nil {
		t.Fatalf("classification = %+v, want chat without controls", got)
	}
}

func TestClassifyIdleInputAfterProviderErrorAsChat(t *testing.T) {
	tests := []struct {
		agent string
		tail  string
	}{
		{agent: "claude", tail: "❯\nOpus 5 | ctx: 31% | main"},
		{agent: "codex", tail: "› Use /skills to list available skills\ngpt-5.6-sol · Context 27% used"},
		{agent: "omp", tail: "╭── GPT-5.6-Luna · xhi · ~/project · main ╮\n╰─                                      ─╯"},
		{agent: "pi", tail: "────────────────\n\n────────────────\n~/project (main)\n$0.000 (sub) 0.0%/272k (auto) · gpt-5.6-luna"},
		{agent: "opencode", tail: "┃ Ask anything... \"Fix broken tests\" ┃\n┃ Build · Qwen3.6-27B ┃"},
		{agent: "kimi", tail: "╭────────────────╮\n│ >              │\n╰────────────────╯\n~/project main\ncontext: 0%"},
		{agent: "qodercli", tail: "> Type your message or @path/to/file\nctx 22% of 200k"},
	}
	for _, test := range tests {
		t.Run(test.agent, func(t *testing.T) {
			content := "Error: Retry failed after 1 attempts: Provider requested a long wait.\n\n" + test.tail
			got := Classify(content, test.agent)
			if got.Kind != AttentionChat || len(got.Options) != 0 || got.Interaction != nil {
				t.Fatalf("classification = %+v, want chat without controls", got)
			}
		})
	}
}

func TestClassifyCapturedIdleAgentPromptsAsChat(t *testing.T) {
	tests := []struct {
		agent   string
		fixture string
	}{
		{agent: "claude", fixture: "claude-direct-real.ansi"},
		{agent: "codex", fixture: "codex-direct-real.ansi"},
		{agent: "omp", fixture: "omp-direct-real.ansi"},
		{agent: "pi", fixture: "pi-direct-real.ansi"},
		{agent: "qodercli", fixture: "qoder-direct-real.ansi"},
	}
	for _, test := range tests {
		t.Run(test.agent, func(t *testing.T) {
			path := filepath.Join("..", "copyresponse", "testdata", test.fixture)
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			got := Classify(string(content), test.agent)
			if got.Kind != AttentionChat || len(got.Options) != 0 || got.Interaction != nil {
				t.Fatalf("classification = %+v, want chat without controls", got)
			}
		})
	}
}

func TestClassifyHistoricalAndSupersededApprovalsSafely(t *testing.T) {
	historical := codexApprovalView + `
• The checks passed. What would you like to do next?
─ Worked for 4s ─
›
`
	if got := Classify(historical, "codex"); got.Kind != AttentionChat || len(got.Options) != 0 {
		t.Fatalf("historical approval classification = %+v, want chat", got)
	}
	if got := Classify(codexApprovalView+"\nNewer output is still arriving.", "codex"); got.Kind != AttentionUnknown {
		t.Fatalf("superseded approval classification = %+v, want unknown", got)
	}
}

func TestClassifyUnknownAgentsWithoutFabricatedControls(t *testing.T) {
	for _, content := range []string{
		codexApprovalView,
		codexQuestionView,
		"Hello! What would you like to work on next?\n❯",
		"1. First test\n2. Second test",
	} {
		got := Classify(content, "unparsed-agent")
		if got.Kind != AttentionUnknown || len(got.Options) != 0 || got.Interaction != nil {
			t.Fatalf("unknown-agent classification = %+v", got)
		}
	}
	_, _, options := ApprovalDetails("")
	if len(options) != 0 {
		t.Fatalf("empty pane fabricated approval options: %#v", options)
	}
}

func TestClassifyOpenCodeApprovalLikeLayoutsWithoutFabricatedControls(t *testing.T) {
	for _, content := range []string{
		codexApprovalView,
		claudeApprovalView,
		qoderApprovalView,
	} {
		got := Classify(content, "opencode")
		if got.Kind != AttentionUnknown || len(got.Options) != 0 || got.Interaction != nil {
			t.Fatalf("OpenCode approval-like classification = %+v", got)
		}
	}
}

func TestClassifyCapturedApprovalLayouts(t *testing.T) {
	tests := []struct {
		name    string
		agent   string
		fixture string
		options []string
		focus   int
	}{
		{
			name:    "codex plan approval",
			agent:   "codex",
			fixture: "codex-final-approval.ansi",
			options: []string{
				"Yes, implement this plan Switch to Default and start coding.",
				"Yes, clear context and implement Fresh thread. Context: 29% used.",
				"No, stay in Plan mode Continue planning with the model.",
			},
			focus: 2,
		},
		{
			name:    "codex implement plan approval",
			agent:   "codex",
			fixture: "codex-implement-plan.ansi",
			options: []string{
				"Yes, implement this plan Switch to Default and start coding.",
				"Yes, clear context and implement Fresh thread. Context: 10% used.",
				"No, stay in Plan mode Continue planning with the model.",
			},
			focus: 0,
		},
		{
			name:    "claude file approval",
			agent:   "claude",
			fixture: "claude-single-approval.ansi",
			options: []string{
				"Yes",
				"Yes, allow all edits during this session (shift+tab)",
				"No",
			},
			focus: 0,
		},
		{
			name:    "qoder plan approval",
			agent:   "qodercli",
			fixture: "qodercli-accept-reject-plan.ansi",
			options: []string{
				"Yes, start executing",
				"Yes, execute as Goal (auto)",
				"Refuse and say something",
				"Reject plan",
			},
			focus: 2,
		},
		{
			name:    "qoder permission with feedback",
			agent:   "qodercli",
			fixture: "qodercli-permission-required.ansi",
			options: []string{
				"Allow once",
				"Allow for this session [session]",
				"Modify with external editor",
				"Reject and type something",
				"No",
			},
			focus: 3,
		},
		{
			name:    "qoder permission",
			agent:   "qodercli",
			fixture: "qodercli-permission-required2.ansi",
			options: []string{
				"Allow once",
				"Allow for this session [session]",
				"Modify with external editor",
				"Reject and type something",
				"No",
			},
			focus: 2,
		},
		{
			name:    "qoder write permission",
			agent:   "qodercli",
			fixture: "qodercli-allow-deny.ansi",
			options: []string{
				"Allow once",
				"Allow for this session [session]",
				"Modify with external editor",
				"Reject and type something",
				"No",
			},
			focus: 0,
		},
		{
			name:    "omp tool approval",
			agent:   "omp",
			fixture: "omp-tool-approval.ansi",
			options: []string{
				"Approve",
				"Deny",
			},
			focus: 0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Classify(attentionFixture(t, test.fixture), test.agent)
			if got.Kind != AttentionApproval ||
				!reflect.DeepEqual(got.Options, test.options) ||
				got.ApprovalFocus != test.focus {
				t.Fatalf("classification = %+v", got)
			}
		})
	}
}

func TestClassifyCapturedQuestionLayouts(t *testing.T) {
	tests := []struct {
		name         string
		agent        string
		fixture      string
		kind         string
		question     string
		index        int
		total        int
		notesActive  bool
		notes        string
		submitLabel  string
		canGoBack    bool
		options      []string
		descriptions []string
	}{
		{
			name:        "codex question",
			agent:       "codex",
			fixture:     "codex-first-question.ansi",
			kind:        "single_select",
			question:    "What Everest trip are you planning?",
			index:       1,
			total:       3,
			submitLabel: "Next",
		},
		{
			name:        "codex question notes",
			agent:       "codex",
			fixture:     "codex-first-question_with_notes.ansi",
			kind:        "single_select",
			question:    "What Everest trip are you planning?",
			index:       1,
			total:       3,
			notesActive: true,
			notes:       "And then skiing down",
			submitLabel: "Next",
		},
		{
			name:        "codex final question",
			agent:       "codex",
			fixture:     "codex-middle-question.ansi",
			kind:        "single_select",
			question:    "What should be in the file initially?",
			index:       2,
			total:       2,
			submitLabel: "Submit",
			canGoBack:   true,
			options: []string{
				"Empty file (Recommended)",
				"One-line placeholder",
				"I will provide content",
			},
			descriptions: []string{
				"Create with zero bytes for a blank starting point.",
				"Create with a short default line and timestamp.",
				"Share exact content to write into the file.",
			},
		},
		{
			name:        "codex standalone question",
			agent:       "codex",
			fixture:     "codex-single-question.ansi",
			kind:        "single_select",
			question:    "Please confirm exact file spec now: path/name and initial content.",
			index:       1,
			total:       1,
			submitLabel: "Submit",
			options: []string{
				"Use default: /workspace/project/tmp/tmp_random_note.txt with empty content (Recommended)",
				"I’ll provide exact path and content",
			},
			descriptions: []string{
				"Proceed with a safe default so we can finalize the file-create plan immediately.",
				"Paste the exact filename and file content to use.",
			},
		},
		{
			name:        "qoder multi select",
			agent:       "qodercli",
			fixture:     "qodercli-first-question-multi-question.ansi",
			kind:        "multi_select",
			question:    "What kind of vibe are you looking for on this weekend trip?",
			index:       1,
			total:       4,
			submitLabel: "Next",
		},
		{
			name:        "qoder multi select notes",
			agent:       "qodercli",
			fixture:     "qodercli-multi-questions-and-notes.ansi",
			kind:        "multi_select",
			question:    "Who's coming along, and how will you get there?",
			index:       4,
			total:       4,
			notesActive: true,
			notes:       "I typed some notes here...",
			submitLabel: "Submit",
			canGoBack:   true,
		},
		{
			name:        "qoder single select",
			agent:       "qodercli",
			fixture:     "qodercli-second-question-single.ansi",
			kind:        "single_select",
			question:    "How far are you willing to travel from home?",
			index:       2,
			total:       4,
			submitLabel: "Next",
			canGoBack:   true,
		},
		{
			name:        "qoder review",
			agent:       "qodercli",
			fixture:     "qodercli-submit-answers.ansi",
			kind:        "single_select",
			question:    "Review your answers and choose what to do",
			index:       5,
			total:       5,
			submitLabel: "Continue",
			canGoBack:   true,
		},
		{
			name:        "qoder standalone question",
			agent:       "qodercli",
			fixture:     "qodercli-single-question.ansi",
			kind:        "single_select",
			question:    "What is your preferred color?",
			index:       1,
			total:       1,
			submitLabel: "Submit",
			options:     []string{"Blue", "Green", "Red"},
			descriptions: []string{
				"A calm, cool color often associated with trust and stability.",
				"A natural, fresh color associated with growth and balance.",
				"A bold, warm color associated with energy and passion.",
			},
		},
		{
			name:        "qoder standalone custom answer",
			agent:       "qodercli",
			fixture:     "qodercli-single-question-with-text.ansi",
			kind:        "single_select",
			question:    "What is your preferred color?",
			index:       1,
			total:       1,
			notesActive: true,
			notes:       "Something here",
			submitLabel: "Submit",
			options:     []string{"Blue", "Green", "Red"},
			descriptions: []string{
				"A calm, cool color often associated with trust and stability.",
				"A natural, fresh color associated with growth and balance.",
				"A bold, warm color associated with energy and passion.",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Classify(attentionFixture(t, test.fixture), test.agent)
			if got.Kind != AttentionQuestion || got.Interaction == nil {
				t.Fatalf("classification = %+v", got)
			}
			interaction := got.Interaction
			if interaction.Kind != test.kind ||
				interaction.Question != test.question ||
				interaction.QuestionIndex != test.index ||
				interaction.QuestionTotal != test.total ||
				interaction.NotesActive != test.notesActive ||
				interaction.Other.Text != test.notes ||
				interaction.SubmitLabel != test.submitLabel ||
				interaction.CanGoBack != test.canGoBack {
				t.Fatalf("interaction = %+v", interaction)
			}
			if test.options != nil {
				var labels, descriptions []string
				for _, option := range interaction.Options {
					labels = append(labels, option.Label)
					descriptions = append(descriptions, option.Description)
				}
				if !reflect.DeepEqual(labels, test.options) ||
					!reflect.DeepEqual(descriptions, test.descriptions) {
					t.Fatalf("options = %#v, descriptions = %#v", labels, descriptions)
				}
			}
		})
	}
}

func TestClassifyCapturedOpenCodeQuestionLayouts(t *testing.T) {
	tests := []struct {
		name          string
		fixture       string
		kind          string
		question      string
		index         int
		total         int
		options       []string
		selected      []bool
		otherSelected bool
		otherText     string
		notesActive   bool
		description   string
	}{
		{
			name:     "standalone",
			fixture:  "opencode-single-question.ansi",
			kind:     "single_select",
			question: "What would you like to work on today?",
			index:    1,
			total:    1,
			options:  []string{"Write some code", "Debug an issue", "Explore the codebase", "Refactor code"},
			selected: []bool{false, false, false, false},
		},
		{
			name:     "multi-step single select",
			fixture:  "opencode-many-questions.ansi",
			kind:     "single_select",
			question: "Do you need a backend?",
			index:    3,
			total:    3,
			options: []string{
				"No backend",
				"Node.js + Express",
				"Python + FastAPI",
				"Next.js API routes",
				"Supabase/Firebase",
			},
			selected: []bool{false, false, false, false, false},
		},
		{
			name:        "multi-step review",
			fixture:     "opencode-many-questions-confirm.ansi",
			kind:        "single_select",
			question:    "Review your answers and choose what to do",
			index:       4,
			total:       4,
			options:     []string{"Submit answers"},
			selected:    []bool{false},
			description: "1. App type: Social platform\n2. Frontend: Vue 3\n3. Backend: Python + FastAPI",
		},
		{
			name:     "multi select",
			fixture:  "opencode-questions-with-multiple-choice-answers.ansi",
			kind:     "multi_select",
			question: "What's your travel style and what are you looking for? (select all that apply)",
			index:    1,
			total:    4,
			options: []string{
				"City breaks and culture",
				"Nature and outdoors",
				"Food and wine",
				"History and landmarks",
				"Relaxation and scenery",
			},
			selected: []bool{false, true, true, false, true},
		},
		{
			name:          "single custom editor",
			fixture:       "opencode-questions-with-free-text.ansi",
			kind:          "single_select",
			question:      "Who's traveling?",
			index:         4,
			total:         4,
			options:       []string{"Solo", "Couple", "Small group (3-5)", "Family with kids"},
			selected:      []bool{false, false, true, false},
			otherText:     "And maybe with friends too!!",
			notesActive:   true,
			otherSelected: false,
		},
		{
			name:        "multi custom editor empty",
			fixture:     "opencode-questions-multiple-choice-with-free-text-before-text.ansi",
			kind:        "multi_select",
			question:    "What's your travel style and what are you looking for? (select all that apply)",
			index:       1,
			total:       4,
			selected:    []bool{false, true, true, false, true},
			notesActive: true,
		},
		{
			name:        "multi custom editor text",
			fixture:     "opencode-questions-multiple-choice-with-free-text-edited.ansi",
			kind:        "multi_select",
			question:    "What's your travel style and what are you looking for? (select all that apply)",
			index:       1,
			total:       4,
			selected:    []bool{false, true, true, false, true},
			otherText:   "That's great!!!",
			notesActive: true,
		},
		{
			name:          "multi custom accepted",
			fixture:       "opencode-questions-multiple-choice-with-free-text.ansi",
			kind:          "multi_select",
			question:      "What's your travel style and what are you looking for? (select all that apply)",
			index:         1,
			total:         4,
			selected:      []bool{false, true, true, false, true},
			otherSelected: true,
			otherText:     "That's great",
		},
		{
			name:        "multi-select review",
			fixture:     "opencode-questions-with-multiple-choice-answers-confirm.ansi",
			kind:        "single_select",
			question:    "Review your answers and choose what to do",
			index:       5,
			total:       5,
			options:     []string{"Submit answers"},
			selected:    []bool{false},
			description: "1. Trip vibe: Nature and outdoors, Food and wine, Relaxation and scenery\n2. Budget: €800-1200\n3. Timing: Next weekend\n4. Company: Small group (3-5)",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := attentionFixture(t, test.fixture)
			if !LayoutHint(content) {
				lines := strings.Split(content, "\n")
				var cleaned []string
				for _, line := range lines[len(lines)-min(8, len(lines)):] {
					cleaned = append(cleaned, cleanOpenCodeLine(line))
				}
				t.Fatalf("layout hint missing; cleaned tail = %#v", cleaned)
			}
			got := Classify(content, "opencode")
			if got.Kind != AttentionQuestion || got.Interaction == nil || len(got.Options) != 0 {
				t.Fatalf("classification = %+v", got)
			}
			interaction := got.Interaction
			var labels []string
			var selected []bool
			for _, option := range interaction.Options {
				labels = append(labels, option.Label)
				selected = append(selected, option.Selected)
			}
			if interaction.Kind != test.kind ||
				interaction.Question != test.question ||
				interaction.QuestionIndex != test.index ||
				interaction.QuestionTotal != test.total ||
				test.options != nil && !reflect.DeepEqual(labels, test.options) ||
				!reflect.DeepEqual(selected, test.selected) ||
				interaction.Other.Selected != test.otherSelected ||
				interaction.Other.Text != test.otherText ||
				interaction.NotesActive != test.notesActive {
				t.Fatalf("interaction = %+v; labels = %#v; selected = %#v", interaction, labels, selected)
			}
			if test.description != "" &&
				interaction.Options[0].Description != test.description {
				t.Fatalf("description = %q", interaction.Options[0].Description)
			}
		})
	}
}

func TestClassifyCapturedQoderSettingsDialogsSafely(t *testing.T) {
	for _, fixture := range []string{
		"qodercli-add-directory.ansi",
		"qodercli-approval.ansi",
		"qodercli-select-option.ansi",
		"qodercli-yes-no.ansi",
	} {
		got := Classify(attentionFixture(t, fixture), "qodercli")
		if got.Kind != AttentionUnknown || len(got.Options) != 0 || got.Interaction != nil {
			t.Fatalf("%s classification = %+v, want terminal-only unknown", fixture, got)
		}
	}
}

func TestClassifyCapturedClaudeQuestionLayouts(t *testing.T) {
	tests := []struct {
		name             string
		fixture          string
		kind             string
		question         string
		index            int
		total            int
		submitLabel      string
		canGoBack        bool
		options          []string
		selected         []bool
		otherSelected    bool
		otherText        string
		otherHidden      bool
		firstDescription string
	}{
		{
			name:        "standalone single select",
			fixture:     "claude-single-question.ansi",
			kind:        "single_select",
			question:    "What kind of file would you like me to create?",
			submitLabel: "Submit",
			options: []string{
				"Text/Markdown note",
				"Code file",
				"Config/data file",
				"Something else",
			},
			selected: []bool{false, false, false, false},
		},
		{
			name:        "plan single select",
			fixture:     "claude-plan-one-question.ansi",
			kind:        "single_select",
			question:    "Which part of the Alps are you thinking of, or where are you starting from?",
			submitLabel: "Submit",
			options: []string{
				"French Alps",
				"Swiss Alps",
				"Italian Alps",
				"Not sure / open to suggestions",
			},
			selected: []bool{false, false, false, false},
		},
		{
			name:          "plan custom answer",
			fixture:       "claude-plan-one-question-notes.ansi",
			kind:          "single_select",
			question:      "Which part of the Alps are you thinking of, or where are you starting from?",
			submitLabel:   "Submit",
			options:       []string{"French Alps", "Swiss Alps", "Italian Alps", "Not sure / open to suggestions"},
			selected:      []bool{false, false, false, false},
			otherSelected: true,
			otherText:     "I don't know",
		},
		{
			name:        "later plan question",
			fixture:     "claude-plan-multi-question.ansi",
			kind:        "single_select",
			question:    "Do you need a database?",
			index:       4,
			total:       5,
			submitLabel: "Next",
			canGoBack:   true,
			options:     []string{"Yes, SQL", "Yes, NoSQL", "No database needed", "Not sure"},
			selected:    []bool{false, false, true, false},
		},
		{
			name:        "plan review",
			fixture:     "claude-plan-submit-answers.ansi",
			kind:        "single_select",
			question:    "Review your answers and choose what to do",
			index:       5,
			total:       5,
			submitLabel: "Continue",
			canGoBack:   true,
			options:     []string{"Submit answers", "Cancel"},
			selected:    []bool{false, false},
			otherHidden: true,
			firstDescription: "1. What kind of webapp are you building: Content/marketing site\n" +
				"2. Which frontend approach do you prefer: Vue\n" +
				"3. Do you need a backend/server, and if so what language: No backend needed\n" +
				"4. Do you need a database: No database needed",
		},
		{
			name:        "multi select",
			fixture:     "claude-multi-select.ansi",
			kind:        "multi_select",
			question:    "Which activities would you like to include on your week-end trip?",
			index:       1,
			total:       2,
			submitLabel: "Next",
			options:     []string{"Hiking", "Sightseeing", "Relaxation", "Food & drink"},
			selected:    []bool{false, true, true, false},
		},
		{
			name:          "multi select custom answer",
			fixture:       "claude-multi-select-with-free-text.ansi",
			kind:          "multi_select",
			question:      "Which activities would you like to include on your week-end trip?",
			index:         1,
			total:         2,
			submitLabel:   "Next",
			options:       []string{"Hiking", "Sightseeing", "Relaxation", "Food & drink"},
			selected:      []bool{false, true, true, false},
			otherSelected: true,
			otherText:     "Sport haha!!",
		},
		{
			name:        "multi select review",
			fixture:     "claude-multi-select-submit-end.ansi",
			kind:        "single_select",
			question:    "Review your answers and choose what to do",
			index:       2,
			total:       2,
			submitLabel: "Continue",
			canGoBack:   true,
			options:     []string{"Submit answers", "Cancel"},
			selected:    []bool{false, false},
			otherHidden: true,
			firstDescription: "1. Which activities would you like to include on your week-end trip: " +
				"Sightseeing, Relaxation, Sport haha!!",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Classify(attentionFixture(t, test.fixture), "claude")
			if got.Kind != AttentionQuestion || got.Interaction == nil || len(got.Options) != 0 {
				t.Fatalf("classification = %+v", got)
			}
			interaction := got.Interaction
			var labels []string
			var selected []bool
			for _, option := range interaction.Options {
				labels = append(labels, option.Label)
				selected = append(selected, option.Selected)
			}
			if interaction.Kind != test.kind ||
				interaction.Question != test.question ||
				interaction.QuestionIndex != test.index ||
				interaction.QuestionTotal != test.total ||
				interaction.SubmitLabel != test.submitLabel ||
				interaction.CanGoBack != test.canGoBack ||
				!reflect.DeepEqual(labels, test.options) ||
				!reflect.DeepEqual(selected, test.selected) ||
				interaction.Other.Selected != test.otherSelected ||
				interaction.Other.Text != test.otherText ||
				interaction.Other.Hidden != test.otherHidden {
				t.Fatalf("interaction = %+v", interaction)
			}
			if test.firstDescription != "" &&
				interaction.Options[0].Description != test.firstDescription {
				t.Fatalf("description = %q", interaction.Options[0].Description)
			}
		})
	}
}

func TestCapturedQoderApprovalFollowedByNewPromptIsChat(t *testing.T) {
	content := attentionFixture(t, "qodercli-permission-required2.ansi") +
		"\n\n Thinking\n │ The request was handled.\n ▪ Done.\n\n >\n"
	got := Classify(content, "qodercli")
	if got.Kind != AttentionChat || len(got.Options) != 0 || got.Interaction != nil {
		t.Fatalf("classification = %+v", got)
	}
}

func TestCapturedClaudeControlsFollowedByNewPromptAreChat(t *testing.T) {
	for _, fixture := range []string{
		"claude-single-approval.ansi",
		"claude-plan-submit-answers.ansi",
	} {
		content := attentionFixture(t, fixture) +
			"\n\n⏺ The request was handled.\n✻ Worked for 1s\n\n❯\n"
		got := Classify(content, "claude")
		if got.Kind != AttentionChat || len(got.Options) != 0 || got.Interaction != nil {
			t.Fatalf("%s classification = %+v", fixture, got)
		}
	}
}

func TestCapturedQoderApprovalFollowedByIndentedOutputIsUnknown(t *testing.T) {
	content := attentionFixture(t, "qodercli-permission-required2.ansi") +
		"\n  Newer output is still arriving.\n"
	got := Classify(content, "qodercli")
	if got.Kind != AttentionUnknown || len(got.Options) != 0 || got.Interaction != nil {
		t.Fatalf("classification = %+v", got)
	}
}

func TestClassifyCapturedOMPPlanApproval(t *testing.T) {
	got := Classify(attentionFixture(t, "omp-plan-approval.ansi"), "omp")
	want := []string{
		"Approve and execute",
		"Approve and compact context",
		"Approve and keep context (~18k / 272k)",
		"Refine plan",
	}
	if got.Kind != AttentionApproval ||
		!reflect.DeepEqual(got.Options, want) ||
		got.ApprovalFocus != 0 {
		t.Fatalf("classification = %+v", got)
	}
}

func TestClassifyCapturedOMPPartialAskQuestion(t *testing.T) {
	got := Classify(attentionFixture(t, "omp-partial-ask.ansi"), "omp")
	if got.Kind != AttentionQuestion || got.Interaction == nil {
		t.Fatalf("classification = %+v", got)
	}
	interaction := got.Interaction
	var labels []string
	for _, option := range interaction.Options {
		labels = append(labels, option.Label)
	}
	want := []string{
		"Beach & coast", "Mountains & nature", "City break",
		"Quiet lakeside cabin (Recommended)",
	}
	if interaction.Options[0].Description != "Relax by the water — swimming, seafood, sunset walks." {
		t.Fatalf("description = %q", interaction.Options[0].Description)
	}
	if interaction.Kind != "single_select" ||
		interaction.Question != "What kind of weekend trip are you in the mood for?" ||
		!reflect.DeepEqual(labels, want) ||
		!interaction.Options[3].Selected ||
		interaction.Focus != (Focus{Kind: "option", Index: 3}) ||
		!interaction.Other.Hidden ||
		interaction.QuestionIndex != 1 ||
		interaction.QuestionTotal != 4 ||
		interaction.SubmitLabel != "Next" ||
		interaction.CanGoBack ||
		interaction.AllOptionCount != 4 {
		t.Fatalf("interaction = %+v", interaction)
	}
}

func TestClassifyCapturedClaudeStaleNote(t *testing.T) {
	got := Classify(attentionFixture(t, "claude-stale-note.ansi"), "claude")
	if got.Kind != AttentionQuestion || got.Interaction == nil {
		t.Fatalf("classification = %+v", got)
	}
	interaction := got.Interaction
	if interaction.Kind != "single_select" ||
		!interaction.Options[2].Selected ||
		interaction.Options[2].Label != "Most of the weekend" ||
		interaction.Other.Selected ||
		interaction.Other.Text != "Hhrr" {
		t.Fatalf("interaction = %+v", interaction)
	}
}

func TestClassifyCapturedOMPNoteQuestion(t *testing.T) {
	got := Classify(attentionFixture(t, "omp-note-question.ansi"), "omp")
	if got.Kind != AttentionQuestion || got.Interaction == nil {
		t.Fatalf("classification = %+v", got)
	}
	interaction := got.Interaction
	if interaction.Kind != "single_select" ||
		interaction.Question != "Who's going, and when?" ||
		len(interaction.Options) != 4 ||
		interaction.Other.Label != "Other (type your own)" ||
		!interaction.Other.Selected ||
		interaction.Other.Text != "GhhTyy" ||
		interaction.QuestionIndex != 4 ||
		interaction.QuestionTotal != 4 {
		t.Fatalf("interaction = %+v", interaction)
	}
}

func TestClassifyCapturedClaudeWrappedReview(t *testing.T) {
	got := Classify(attentionFixture(t, "claude-wrapped-review.ansi"), "claude")
	if got.Kind != AttentionQuestion || got.Interaction == nil {
		t.Fatalf("classification = %+v", got)
	}
	interaction := got.Interaction
	want := "1. There's a big pile of uncommitted changes already in the repo (approval.go, " +
		"dispatch.go, question parser, TerminalView, agents.ts, etc.) — is " +
		"finishing/shipping that work part of the weekend plan: custom answer\n" +
		"2. What's the main type of work you want to tackle this weekend: " +
		"Bug fixes / polish, Testing & stability, Refactor / cleanup\n" +
		"3. Roughly how much time do you want to put into this over the weekend: custom answer\n" +
		"4. If you had to name ONE thing you want done or working by Monday, what is it? (free text): " +
		"No single goal — flexible weekend"
	if interaction.Question != "Review your answers and choose what to do" ||
		interaction.Options[0].Description != want {
		t.Fatalf("description = %q", interaction.Options[0].Description)
	}
	entries := interaction.Options[0].Summary
	if len(entries) != 4 ||
		entries[2].Question != "Roughly how much time do you want to put into this over the weekend" ||
		entries[2].Answer != "custom answer" {
		t.Fatalf("summary = %+v", entries)
	}
}

func TestClassifyCapturedClaudeWrappedQuestion(t *testing.T) {
	got := Classify(attentionFixture(t, "claude-wrapped-question.ansi"), "claude")
	if got.Kind != AttentionQuestion || got.Interaction == nil {
		t.Fatalf("classification = %+v", got)
	}
	interaction := got.Interaction
	want := "There's a big pile of uncommitted changes already in the repo (approval.go, " +
		"dispatch.go, question parser, TerminalView, agents.ts, etc.) — is " +
		"finishing/shipping that work part of the weekend plan?"
	if interaction.Question != want ||
		interaction.Other.Text != "we" ||
		!interaction.Other.Selected {
		t.Fatalf("interaction = %+v", interaction)
	}
}

func TestClassifyCapturedCodexFollowUpQuestion(t *testing.T) {
	got := Classify(attentionFixture(t, "codex-follow-up-question.ansi"), "codex")
	if got.Kind != AttentionQuestion || got.Interaction == nil {
		t.Fatalf("classification = %+v", got)
	}
	interaction := got.Interaction
	if interaction.Question != "Was the custom note preserved?" ||
		len(interaction.Options) != 2 ||
		interaction.Options[0].Label != "Yes" ||
		interaction.Other.Label != "None of the above" ||
		interaction.QuestionIndex != 1 ||
		interaction.QuestionTotal != 1 {
		t.Fatalf("interaction = %+v", interaction)
	}
}

func TestFillCustomAnswersReplacesPlaceholders(t *testing.T) {
	interaction := &Interaction{
		Options: []Option{{
			Label: "Submit answers",
			Summary: []SummaryEntry{
				{Question: "Roughly how much time do you want to put into this over the weekend", Answer: CustomAnswerPlaceholder},
				{Question: "Pick a lane", Answer: "Bug fixes"},
				{Question: "Name one outcome", Answer: CustomAnswerPlaceholder},
			},
		}},
	}
	FillCustomAnswers(interaction, map[string]string{
		SummaryKey("Roughly how much time do you want to put into this over the weekend?"): "Tyy",
	})
	entries := interaction.Options[0].Summary
	want := "1. Roughly how much time do you want to put into this over the weekend: Tyy\n" +
		"2. Pick a lane: Bug fixes\n" +
		"3. Name one outcome: custom answer"
	if entries[0].Answer != "Tyy" ||
		entries[1].Answer != "Bug fixes" ||
		entries[2].Answer != CustomAnswerPlaceholder ||
		interaction.Options[0].Description != want {
		t.Fatalf("interaction = %+v", interaction.Options[0])
	}
}

func attentionFixture(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata", "attention", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
