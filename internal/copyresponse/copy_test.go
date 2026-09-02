package copyresponse

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/slashcmd"
)

type fakePane struct {
	snapshots  []string
	reads      int
	texts      []string
	keys       [][]string
	onSendKeys func()
}

func (p *fakePane) ReadPane(ctx context.Context, _ string, _ int, _ string) (herdr.PaneRead, error) {
	if err := ctx.Err(); err != nil {
		return herdr.PaneRead{}, err
	}
	index := p.reads
	p.reads++
	if index >= len(p.snapshots) {
		index = len(p.snapshots) - 1
	}
	if index < 0 {
		return herdr.PaneRead{}, errors.New("no pane snapshots")
	}
	return herdr.PaneRead{Content: []byte(p.snapshots[index])}, nil
}

func (p *fakePane) SendText(ctx context.Context, _ string, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.texts = append(p.texts, text)
	return nil
}

func (p *fakePane) SendKeys(ctx context.Context, _ string, keys []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.keys = append(p.keys, append([]string(nil), keys...))
	if p.onSendKeys != nil {
		p.onSendKeys()
	}
	return nil
}

func TestRunCopiesConfirmedResponseAndRestoresClipboard(t *testing.T) {
	pane := &fakePane{snapshots: []string{
		"❯ ",
		"Copied to clipboard (17 characters, 2 lines)",
	}}
	profile, ok := slashcmd.CopyProfileFor("claude", "")
	if !ok {
		t.Fatal("missing Claude copy profile")
	}
	reads := 0
	var writes [][]byte
	result, err := Run(
		context.Background(),
		"pane-1",
		profile,
		pane,
		func(context.Context) ([]byte, error) {
			reads++
			if reads == 1 {
				return []byte("before"), nil
			}
			return []byte("first line\nsecond"), nil
		},
		func(_ context.Context, data []byte) error {
			writes = append(writes, append([]byte(nil), data...))
			return nil
		},
		1,
		func(context.Context, string) (int64, error) { return 1, nil },
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result != (Result{Text: "first line\nsecond", Source: "clipboard", Chars: 17, Lines: 2}) {
		t.Fatalf("Run() result = %+v", result)
	}
	if !reflect.DeepEqual(pane.texts, []string{"/copy"}) {
		t.Fatalf("sent text = %v, want /copy", pane.texts)
	}
	if len(writes) != 2 || string(writes[1]) != "before" || string(writes[0]) == "before" {
		t.Fatalf("clipboard writes = %q, want sentinel then original", writes)
	}
}
func TestRunContinuesWhenInitialClipboardReadFails(t *testing.T) {
	response := []byte("first line\nsecond")
	pane := &fakePane{snapshots: []string{
		"❯ ",
		"Copied to clipboard (17 characters, 2 lines)",
	}}
	profile, ok := slashcmd.CopyProfileFor("claude", "")
	if !ok {
		t.Fatal("missing Claude copy profile")
	}
	reads := 0
	var writes [][]byte
	result, err := Run(
		context.Background(),
		"pane-empty-clipboard",
		profile,
		pane,
		func(context.Context) ([]byte, error) {
			reads++
			if reads == 1 {
				return nil, errors.New("clipboard is empty")
			}
			return response, nil
		},
		func(_ context.Context, data []byte) error {
			writes = append(writes, append([]byte(nil), data...))
			return nil
		},
		1,
		nil,
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Text != string(response) || result.Source != "clipboard" {
		t.Fatalf("Run() result = %+v, want copied response", result)
	}
	if len(writes) != 2 || len(writes[1]) != 0 {
		t.Fatalf("clipboard writes = %q, want sentinel then empty restore", writes)
	}
}

func TestRunBoundsClipboardRestore(t *testing.T) {
	pane := &fakePane{snapshots: []string{
		"❯ ",
		"Copied to clipboard (17 characters, 2 lines)",
	}}
	profile, ok := slashcmd.CopyProfileFor("claude", "")
	if !ok {
		t.Fatal("missing Claude copy profile")
	}
	reads := 0
	writes := 0
	restoreHasDeadline := false
	started := time.Now()
	_, err := Run(
		context.Background(),
		"pane-1",
		profile,
		pane,
		func(context.Context) ([]byte, error) {
			reads++
			if reads == 1 {
				return []byte("before"), nil
			}
			return []byte("first line\nsecond"), nil
		},
		func(ctx context.Context, _ []byte) error {
			writes++
			if writes == 1 {
				return nil
			}
			_, restoreHasDeadline = ctx.Deadline()
			<-ctx.Done()
			return ctx.Err()
		},
		1,
		func(context.Context, string) (int64, error) { return 2, nil },
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want bounded restore deadline", err)
	}
	if !restoreHasDeadline {
		t.Fatal("clipboard restore context has no deadline")
	}
	if writes != 2 {
		t.Fatalf("clipboard writes = %d, want sentinel and bounded restore", writes)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("clipboard restore took %s, want bounded cleanup", elapsed)
	}
}

func TestRunPollsPastStaleConfirmationAfterRepeat(t *testing.T) {
	confirmation := "Copied to clipboard (17 characters, 2 lines)"
	response := []byte("123456789\n1234567")
	pane := &fakePane{snapshots: []string{
		confirmation + "\n❯ ",
		"❯ /copy",
		confirmation + "\n❯ ",
		confirmation + "\n❯ ",
		"Agent output\n" + confirmation + "\n" + confirmation + "\n❯ ",
	}}
	profile, ok := slashcmd.CopyProfileFor("claude", "")
	if !ok {
		t.Fatal("missing Claude copy profile")
	}
	clipboard := []byte("before")
	var sentinel []byte
	var clipboardReads [][]byte
	writes := 0
	result, err := Run(
		context.Background(),
		"pane-1",
		profile,
		pane,
		func(context.Context) ([]byte, error) {
			if len(clipboardReads) == 3 {
				clipboard = append([]byte(nil), response...)
			}
			value := append([]byte(nil), clipboard...)
			clipboardReads = append(clipboardReads, value)
			return value, nil
		},
		func(_ context.Context, data []byte) error {
			writes++
			clipboard = append([]byte(nil), data...)
			if writes == 1 {
				sentinel = append([]byte(nil), data...)
			}
			return nil
		},
		1,
		func(context.Context, string) (int64, error) { return 2, nil },
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Text != string(response) || result.Chars != 17 || result.Lines != 2 {
		t.Fatalf("Run() result = %+v, want second identical response", result)
	}
	if len(clipboardReads) != 5 || string(sentinel) == string(response) || writes != 2 {
		t.Fatalf("clipboard exchange = reads %q, sentinel %q, writes %d", clipboardReads, sentinel, writes)
	}
	if !reflect.DeepEqual(pane.keys, [][]string{{"Enter"}}) {
		t.Fatalf("keys = %v, want one composer submission", pane.keys)
	}
}

func TestRunAcceptsUncountedConfirmationAfterScroll(t *testing.T) {
	response := []byte("same response")
	pane := &fakePane{snapshots: []string{
		"• Copied last message to clipboard\n› ",
		"› /copy",
		"• Copied last message to clipboard\n› ",
	}}
	profile, ok := slashcmd.CopyProfileFor("codex", "")
	if !ok {
		t.Fatal("missing Codex copy profile")
	}
	writes := 0
	result, err := Run(
		context.Background(),
		"pane-1",
		profile,
		pane,
		func(context.Context) ([]byte, error) {
			if writes > 0 {
				return response, nil
			}
			return []byte("before"), nil
		},
		func(_ context.Context, _ []byte) error {
			writes++
			return nil
		},
		1,
		nil,
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Text != string(response) || result.Chars != len(response) || result.Lines != 1 {
		t.Fatalf("Run() result = %+v, want copied response", result)
	}
	if !reflect.DeepEqual(pane.keys, [][]string{{"Enter"}}) {
		t.Fatalf("keys = %v, want one composer submission", pane.keys)
	}
}

func TestRunAcceptsDifferentConfirmationAfterRepeat(t *testing.T) {
	oldConfirmation := "Copied to clipboard (17 characters, 2 lines)"
	newConfirmation := "Copied to clipboard (23 characters, 3 lines)"
	response := []byte("123456789\n123456789\n123")
	pane := &fakePane{snapshots: []string{
		oldConfirmation + "\n❯ ",
		"❯ /copy",
		"Agent output\n" + oldConfirmation + "\n" + newConfirmation + "\n❯ ",
	}}
	profile, ok := slashcmd.CopyProfileFor("claude", "")
	if !ok {
		t.Fatal("missing Claude copy profile")
	}
	clipboard := []byte("before")
	reads := 0
	writes := 0
	result, err := Run(
		context.Background(),
		"pane-1",
		profile,
		pane,
		func(context.Context) ([]byte, error) {
			reads++
			if reads == 3 {
				clipboard = append([]byte(nil), response...)
			}
			return append([]byte(nil), clipboard...), nil
		},
		func(_ context.Context, data []byte) error {
			writes++
			clipboard = append([]byte(nil), data...)
			return nil
		},
		1,
		func(context.Context, string) (int64, error) { return 2, nil },
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Text != string(response) || result.Chars != 23 || result.Lines != 3 {
		t.Fatalf("Run() result = %+v, want latest different response", result)
	}
	if reads != 4 || writes != 2 || string(clipboard) != "before" {
		t.Fatalf("clipboard exchange = reads %d, writes %d, final %q", reads, writes, clipboard)
	}
}

func TestRunRejectsUnchangedPreviousConfirmationAfterRepeat(t *testing.T) {
	confirmation := "Copied to clipboard (17 characters, 2 lines)"
	pane := &fakePane{snapshots: []string{
		confirmation + "\n❯ ",
		"❯ /copy",
		"Agent output\n" + confirmation + "\n❯ ",
	}}
	profile, ok := slashcmd.CopyProfileFor("claude", "")
	if !ok {
		t.Fatal("missing Claude copy profile")
	}
	clipboard := []byte("before")
	var sentinel []byte
	var clipboardReads [][]byte
	var beforeRestore []byte
	writes := 0
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err := Run(
		ctx,
		"pane-1",
		profile,
		pane,
		func(context.Context) ([]byte, error) {
			value := append([]byte(nil), clipboard...)
			clipboardReads = append(clipboardReads, value)
			return value, nil
		},
		func(_ context.Context, data []byte) error {
			writes++
			if writes == 2 {
				beforeRestore = append([]byte(nil), clipboard...)
			}
			clipboard = append([]byte(nil), data...)
			if writes == 1 {
				sentinel = append([]byte(nil), data...)
			}
			return nil
		},
		1,
		nil,
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want context deadline after stale confirmation", err)
	}
	if len(clipboardReads) != 2 || string(beforeRestore) != string(sentinel) || string(clipboard) != "before" {
		t.Fatalf("clipboard state = reads %q, before restore %q, sentinel %q, final %q", clipboardReads, beforeRestore, sentinel, clipboard)
	}
	if !reflect.DeepEqual(pane.keys, [][]string{{"Enter"}, {"Escape"}}) {
		t.Fatalf("keys = %v, want submission followed by recovery", pane.keys)
	}
}

func TestRunRejectsRegressedPaneRevisionAndRestoresClipboard(t *testing.T) {
	pane := &fakePane{snapshots: []string{
		"❯ ",
		"Copied to clipboard (5 characters, 1 line)",
		"❯ ",
	}}
	profile, _ := slashcmd.CopyProfileFor("claude", "")
	reads := 0
	var writes [][]byte
	_, err := Run(
		context.Background(),
		"pane-1",
		profile,
		pane,
		func(context.Context) ([]byte, error) {
			reads++
			if reads == 1 {
				return []byte("before"), nil
			}
			return []byte("hello"), nil
		},
		func(_ context.Context, data []byte) error {
			writes = append(writes, append([]byte(nil), data...))
			return nil
		},
		7,
		func(context.Context, string) (int64, error) { return 6, nil },
	)
	if !errors.Is(err, ErrStaleOutput) {
		t.Fatalf("Run() error = %v, want ErrStaleOutput", err)
	}
	if len(pane.keys) == 0 || !reflect.DeepEqual(pane.keys[0], []string{"Escape"}) {
		t.Fatalf("escape keys = %v, want Escape recovery", pane.keys)
	}
	if len(writes) != 2 || string(writes[1]) != "before" {
		t.Fatalf("clipboard writes = %q, want restored original", writes)
	}
}
func TestRunPreservesBusyComposer(t *testing.T) {
	response := []byte("Codex response")
	pane := &fakePane{snapshots: []string{
		"› Summarize recent commits",
		"› /copy",
		"Copied last message to clipboard\n› ",
	}}
	profile, ok := slashcmd.CopyProfileFor("codex", "")
	if !ok {
		t.Fatal("missing Codex copy profile")
	}
	reads := 0
	var writes [][]byte
	result, err := Run(
		context.Background(),
		"pane-codex-prompt",
		profile,
		pane,
		func(context.Context) ([]byte, error) {
			reads++
			if reads == 1 {
				return []byte("before"), nil
			}
			return response, nil
		},
		func(_ context.Context, data []byte) error {
			writes = append(writes, append([]byte(nil), data...))
			return nil
		},
		1,
		nil,
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Text != string(response) || result.Source != "clipboard" {
		t.Fatalf("Run() result = %+v, want copied response", result)
	}
	if !reflect.DeepEqual(pane.texts, []string{"/copy", "Summarize recent commits"}) {
		t.Fatalf("sent text = %v, want copy command and restored prompt", pane.texts)
	}
	if !reflect.DeepEqual(pane.keys, [][]string{{"Escape"}, {"Enter"}}) {
		t.Fatalf("sent keys = %v, want composer clear and command submission", pane.keys)
	}
	if len(writes) != 2 || string(writes[1]) != "before" {
		t.Fatalf("clipboard writes = %q, want sentinel then original", writes)
	}
}

func TestRunEscapesAnAbortedPostSubmissionPicker(t *testing.T) {
	pane := &fakePane{snapshots: []string{
		"❯ ",
		"PICKER",
	}}
	profile, _ := slashcmd.CopyProfileFor("claude", "")
	profile.PostSubmission = regexp.MustCompile(`(?m)^PICKER$`)
	var writes [][]byte
	_, err := Run(
		context.Background(),
		"pane-1",
		profile,
		pane,
		func(context.Context) ([]byte, error) { return []byte("before"), nil },
		func(_ context.Context, data []byte) error {
			writes = append(writes, append([]byte(nil), data...))
			return nil
		},
		1,
		nil,
	)
	if !errors.Is(err, ErrNoCopy) {
		t.Fatalf("Run() error = %v, want ErrNoCopy", err)
	}
	if !reflect.DeepEqual(pane.keys, [][]string{{"Enter"}, {"Escape"}, {"Escape"}}) {
		t.Fatalf("keys = %v, want one accept and bounded Escape recovery", pane.keys)
	}
	if len(writes) != 2 || string(writes[1]) != "before" {
		t.Fatalf("clipboard writes = %q, want restored original", writes)
	}
}

func TestRunUsesVerifiedSecondaryPathWhenClipboardCountsMismatch(t *testing.T) {
	secondaryPath := filepath.Join(t.TempDir(), "response.md")
	secondaryResponse := []byte("secondary\ntext")
	profile, _ := slashcmd.CopyProfileFor("qoder", "")
	profile.SecondaryPath = secondaryPath
	pane := &fakePane{snapshots: []string{
		" > ",
		"Copied to clipboard (14 characters, 2 lines)",
	}}
	reads := 0
	var writeErr error
	result, err := Run(
		context.Background(),
		"pane-1",
		profile,
		pane,
		func(context.Context) ([]byte, error) {
			reads++
			if reads == 1 {
				return []byte("before"), nil
			}
			return []byte("different"), nil
		},
		func(context.Context, []byte) error {
			writeErr = os.WriteFile(secondaryPath, secondaryResponse, 0o600)
			return writeErr
		},
		1,
		func(context.Context, string) (int64, error) { return 2, nil },
	)
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Source != "secondary_file" || result.Text != string(secondaryResponse) {
		t.Fatalf("Run() result = %+v, want secondary file", result)
	}
}

func TestRunRejectsUnchangedSecondaryFallback(t *testing.T) {
	secondaryPath := filepath.Join(t.TempDir(), "response.md")
	secondaryResponse := []byte("secondary\ntext")
	if err := os.WriteFile(secondaryPath, secondaryResponse, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(secondaryPath, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	profile, _ := slashcmd.CopyProfileFor("qoder", "")
	profile.SecondaryPath = secondaryPath
	pane := &fakePane{snapshots: []string{
		" > ",
		"Copied to clipboard (14 characters, 2 lines)",
	}}
	reads := 0
	_, err := Run(
		context.Background(),
		"pane-1",
		profile,
		pane,
		func(context.Context) ([]byte, error) {
			reads++
			switch reads {
			case 1:
				return []byte("before"), nil
			case 2:
				return secondaryResponse, nil
			default:
				return []byte("different"), nil
			}
		},
		func(context.Context, []byte) error { return nil },
		1,
		func(context.Context, string) (int64, error) { return 2, nil },
	)
	if !errors.Is(err, ErrNoCopy) {
		t.Fatalf("Run() error = %v, want unchanged secondary response rejected", err)
	}
}

func TestRunMatchesConfirmedCharacterCountsByRune(t *testing.T) {
	response := []byte("naïve —")
	pane := &fakePane{snapshots: []string{
		"❯ ",
		"Copied to clipboard (7 characters, 1 line)",
	}}
	profile, ok := slashcmd.CopyProfileFor("claude", "")
	if !ok {
		t.Fatal("missing Claude copy profile")
	}
	reads := 0
	result, err := Run(
		context.Background(),
		"pane-runes",
		profile,
		pane,
		func(context.Context) ([]byte, error) {
			reads++
			if reads == 1 {
				return []byte("before"), nil
			}
			return response, nil
		},
		func(context.Context, []byte) error { return nil },
		1,
		nil,
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result != (Result{Text: string(response), Source: "clipboard", Chars: 7, Lines: 1}) {
		t.Fatalf("Run() result = %+v, want rune-counted response", result)
	}
}

func TestRunAcceptsUncountedAgentConfirmation(t *testing.T) {
	response := []byte("Codex response")
	pane := &fakePane{snapshots: []string{
		"› Use /skills to list available skills",
		"• Copied last message to clipboard",
	}}
	profile, ok := slashcmd.CopyProfileFor("codex", "")
	if !ok {
		t.Fatal("missing Codex copy profile")
	}
	reads := 0
	result, err := Run(
		context.Background(),
		"pane-codex",
		profile,
		pane,
		func(context.Context) ([]byte, error) {
			reads++
			if reads == 1 {
				return []byte("before"), nil
			}
			return response, nil
		},
		func(context.Context, []byte) error { return nil },
		1,
		nil,
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Text != string(response) || result.Chars != len(response) || result.Lines != 1 {
		t.Fatalf("Run() result = %+v, want uncounted confirmation response", result)
	}
}

func TestRunPiRejectsUnverifiedIdleLayoutBeforeInjection(t *testing.T) {
	profile, ok := slashcmd.CopyProfileFor("pi", "")
	if !ok {
		t.Fatal("missing Pi copy profile")
	}
	pane := &fakePane{snapshots: []string{"Copied last agent message to clipboard"}}
	clipboardReads := 0
	_, err := Run(
		context.Background(),
		"pane-pi",
		profile,
		pane,
		func(context.Context) ([]byte, error) {
			clipboardReads++
			return []byte("before"), nil
		},
		func(context.Context, []byte) error { return nil },
		1,
		nil,
	)
	if !errors.Is(err, ErrComposerBusy) {
		t.Fatalf("Run() error = %v, want ErrComposerBusy", err)
	}
	if len(pane.texts) != 0 || clipboardReads != 0 {
		t.Fatalf("Run() injected before Pi idle verification: texts=%v clipboard reads=%d", pane.texts, clipboardReads)
	}
}

func TestRunPiAcceptsRecordedIdleLayout(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("testdata", "pi-direct-real.ansi"))
	if err != nil {
		t.Fatal(err)
	}
	const confirmation = "Copied last agent message to clipboard"
	initial := strings.Replace(string(content), confirmation, "", 1)
	response := []byte("Pi response")
	pane := &fakePane{snapshots: []string{initial, string(content)}}
	profile, ok := slashcmd.CopyProfileFor("pi", "")
	if !ok {
		t.Fatal("missing Pi copy profile")
	}
	reads := 0
	result, err := Run(
		context.Background(),
		"pane-pi",
		profile,
		pane,
		func(context.Context) ([]byte, error) {
			reads++
			if reads == 1 {
				return []byte("before"), nil
			}
			return response, nil
		},
		func(context.Context, []byte) error { return nil },
		1,
		nil,
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Text != string(response) || result.Source != "clipboard" {
		t.Fatalf("Run() result = %+v, want recorded Pi clipboard response", result)
	}
	if !reflect.DeepEqual(pane.texts, []string{"/copy"}) {
		t.Fatalf("sent text = %v, want /copy", pane.texts)
	}
}

func TestRunAcceptsKimiCharacterOnlyConfirmation(t *testing.T) {
	response := []byte("naïve —")
	pane := &fakePane{snapshots: []string{
		"│ > │",
		"Copied to clipboard (7 characters).",
	}}
	profile, ok := slashcmd.CopyProfileFor("kimi", "")
	if !ok {
		t.Fatal("missing Kimi copy profile")
	}
	reads := 0
	result, err := Run(
		context.Background(),
		"pane-kimi",
		profile,
		pane,
		func(context.Context) ([]byte, error) {
			reads++
			if reads == 1 {
				return []byte("before"), nil
			}
			return response, nil
		},
		func(context.Context, []byte) error { return nil },
		1,
		nil,
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result != (Result{Text: string(response), Source: "clipboard", Chars: 7, Lines: 1}) {
		t.Fatalf("Run() result = %+v, want Kimi response", result)
	}
}

func TestRunUsesRecordedQoderMenuStates(t *testing.T) {
	readCapture := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	direct := readCapture("qoder-direct-real.ansi")
	menu := readCapture("qoder-copy-menu-real.ansi")
	confirmation := "● Copied to clipboard (801 characters, 9 lines)"
	menu = strings.ReplaceAll(menu, confirmation, "")
	pane := &fakePane{snapshots: []string{
		direct,
		direct + "\n" + menu,
		direct + "\n > /copy\n",
		direct + "\n > /copy\n" + confirmation + "\n > ",
	}}
	profile, ok := slashcmd.CopyProfileFor("qoder", "")
	if !ok {
		t.Fatal("missing Qoder copy profile")
	}
	profile.SecondaryPath = filepath.Join(t.TempDir(), "response.md")
	response := []byte(strings.Repeat("a", 793) + strings.Repeat("\n", 8))
	reads := 0
	result, err := Run(
		context.Background(),
		"pane-qoder",
		profile,
		pane,
		func(context.Context) ([]byte, error) {
			reads++
			if reads == 1 {
				return []byte("before"), nil
			}
			return response, nil
		},
		func(context.Context, []byte) error { return nil },
		1,
		nil,
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result != (Result{Text: string(response), Source: "clipboard", Chars: 801, Lines: 9}) {
		t.Fatalf("Run() result = %+v, want recorded Qoder orchestration result", result)
	}
	if !reflect.DeepEqual(pane.keys, [][]string{{"Enter"}, {"Enter"}}) {
		t.Fatalf("keys = %v, want Qoder menu acceptance and submission", pane.keys)
	}
}

func TestRunUsesRecordedOmpPicker(t *testing.T) {
	readCapture := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	direct := readCapture("omp-direct-real.ansi")
	picker := readCapture("omp-picker-real.ansi")
	confirmation := "Copied last message to clipboard"
	pane := &fakePane{snapshots: []string{
		direct,
		direct + "\n" + picker,
		direct + "\n" + confirmation,
	}}
	profile, ok := slashcmd.CopyProfileFor("omp", "")
	if !ok {
		t.Fatal("missing OMP copy profile")
	}
	response := []byte("OMP markdown response")
	reads := 0
	result, err := Run(
		context.Background(),
		"pane-omp",
		profile,
		pane,
		func(context.Context) ([]byte, error) {
			reads++
			if reads == 1 {
				return []byte("before"), nil
			}
			return response, nil
		},
		func(context.Context, []byte) error { return nil },
		1,
		func(context.Context, string) (int64, error) { return 1, nil },
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result != (Result{Text: string(response), Source: "clipboard", Chars: len(response), Lines: 1}) {
		t.Fatalf("Run() result = %+v, want recorded OMP orchestration result", result)
	}
	if !reflect.DeepEqual(pane.keys, [][]string{{"Enter"}}) {
		t.Fatalf("keys = %v, want OMP picker acceptance", pane.keys)
	}
}

func TestRunUsesRecordedOmp020PickerAndConfirmation(t *testing.T) {
	readCapture := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	idle := readCapture("omp-idle-0.20.ansi")
	// The palette replaces the empty composer line the moment /copy is typed.
	palette := strings.Replace(
		idle,
		"\n────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────\n                                                                                                                                                       \n",
		"\n────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────\n /copy\n",
		1,
	) + "\n  copy        Pick text or code from the conversation to copy"
	if palette == idle+"\n  copy        Pick text or code from the conversation to copy" {
		t.Fatal("idle capture no longer contains the empty composer between two rules")
	}
	pane := &fakePane{snapshots: []string{
		idle,
		palette,
		readCapture("omp-picker-0.20.ansi"),
		readCapture("omp-confirmation-0.20.ansi"),
	}}
	profile, ok := slashcmd.CopyProfileFor("omp", "")
	if !ok {
		t.Fatal("missing OMP copy profile")
	}
	response := []byte("OMP 0.20 markdown response")
	reads := 0
	result, err := Run(
		context.Background(),
		"pane-omp",
		profile,
		pane,
		func(context.Context) ([]byte, error) {
			reads++
			if reads == 1 {
				return []byte("before"), nil
			}
			return response, nil
		},
		func(context.Context, []byte) error { return nil },
		1,
		func(context.Context, string) (int64, error) { return 1, nil },
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result != (Result{Text: string(response), Source: "clipboard", Chars: len(response), Lines: 1}) {
		t.Fatalf("Run() result = %+v, want recorded OMP 0.20 orchestration result", result)
	}
	if !reflect.DeepEqual(pane.keys, [][]string{{"Enter"}, {"Enter"}}) {
		t.Fatalf("keys = %v, want palette then picker acceptance", pane.keys)
	}
}

func TestRunAcceptsOmpPickerBeforeConfirmation(t *testing.T) {
	response := []byte("OMP response")
	pane := &fakePane{snapshots: []string{
		"────────────────────\n\n────────────────────",
		"────────────────────\n /copy\n────────────────────\n  copy        Pick text or code from the conversation to copy",
		"╭─ Copy to clipboard ─╮\n│ ❯ response text │\n╰─ ↑↓ move · Enter copy · Esc/Ctrl+C quit ─╯",
		"Copied last message to clipboard\n────────────────────\n\n────────────────────",
	}}
	profile, ok := slashcmd.CopyProfileFor("omp", "")
	if !ok {
		t.Fatal("missing OMP copy profile")
	}
	reads := 0
	result, err := Run(
		context.Background(),
		"pane-omp",
		profile,
		pane,
		func(context.Context) ([]byte, error) {
			reads++
			if reads == 1 {
				return []byte("before"), nil
			}
			return response, nil
		},
		func(context.Context, []byte) error { return nil },
		1,
		nil,
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !reflect.DeepEqual(pane.keys, [][]string{{"Enter"}, {"Enter"}}) {
		t.Fatalf("keys = %v, want command and picker acceptance", pane.keys)
	}
	if result.Text != string(response) || result.Chars != len(response) || result.Lines != 1 {
		t.Fatalf("Run() result = %+v, want OMP response", result)
	}
}

func TestRunUsesQoderRuneCountForSecondaryResponse(t *testing.T) {
	response := []byte("naïve —")
	secondaryPath := filepath.Join(t.TempDir(), "response.md")
	if err := os.WriteFile(secondaryPath, response, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(secondaryPath, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	var writeErr error
	pane := &fakePane{
		snapshots: []string{
			" >   Type your message or @path/to/file",
			" > /copy\n ❯ copy  Copy the last assistant response to clipboard",
			" > /copy\n ● Copied to clipboard (7 characters, 1 line)\n >   Type your message or @path/to/file",
		},
		onSendKeys: func() {
			writeErr = os.WriteFile(secondaryPath, response, 0o600)
		},
	}
	profile, ok := slashcmd.CopyProfileFor("qoder", "")
	if !ok {
		t.Fatal("missing Qoder copy profile")
	}
	profile.SecondaryPath = secondaryPath
	reads := 0
	result, err := Run(
		context.Background(),
		"pane-qoder",
		profile,
		pane,
		func(context.Context) ([]byte, error) {
			reads++
			if reads == 1 {
				return []byte("before"), nil
			}
			return []byte("stale clipboard"), nil
		},
		func(context.Context, []byte) error {
			return nil
		},
		1,
		nil,
	)
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result != (Result{Text: string(response), Source: "secondary_file", Chars: 7, Lines: 1}) {
		t.Fatalf("Run() result = %+v, want Qoder rune-counted secondary response", result)
	}
	if !reflect.DeepEqual(pane.keys, [][]string{{"Enter"}}) {
		t.Fatalf("keys = %v, want one Qoder menu acceptance", pane.keys)
	}
}

func TestRunAcceptsUncountedRepeatAfterConfirmationScrollsOut(t *testing.T) {
	response := []byte("Codex response")
	confirmation := "Copied last message to clipboard"
	scrollingOutput := strings.Repeat("scrolling output\n", 130)
	pane := &fakePane{snapshots: []string{
		confirmation + "\n› ",
		"› /copy",
		scrollingOutput + confirmation + "\n› ",
	}}
	profile, ok := slashcmd.CopyProfileFor("codex", "")
	if !ok {
		t.Fatal("missing Codex copy profile")
	}
	reads := 0
	result, err := Run(
		context.Background(),
		"pane-codex-repeat",
		profile,
		pane,
		func(context.Context) ([]byte, error) {
			reads++
			if reads == 1 {
				return []byte("before"), nil
			}
			return response, nil
		},
		func(context.Context, []byte) error { return nil },
		1,
		nil,
	)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result != (Result{Text: string(response), Source: "clipboard", Chars: len(response), Lines: 1}) {
		t.Fatalf("Run() result = %+v, want repeated uncounted response", result)
	}
	if !reflect.DeepEqual(pane.keys, [][]string{{"Enter"}}) {
		t.Fatalf("keys = %v, want one command submission", pane.keys)
	}
}
