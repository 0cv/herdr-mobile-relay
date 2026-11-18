package copyresponse

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
	"github.com/0cv/herdr-mobile-relay/internal/slashcmd"
)

const (
	paneReadLines           = 120
	copyTimeoutMS           = 2_000
	confirmationPollPeriod  = 10 * time.Millisecond
	clipboardPollPeriod     = 50 * time.Millisecond
	recoveryTimeout         = 500 * time.Millisecond
	clipboardRestoreTimeout = 500 * time.Millisecond
	maxEnterPresses         = 3
)

var (
	ErrComposerBusy = errors.New("agent composer is busy")
	ErrPickerOpen   = errors.New("agent copy picker is already open")
	ErrStaleOutput  = errors.New("copy confirmation is stale")
	ErrNoCopy       = errors.New("agent did not confirm a copied response")
)

type Pane interface {
	ReadPane(context.Context, string, int, string) (herdr.PaneRead, error)
	SendText(context.Context, string, string) error
	SendKeys(context.Context, string, []string) error
}

type Result struct {
	Text   string
	Source string
	Chars  int
	Lines  int
}

type RevisionReader func(context.Context, string) (int64, error)
type ClipboardReader func(context.Context) ([]byte, error)
type ClipboardWriter func(context.Context, []byte) error

func Run(
	ctx context.Context,
	paneID string,
	profile slashcmd.CopyProfile,
	pane Pane,
	readClipboard ClipboardReader,
	writeClipboard ClipboardWriter,
	initialRevision int64,
	currentRevision RevisionReader,
) (result Result, err error) {
	if paneID == "" {
		return Result{}, errors.New("pane is required")
	}
	if pane == nil || readClipboard == nil || writeClipboard == nil {
		return Result{}, errors.New("copy response dependencies are unavailable")
	}
	if profile.Confirmation == nil || profile.Composer == nil {
		return Result{}, errors.New("agent copy profile is unavailable")
	}

	initialSnapshot, err := readPane(ctx, pane, paneID)
	if err != nil {
		return Result{}, fmt.Errorf("read pane before copy: %w", err)
	}
	initialState := profile.CleanSnapshot(initialSnapshot)
	if profile.MenuOpen(initialState) || profile.PickerOpen(initialState) {
		return Result{}, ErrPickerOpen
	}
	// Preserve a pending prompt instead of rejecting the copy or appending /copy
	// to it. The prompt is restored after the response-copy transaction finishes.
	composer, foundComposer := profile.ComposerText(initialState)
	composerNeedsRestore := foundComposer && composer != ""
	if profile.IdleLayout != nil && !profile.IdleLayout.MatchString(initialState) {
		return Result{}, ErrComposerBusy
	}
	if !composerNeedsRestore && !profile.ComposerReady(initialState) {
		return Result{}, ErrComposerBusy
	}
	copied, err := newCopiedResponse(readClipboard, profile.SecondaryPath)
	if err != nil {
		return Result{}, err
	}

	originalClipboard, _ := readClipboard(ctx)
	if err := writeClipboard(ctx, []byte(copied.sentinel)); err != nil {
		return Result{}, fmt.Errorf("write clipboard sentinel: %w", err)
	}
	defer func() {
		restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), clipboardRestoreTimeout)
		defer cancel()
		if restoreErr := writeClipboard(restoreCtx, originalClipboard); restoreErr != nil {
			if err == nil {
				result = Result{}
				err = fmt.Errorf("restore clipboard: %w", restoreErr)
				return
			}
			err = errors.Join(err, fmt.Errorf("restore clipboard: %w", restoreErr))
		}
	}()
	var composerCleared bool
	if composerNeedsRestore {
		clearKeys := profile.ComposerClearKeys
		if len(clearKeys) == 0 {
			clearKeys = []string{"Escape"}
		}
		if err := pane.SendKeys(ctx, paneID, clearKeys); err != nil {
			return Result{}, fmt.Errorf("clear agent composer: %w", err)
		}
		composerCleared = true
		defer func() {
			if !composerCleared {
				return
			}
			if restoreErr := restoreComposerIfEmpty(ctx, pane, paneID, profile, composer); restoreErr != nil {
				if err == nil {
					result = Result{}
					err = fmt.Errorf("restore agent composer: %w", restoreErr)
					return
				}
				err = errors.Join(err, fmt.Errorf("restore agent composer: %w", restoreErr))
			}
		}()
		if len(profile.ComposerClearKeys) > 0 {
			clearedSnapshot, err := readPane(ctx, pane, paneID)
			if err != nil {
				return Result{}, fmt.Errorf("verify agent composer clear: %w", err)
			}
			clearedText, found := profile.ComposerText(clearedSnapshot)
			if !found || clearedText != "" {
				return Result{}, errors.New("verify agent composer clear: composer remains non-empty")
			}
		}
	}

	submitted := false
	abort := func(cause error) (Result, error) {
		if submitted {
			recoverPane(ctx, pane, paneID, profile)
		}
		return Result{}, cause
	}
	if err := pane.SendText(ctx, paneID, "/copy"); err != nil {
		return abort(fmt.Errorf("send copy command: %w", err))
	}
	submitted = true

	confirmation, err := waitForFreshConfirmation(ctx, pane, paneID, profile, initialState, copied)
	if err != nil {
		return abort(err)
	}
	confirmedChars, confirmedLines, ok := profile.ConfirmationCounts(string(confirmation))
	if !ok {
		return abort(ErrNoCopy)
	}
	if currentRevision != nil {
		revision, revisionErr := currentRevision(ctx, paneID)
		if revisionErr != nil {
			return abort(fmt.Errorf("check copy revision: %w", revisionErr))
		}
		// Herdr revisions are not guaranteed to advance for terminal-only updates
		// (Claude keeps a stable revision while its rendered pane changes). A
		// regression still proves that the snapshot came from an older pane.
		if revision < initialRevision {
			return abort(ErrStaleOutput)
		}
	}

	clipboard, readErr := readClipboard(ctx)
	if readErr == nil && copied.isFreshClipboard(clipboard, confirmedChars, confirmedLines) {
		return Result{Text: string(clipboard), Source: "clipboard", Chars: utf8.RuneCount(clipboard), Lines: lineCount(clipboard)}, nil
	}
	if secondary, ok := copied.freshSecondaryResponse(confirmedChars, confirmedLines); ok {
		return Result{Text: string(secondary), Source: "secondary_file", Chars: utf8.RuneCount(secondary), Lines: lineCount(secondary)}, nil
	}
	if readErr != nil {
		return abort(fmt.Errorf("read copied response: %w", readErr))
	}
	return abort(ErrNoCopy)
}

func waitForFreshConfirmation(
	ctx context.Context,
	pane Pane,
	paneID string,
	profile slashcmd.CopyProfile,
	baseline string,
	copied copiedResponse,
) ([]byte, error) {
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(copyTimeoutMS)*time.Millisecond)
	defer cancel()
	var nextClipboardCheck time.Time
	clipboardReady := func(state string) bool {
		now := time.Now()
		if now.Before(nextClipboardCheck) {
			return false
		}
		nextClipboardCheck = now.Add(clipboardPollPeriod)
		return copied.ready(waitCtx, profile, state)
	}
	enters := 0
	lastAction := ""
	previousState := profile.CleanSnapshot(baseline)
	confirmationAfterAction := false
	timeoutErr := func() error {
		if waitCtx.Err() == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrNoCopy
	}
	waitNext := func() error {
		timer := time.NewTimer(confirmationPollPeriod)
		defer timer.Stop()
		select {
		case <-waitCtx.Done():
			return timeoutErr()
		case <-timer.C:
			return nil
		}
	}
	for {
		if err := timeoutErr(); err != nil {
			return nil, err
		}
		snapshot, err := readPane(waitCtx, pane, paneID)
		if err != nil {
			return nil, fmt.Errorf("read pane after copy confirmation: %w", err)
		}
		state := profile.CleanSnapshot(snapshot)
		confirmationAppeared := confirmationAppearedInDelta(profile, previousState, state)
		delta, _ := snapshotDelta(profile.CleanSnapshot(baseline), state)
		action := ""
		switch {
		case profile.MenuOpen(delta):
			action = "accept copy command"
		case profile.PickerOpen(delta):
			action = "accept copied response"
		default:
			if composer, found := profile.ComposerText(delta); found && composer == "/copy" {
				action = "submit copy command"
			}
			chars, lines, matched := profile.ConfirmationCounts(state)
			if action == "" {
				if confirmationAfterAction && matched && chars < 0 && lines < 0 && clipboardReady(state) {
					return []byte(state), nil
				}
				if fresh, ok := freshConfirmation(profile, baseline, state); ok && clipboardReady(state) {
					return []byte(fresh), nil
				}
				if confirmationAppeared && matched &&
					!profile.MenuOpen(state) && !profile.PickerOpen(state) {
					composer, found := profile.ComposerText(state)
					if confirmationAfterAction || !found || composer != "/copy" {
						if clipboardReady(state) && chars < 0 && lines < 0 {
							return []byte(state), nil
						}
					}
				}
			}
		}
		switch {
		case action == "":
			lastAction = ""
		case action != lastAction:
			if enters >= maxEnterPresses {
				return nil, ErrNoCopy
			}
			if err := pane.SendKeys(waitCtx, paneID, []string{"Enter"}); err != nil {
				return nil, fmt.Errorf("%s: %w", action, err)
			}
			enters++
			confirmationAfterAction = true
			lastAction = action
		}
		previousState = state
		if err := waitNext(); err != nil {
			return nil, err
		}
	}
}

type copiedResponse struct {
	readClipboard       ClipboardReader
	sentinel            string
	secondaryPath       string
	secondaryBefore     []byte
	secondaryBeforeInfo os.FileInfo
	secondaryExisted    bool
}

func newCopiedResponse(readClipboard ClipboardReader, secondaryPath string) (copiedResponse, error) {
	sentinel, err := newSentinel()
	if err != nil {
		return copiedResponse{}, fmt.Errorf("create clipboard sentinel: %w", err)
	}
	copied := copiedResponse{readClipboard: readClipboard, sentinel: sentinel, secondaryPath: secondaryPath}
	if secondaryPath == "" {
		return copied, nil
	}
	info, statErr := os.Stat(secondaryPath)
	if statErr != nil {
		return copied, nil
	}
	data, readErr := os.ReadFile(secondaryPath)
	if readErr != nil {
		return copied, nil
	}
	copied.secondaryBefore = data
	copied.secondaryBeforeInfo = info
	copied.secondaryExisted = true
	return copied, nil
}

func (c copiedResponse) isFreshClipboard(data []byte, chars, lines int) bool {
	return string(data) != c.sentinel && matchesCounts(data, chars, lines)
}

func (c copiedResponse) ready(ctx context.Context, profile slashcmd.CopyProfile, state string) bool {
	chars, lines, ok := profile.ConfirmationCounts(state)
	if !ok {
		return false
	}
	clipboard, err := c.readClipboard(ctx)
	if err == nil && c.isFreshClipboard(clipboard, chars, lines) {
		return true
	}
	_, ok = c.freshSecondaryResponse(chars, lines)
	return ok
}

func (c copiedResponse) freshSecondaryResponse(chars, lines int) ([]byte, bool) {
	if c.secondaryPath == "" {
		return nil, false
	}
	secondary, err := os.ReadFile(c.secondaryPath)
	secondaryInfo, statErr := os.Stat(c.secondaryPath)
	if err != nil || statErr != nil ||
		string(secondary) == c.sentinel || !matchesCounts(secondary, chars, lines) {
		return nil, false
	}
	if !c.secondaryExisted || !bytes.Equal(secondary, c.secondaryBefore) {
		return secondary, true
	}
	if c.secondaryBeforeInfo == nil ||
		(os.SameFile(c.secondaryBeforeInfo, secondaryInfo) && c.secondaryBeforeInfo.ModTime().Equal(secondaryInfo.ModTime())) {
		return nil, false
	}
	return secondary, true
}

func freshConfirmation(profile slashcmd.CopyProfile, baseline, current string) (string, bool) {
	baselineState := profile.CleanSnapshot(baseline)
	currentState := profile.CleanSnapshot(current)
	currentChars, currentLines, hasCurrent := profile.ConfirmationCounts(currentState)
	if !hasCurrent {
		return "", false
	}
	baselineChars, baselineLines, hasBaseline := profile.ConfirmationCounts(baselineState)
	if !hasBaseline ||
		(currentChars >= 0 && baselineChars >= 0 && currentChars != baselineChars) ||
		(currentLines >= 0 && baselineLines >= 0 && currentLines != baselineLines) ||
		confirmationCount(profile, currentState) > confirmationCount(profile, baselineState) {
		return currentState, true
	}
	return "", false
}
func confirmationAppearedInDelta(profile slashcmd.CopyProfile, before, after string) bool {
	delta, _ := snapshotDelta(profile.CleanSnapshot(before), profile.CleanSnapshot(after))
	_, _, ok := profile.ConfirmationCounts(delta)
	return ok
}

func confirmationCount(profile slashcmd.CopyProfile, content string) int {
	if profile.Confirmation == nil {
		return 0
	}
	return len(profile.Confirmation.FindAllStringIndex(content, -1))
}

func snapshotDelta(before, after string) (string, bool) {
	before = strings.ReplaceAll(before, "\r\n", "\n")
	after = strings.ReplaceAll(after, "\r\n", "\n")
	if before == "" {
		return after, true
	}
	beforeLines := strings.Split(before, "\n")
	afterLines := strings.Split(after, "\n")
	maxOverlap := len(beforeLines)
	if len(afterLines) < maxOverlap {
		maxOverlap = len(afterLines)
	}
	for overlap := maxOverlap; overlap > 0; overlap-- {
		offset := len(beforeLines) - overlap
		matches := true
		for index := 0; index < overlap; index++ {
			if beforeLines[offset+index] != afterLines[index] {
				matches = false
				break
			}
		}
		if matches {
			return strings.Join(afterLines[overlap:], "\n"), true
		}
	}
	return after, false
}

func readPane(ctx context.Context, pane Pane, paneID string) (string, error) {
	read, err := pane.ReadPane(ctx, paneID, paneReadLines, "ansi")
	if err != nil {
		return "", err
	}
	return string(read.Content), nil
}

func restoreComposerIfEmpty(ctx context.Context, pane Pane, paneID string, profile slashcmd.CopyProfile, composer string) error {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recoveryTimeout)
	defer cancel()
	snapshot, err := readPane(recoveryCtx, pane, paneID)
	if err != nil {
		return err
	}
	current, found := profile.ComposerText(snapshot)
	if !found || current != "" {
		return nil
	}
	return pane.SendText(recoveryCtx, paneID, composer)
}

func recoverPane(ctx context.Context, pane Pane, paneID string, profile slashcmd.CopyProfile) {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recoveryTimeout)
	defer cancel()
	for range 2 {
		if err := pane.SendKeys(recoveryCtx, paneID, []string{"Escape"}); err != nil {
			return
		}
		snapshot, err := readPane(recoveryCtx, pane, paneID)
		if err != nil {
			continue
		}
		composerIdle := profile.ComposerReady(snapshot)
		if composerIdle && !profile.MenuOpen(snapshot) && !profile.PickerOpen(snapshot) {
			return
		}
	}
}

func newSentinel() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return "herdr-mobile-relay-copy-" + hex.EncodeToString(data), nil
}

// Negative counts mean the agent omitted that dimension; sentinel and freshness still guard the payload.
func matchesCounts(data []byte, chars, lines int) bool {
	if chars >= 0 && utf8.RuneCount(data) != chars {
		return false
	}
	if lines >= 0 && lineCount(data) != lines {
		return false
	}
	return true
}

func lineCount(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	lines := 1
	for _, value := range data {
		if value == '\n' {
			lines++
		}
	}
	return lines
}
