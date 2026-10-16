package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/0cv/herdr-mobile-relay/internal/doctor"
	"github.com/0cv/herdr-mobile-relay/internal/release"
)

// Package-private acquisition/I/O seams for hosted tests. Production constructs
// these here, never from arguments/configuration or a caller-supplied executor.
// Report policy, scheduling and rendering always remain the real doctor APIs.
type diagnosticRuntime struct {
	executable                func() (string, error)
	getenv                    func(string) string
	readConfig                func(string) doctor.Inspection
	readIdentity              func(string, string, string, string) doctor.BuildIdentity
	queries                   func(string, doctor.Inspection, doctor.BuildIdentity) doctor.Dependencies
	version, revision, target string
}

func productionDiagnosticRuntime() diagnosticRuntime {
	return diagnosticRuntime{
		executable: os.Executable, getenv: os.Getenv,
		readConfig: doctor.ReadConfig, readIdentity: doctor.ReadPackageIdentity,
		queries: func(root string, cfg doctor.Inspection, build doctor.BuildIdentity) doctor.Dependencies {
			q := doctor.NewPackageQueries(root, cfg, build)
			return doctor.Dependencies{Host: q, Transport: q} // no ingress implementation
		},
		version: version, revision: revision, target: release.CurrentTarget(),
	}
}

func runDoctorCommand(args []string, concise bool) (int, error) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runDoctorWithRuntime(ctx, args, concise, os.Stdout, productionDiagnosticRuntime())
}

type diagnosticFlags struct {
	json, ingress bool
	config        string
}

// Fixed syntax only. Duplicates, boolean values, positional/unknown arguments,
// empty/relative/noncanonical config paths and help are invocation errors. Even
// --json with an invalid invocation writes no document; main prints one fixed
// diagnostic on stderr. Selected *file contents* are instead report evidence.
func parseDiagnosticArgs(args []string) (diagnosticFlags, bool) {
	var flags diagnosticFlags
	if len(args) > 4 {
		return flags, false
	}
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if seen[arg] {
			return diagnosticFlags{}, false
		}
		seen[arg] = true
		switch arg {
		case "--json":
			flags.json = true
		case "--probe-ingress":
			flags.ingress = true
		case "--config":
			i++
			if i >= len(args) || !diagnosticSelection(args[i]) {
				return diagnosticFlags{}, false
			}
			flags.config = args[i]
		default:
			return diagnosticFlags{}, false
		}
	}
	return flags, true
}

func diagnosticSelection(path string) bool {
	return len(path) > 0 && len(path) <= 4096 && utf8.ValidString(path) &&
		filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" &&
		strings.IndexFunc(path, unicode.IsControl) < 0
}

// os.Executable may name the installed shim rather than its target. Resolve
// that trusted OS-supplied path (not a config/argument selector) before deriving
// the package root. This does not authorize following manifest/config symlinks:
// their readers still use their own no-follow acquisition and identity checks.
func diagnosticExecutableRoot(path string) string {
	if !diagnosticSelection(path) {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !diagnosticSelection(resolved) {
		return ""
	}
	info, err := os.Lstat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return ""
	}
	return filepath.Dir(resolved)
}

func diagnosticEnded(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	deadline, ok := ctx.Deadline()
	return ok && !time.Now().Before(deadline)
}

func runDoctorWithRuntime(parent context.Context, args []string, concise bool, out io.Writer, runtime diagnosticRuntime) (int, error) {
	flags, valid := parseDiagnosticArgs(args)
	if !valid || parent == nil {
		return 2, errors.New("invalid diagnostic invocation")
	}
	// One budget begins BEFORE configuration/metadata acquisition. No reads in
	// abandoned goroutines: a noncooperative filesystem may exceed it, but late
	// metadata is discarded and the runner schedules no expired query.
	ctx, cancel := context.WithTimeout(parent, doctor.GlobalBudget)
	defer cancel()
	var cfg doctor.Inspection
	var build doctor.BuildIdentity
	root := ""
	if !diagnosticEnded(ctx) {
		if executable, err := runtime.executable(); err == nil {
			root = diagnosticExecutableRoot(executable)
		}
	}
	selected := flags.config
	if selected == "" {
		selected = runtime.getenv("HERDR_RELAY_ENV")
		if selected == "" {
			if dir := runtime.getenv("HERDR_PLUGIN_CONFIG_DIR"); dir != "" {
				// Validate before Join can normalize an invalid selector.
				if diagnosticSelection(dir) {
					selected = filepath.Join(dir, "relay.env")
				} else {
					selected = dir
				}
			} else if root != "" {
				selected = filepath.Join(root, "relay", ".env")
			}
		}
	}
	if selected != "" && !diagnosticEnded(ctx) {
		if diagnosticSelection(selected) {
			cfg = runtime.readConfig(selected)
		} else {
			// Invalid ambient selectors are refused configuration evidence, not
			// permission to normalize/guess a different file or reflect the path.
			cfg = doctor.ParseConfig(strings.NewReader("!"))
		}
	}
	if root != "" && !diagnosticEnded(ctx) {
		build = runtime.readIdentity(root, runtime.version, runtime.revision, runtime.target)
	}
	if diagnosticEnded(ctx) {
		build = doctor.BuildIdentity{}
	}
	// Only the OS-resolved executing package root reaches the verifier. No
	// argument/config/ambient web-root or responding relay supplies authority.
	deps := runtime.queries(root, cfg, build)
	report := doctor.Run(ctx, cfg, build, doctor.Options{ProbeIngress: flags.ingress}, deps)
	if err := renderDiagnostic(report, out, flags.json, concise); err != nil {
		return 1, err
	}
	return report.ExitCode(), nil
}

func renderDiagnostic(report doctor.Report, out io.Writer, json, concise bool) error {
	if json {
		return report.RenderJSON(out)
	}
	if concise {
		return report.RenderStatus(out)
	}
	return report.RenderHuman(out)
}
