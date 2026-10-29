//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJournalIntegration(t *testing.T) {
	if os.Getenv("HERDR_TEST_JOURNAL") != "1" {
		t.Skip("set HERDR_TEST_JOURNAL=1 to test the user journal")
	}

	for _, command := range []string{"systemd-run", "systemctl", "journalctl"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Fatalf("journal integration prerequisite %q is unavailable: %v", command, err)
		}
	}
	if output, err := runJournalCommand(5*time.Second, "systemctl", "--user", "show-environment"); err != nil {
		t.Fatalf("journal integration requires a reachable user manager: %v (%s)", err, output)
	}

	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	testBinary, err = filepath.Abs(testBinary)
	if err != nil {
		t.Fatal(err)
	}

	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			marker := fmt.Sprintf("herdr-journal-%d-%s", time.Now().UnixNano(), format)
			unit := "herdr-relay-logtest-" + strings.TrimPrefix(marker, "herdr-journal-")
			entries := runJournalFixture(t, testBinary, unit, marker, format, "debug", false)
			assertFixtureRecords(t, entries, marker, format, true)
			warningEntries := journalEntries(t, unit, "-p", "warning")
			assertMarkedPriorities(t, warningEntries, marker, map[string]string{
				marker + " warn":  "4",
				marker + " error": "3",
			})

			infoMarker := marker + "-info"
			infoUnit := unit + "-info"
			infoEntries := runJournalFixture(t, testBinary, infoUnit, infoMarker, format, "info", false)
			assertFixtureRecords(t, infoEntries, infoMarker, format, false)

			errorMarker := marker + "-serve-error"
			errorUnit := unit + "-serve-error"
			errorEntries := runJournalFixture(t, testBinary, errorUnit, errorMarker, format, "error", true)
			assertMarkedPriorities(t, errorEntries, errorMarker, map[string]string{
				errorMarker + " serve-error": "3",
			})
		})
	}
}

func TestJournalLoggingFixture(t *testing.T) {
	if os.Getenv("HERDR_JOURNAL_FIXTURE") != "1" {
		return
	}

	level := slog.LevelInfo
	if os.Getenv("HERDR_RELAY_LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	marker := os.Getenv("HERDR_JOURNAL_FIXTURE_MARKER")
	logger := newRelayLogger(os.Stderr, os.Getenv("HERDR_RELAY_LOG_FORMAT"), level, stderrIsJournal(os.Stderr))
	if os.Getenv("HERDR_JOURNAL_FIXTURE_SERVE_ERROR") == "1" {
		reportError(os.Stderr, []string{"serve"}, fmt.Errorf("%s serve-error", marker))
		return
	}
	logger.Debug(marker+" debug", "fixture", true)
	logger.Info(marker+" info", "fixture", true)
	logger.Warn(marker+" warn", "fixture", true)
	logger.Error(marker+" error", "fixture", true)
}

type journalEntry struct {
	Message  string
	Priority string
}

func runJournalFixture(t *testing.T, testBinary, unit, marker, format, level string, serveError bool) []journalEntry {
	t.Helper()
	cleanupJournalUnit(t, unit)
	t.Cleanup(func() { cleanupJournalUnit(t, unit) })

	arguments := []string{
		"--user",
		"--wait",
		"--collect",
		"--unit=" + unit,
		"--property=StandardOutput=journal",
		"--property=StandardError=journal",
		"--setenv=HERDR_JOURNAL_FIXTURE=1",
		"--setenv=HERDR_JOURNAL_FIXTURE_MARKER=" + marker,
		"--setenv=HERDR_RELAY_LOG_FORMAT=" + format,
		"--setenv=HERDR_RELAY_LOG_LEVEL=" + level,
	}
	if serveError {
		arguments = append(arguments, "--setenv=HERDR_JOURNAL_FIXTURE_SERVE_ERROR=1")
	}
	arguments = append(arguments, "--", testBinary, "-test.run=^TestJournalLoggingFixture$", "-test.v")

	output, err := runJournalCommand(30*time.Second, "systemd-run", arguments...)
	if err != nil {
		t.Fatalf("systemd-run for %s failed: %v (%s)", unit, err, output)
	}

	wantMarked := 3
	if level == "debug" {
		wantMarked = 4
	}
	if serveError {
		wantMarked = 1
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries := journalEntries(t, unit)
		marked := markedEntries(entries, marker)
		if len(marked) >= wantMarked || time.Now().After(deadline) {
			if len(marked) < wantMarked {
				t.Logf("journal fixture incomplete: unit=%s format=%s level=%s serveError=%t wantMarked=%d marked=%d systemd-run-output=%q journal-entries=%s",
					unit, format, level, serveError, wantMarked, len(marked), boundedJournalOutput(output), journalEntriesDiagnostic(entries))
			}
			return entries
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func journalEntries(t *testing.T, unit string, extra ...string) []journalEntry {
	t.Helper()
	arguments := []string{"--user", "-u", unit, "--no-pager", "-o", "json"}
	arguments = append(arguments, extra...)
	output, err := runJournalCommand(5*time.Second, "journalctl", arguments...)
	if err != nil {
		t.Fatalf("journalctl for %s failed: %v (%s)", unit, err, output)
	}

	var entries []journalEntry
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("journalctl returned invalid JSON: %v; line=%q", err, line)
		}
		message, _ := raw["MESSAGE"].(string)
		priority := fmt.Sprint(raw["PRIORITY"])
		entries = append(entries, journalEntry{Message: message, Priority: priority})
	}
	return entries
}

func assertFixtureRecords(t *testing.T, entries []journalEntry, marker, format string, includeDebug bool) {
	t.Helper()
	want := map[string]string{
		marker + " info":  "6",
		marker + " warn":  "4",
		marker + " error": "3",
	}
	if includeDebug {
		want[marker+" debug"] = "7"
	}
	assertMarkedPriorities(t, entries, marker, want)
	for _, entry := range markedEntries(entries, marker) {
		if strings.HasPrefix(entry.Message, "<") {
			t.Errorf("journal MESSAGE retained priority prefix: %q", entry.Message)
		}
		if format == "json" {
			var body map[string]any
			if err := json.Unmarshal([]byte(entry.Message), &body); err != nil {
				t.Errorf("journal MESSAGE is not JSON: %v; message=%q", err, entry.Message)
			}
		}
	}
}

func assertMarkedPriorities(t *testing.T, entries []journalEntry, marker string, want map[string]string) {
	t.Helper()
	marked := markedEntries(entries, marker)
	if len(marked) != len(want) {
		t.Errorf("marked journal entries = %d, want %d: %v", len(marked), len(want), marked)
	}
	got := make(map[string]string)
	for _, entry := range marked {
		for message := range want {
			if strings.Contains(entry.Message, message) {
				if _, exists := got[message]; exists {
					t.Errorf("journal record %q occurred more than once", message)
				}
				got[message] = entry.Priority
			}
		}
	}
	for message, priority := range want {
		if got[message] != priority {
			t.Errorf("journal priority for %q = %q, want %q (entries=%v)", message, got[message], priority, markedEntries(entries, marker))
		}
	}
}

func markedEntries(entries []journalEntry, marker string) []journalEntry {
	marked := make([]journalEntry, 0, len(entries))
	for _, entry := range entries {
		if strings.Contains(entry.Message, marker) {
			marked = append(marked, entry)
		}
	}
	return marked
}

func cleanupJournalUnit(t *testing.T, unit string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "systemctl", "--user", "stop", unit)
	_ = command.Run()
	if ctx.Err() != nil {
		t.Logf("timed out stopping owned transient unit %s", unit)
		return
	}
	reset := exec.CommandContext(ctx, "systemctl", "--user", "reset-failed", unit)
	_ = reset.Run()
	if ctx.Err() != nil {
		t.Logf("timed out resetting owned transient unit %s", unit)
	}
}

func runJournalCommand(timeout time.Duration, name string, arguments ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, name, arguments...)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		return output, ctx.Err()
	}
	return output, err
}

func boundedJournalOutput(output []byte) string {
	const maxBytes = 4096
	if len(output) > maxBytes {
		return string(output[:maxBytes]) + "...[truncated]"
	}
	return string(output)
}

func journalEntriesDiagnostic(entries []journalEntry) string {
	output, err := json.Marshal(entries)
	if err != nil {
		return fmt.Sprintf("unavailable: %v", err)
	}
	return boundedJournalOutput(output)
}
