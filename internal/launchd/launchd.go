// Package launchd runs the relay's background workers as one-shot macOS
// launchd jobs and removes the jobs of workers that have finished.
package launchd

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/childenv"
)

const (
	UpdateWorkerPrefix    = "herdr-mobile-relay-update-"
	AppDeployWorkerPrefix = "herdr-mobile-relay-app-deploy-"
)

const plistDoctype = `<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`

type Job struct {
	Label       string
	Program     []string
	Environment map[string]string
}

type runner func(ctx context.Context, args ...string) ([]byte, error)

func launchctl(ctx context.Context, args ...string) ([]byte, error) {
	return childenv.CommandContext(ctx, "launchctl", args...).CombinedOutput()
}

// Bootstrap starts job once. Without KeepAlive, launchd leaves the job loaded
// but idle after it exits, until SweepWorkers removes it.
func Bootstrap(ctx context.Context, dir string, job Job) error {
	return bootstrap(ctx, launchctl, dir, os.Getuid(), job)
}

func bootstrap(ctx context.Context, run runner, dir string, uid int, job Job) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, job.Label+".plist")
	if err := os.WriteFile(path, plist(job), 0o600); err != nil {
		return fmt.Errorf("write launchd job: %w", err)
	}
	defer os.Remove(path)
	domain := fmt.Sprintf("gui/%d", uid)
	// A relay started over SSH, with nobody logged in at the desktop, has no gui domain.
	// SweepWorkers only reaches the relay's own domain, so a relay started over SSH while
	// someone is logged in at the desktop leaves its finished workers loaded, idle, until logout.
	if _, err := run(ctx, "print", domain); err != nil {
		domain = fmt.Sprintf("user/%d", uid)
	}
	if output, err := run(ctx, "bootstrap", domain, path); err != nil {
		return fmt.Errorf("launchctl bootstrap %s: %w: %s", domain, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func plist(job Job) []byte {
	var document bytes.Buffer
	document.WriteString(xml.Header)
	document.WriteString(plistDoctype + "\n")
	document.WriteString("<plist version=\"1.0\">\n<dict>\n")
	writeElement(&document, 1, "key", "Label")
	writeElement(&document, 1, "string", job.Label)
	writeElement(&document, 1, "key", "ProgramArguments")
	document.WriteString("\t<array>\n")
	for _, argument := range job.Program {
		writeElement(&document, 2, "string", argument)
	}
	document.WriteString("\t</array>\n")
	if len(job.Environment) > 0 {
		writeElement(&document, 1, "key", "EnvironmentVariables")
		document.WriteString("\t<dict>\n")
		for _, name := range slices.Sorted(maps.Keys(job.Environment)) {
			writeElement(&document, 2, "key", name)
			writeElement(&document, 2, "string", job.Environment[name])
		}
		document.WriteString("\t</dict>\n")
	}
	writeElement(&document, 1, "key", "RunAtLoad")
	document.WriteString("\t<true/>\n")
	document.WriteString("</dict>\n</plist>\n")
	return document.Bytes()
}

func writeElement(document *bytes.Buffer, depth int, tag, text string) {
	document.WriteString(strings.Repeat("\t", depth) + "<" + tag + ">")
	_ = xml.EscapeText(document, []byte(text))
	document.WriteString("</" + tag + ">\n")
}

// SweepWorkers removes update and app-deploy worker jobs that are not running.
// Releases up to 0.22.4 started workers that launchd restarted forever.
func SweepWorkers(ctx context.Context) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return sweep(ctx, launchctl, UpdateWorkerPrefix, AppDeployWorkerPrefix)
}

func sweep(ctx context.Context, run runner, prefixes ...string) error {
	output, err := run(ctx, "list")
	if err != nil {
		return fmt.Errorf("launchctl list: %w: %s", err, strings.TrimSpace(string(output)))
	}
	var failures []error
	for _, label := range staleWorkers(string(output), prefixes...) {
		if output, err := run(ctx, "remove", label); err != nil {
			failures = append(failures, fmt.Errorf("launchctl remove %s: %w: %s", label, err, strings.TrimSpace(string(output))))
		}
	}
	return errors.Join(failures...)
}

// staleWorkers reads `launchctl list` output, one "PID<tab>Status<tab>Label"
// row per job, where a PID of "-" means the job is not running.
func staleWorkers(listOutput string, prefixes ...string) []string {
	var labels []string
	for _, line := range strings.Split(listOutput, "\n") {
		fields := strings.SplitN(strings.TrimRight(line, "\r"), "\t", 3)
		if len(fields) != 3 || fields[0] != "-" {
			continue
		}
		label := fields[2]
		if slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(label, prefix) }) {
			labels = append(labels, label)
		}
	}
	return labels
}
