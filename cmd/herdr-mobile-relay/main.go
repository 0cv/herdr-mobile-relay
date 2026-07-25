package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/0cv/herdr-mobile-relay/internal/app"
	"github.com/0cv/herdr-mobile-relay/internal/appdeploy"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/eventhook"
	"github.com/0cv/herdr-mobile-relay/internal/release"
	"github.com/0cv/herdr-mobile-relay/internal/setuphelper"
	"github.com/0cv/herdr-mobile-relay/internal/stablestate"
	"github.com/0cv/herdr-mobile-relay/internal/support"
	relayupdate "github.com/0cv/herdr-mobile-relay/internal/update"
)

var (
	version  = "dev"
	revision = "unknown"
)

func main() {
	exitCode, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdr-mobile-relay: %v\n", err)
		os.Exit(exitCode)
	}
}

func run(args []string) (int, error) {
	command := "serve"
	if len(args) > 0 {
		command, args = args[0], args[1:]
	}
	switch command {
	case "serve":
		if len(args) != 0 {
			return 2, errors.New("serve does not accept arguments")
		}
		return runServe()
	case "version":
		if len(args) == 1 && args[0] == "--json" {
			data, _ := json.Marshal(map[string]string{"version": version, "revision": revision, "target": release.CurrentTarget()})
			fmt.Println(string(data))
			return 0, nil
		}
		if len(args) != 0 {
			return 2, errors.New("usage: herdr-mobile-relay version [--json]")
		}
		fmt.Printf("herdr-mobile-relay %s (%s)\n", version, revision)
		return 0, nil
	case "event-hook":
		if len(args) != 0 {
			return 2, errors.New("event-hook does not accept arguments")
		}
		return status(eventhook.Run())
	case "update-worker":
		if len(args) != 1 {
			return 2, errors.New("usage: herdr-mobile-relay update-worker JOB.json")
		}
		err := relayupdate.Run(context.Background(), args[0])
		if errors.Is(err, relayupdate.ErrConcurrent) {
			return 3, err
		}
		return status(err)
	case "app-deploy-worker":
		if len(args) != 1 {
			return 2, errors.New("usage: herdr-mobile-relay app-deploy-worker JOB.json")
		}
		return status(appdeploy.Run(context.Background(), args[0]))
	case "app-deploy-configured":
		if len(args) != 0 {
			return 2, errors.New("app-deploy-configured does not accept arguments")
		}
		cfg, err := config.Load()
		if err != nil {
			return 1, err
		}
		return status(appdeploy.RunConfigured(context.Background(), cfg.RuntimeDir, cfg.WebRoot, version, revision))
	case "pages-projects":
		if len(args) < 1 || len(args) > 3 {
			return 2, errors.New("usage: herdr-mobile-relay pages-projects {list|names|matching ORIGIN|validate NAME ORIGIN}")
		}
		projects, err := appdeploy.ParseProjects(os.Stdin)
		if err != nil {
			return 1, err
		}
		switch args[0] {
		case "list":
			if len(args) != 1 {
				return 2, errors.New("pages-projects list accepts no arguments")
			}
			for _, project := range projects {
				suffix := ""
				if len(project.Domains) > 0 {
					suffix = " (" + strings.Join(project.Domains, ", ") + ")"
				}
				fmt.Printf("  %s%s\n", project.Name, suffix)
			}
		case "names":
			if len(args) != 1 {
				return 2, errors.New("pages-projects names accepts no arguments")
			}
			for _, project := range projects {
				fmt.Println(project.Name)
			}
		case "matching":
			if len(args) != 2 {
				return 2, errors.New("usage: pages-projects matching ORIGIN")
			}
			matches, err := appdeploy.MatchingProjects(projects, args[1])
			if err != nil {
				return 1, err
			}
			for _, project := range matches {
				fmt.Println(project.Name)
			}
		case "validate":
			if len(args) != 3 {
				return 2, errors.New("usage: pages-projects validate NAME ORIGIN")
			}
			if err := appdeploy.ValidateProject(projects, args[1], args[2]); err != nil {
				return 1, err
			}
		default:
			return 2, errors.New("unknown pages-projects operation")
		}
		return 0, nil
	case "stable-state":
		if len(args) == 0 {
			return 2, errors.New("stable-state requires an operation")
		}
		return status(stablestate.Run(args, os.Stdout, os.Stderr))
	case "support":
		if len(args) != 0 {
			return 2, errors.New("support does not accept arguments")
		}
		cfg, err := config.Load()
		if err != nil {
			return 1, err
		}
		snapshot, err := support.Load(cfg.RuntimeDir)
		if err != nil {
			return 1, err
		}
		encoded, err := json.MarshalIndent(snapshot, "", "  ")
		if err != nil {
			return 1, err
		}
		fmt.Println(string(encoded))
		return 0, nil
	case "verify-release":
		verifyFlags := flag.NewFlagSet("verify-release", flag.ContinueOnError)
		verifyFlags.SetOutput(os.Stderr)
		target := verifyFlags.String("target", release.CurrentTarget(), "expected os/architecture")
		expectedVersion := verifyFlags.String("version", "", "expected release version")
		expectedRevision := verifyFlags.String("revision", "", "expected release revision")
		allowCrossTarget := verifyFlags.Bool("allow-cross-target", false, "allow a build-host tool to verify another target")
		if err := verifyFlags.Parse(args); err != nil {
			return 2, err
		}
		if *allowCrossTarget && (*expectedVersion != "" || *expectedRevision != "") {
			return 2, errors.New("--allow-cross-target cannot be combined with --version or --revision candidate checks")
		}
		if verifyFlags.NArg() > 1 {
			return 2, errors.New("usage: herdr-mobile-relay verify-release [--target os/arch] [--version VERSION] [--revision REVISION] [--allow-cross-target] [DIRECTORY]")
		}
		root := ""
		if verifyFlags.NArg() == 1 {
			root = verifyFlags.Arg(0)
		} else {
			executable, err := os.Executable()
			if err != nil {
				return 1, err
			}
			root = filepath.Dir(executable)
		}
		manifest, err := release.Verify(root, *target)
		if err != nil {
			return 1, err
		}
		if err := verifyReleaseIdentity(manifest, *expectedVersion, *expectedRevision, *target, *allowCrossTarget); err != nil {
			return 1, err
		}
		encoded, _ := json.Marshal(manifest)
		fmt.Println(string(encoded))
		return 0, nil
	case "release-manifest":
		if len(args) != 4 {
			return 2, errors.New("usage: herdr-mobile-relay release-manifest DIRECTORY VERSION REVISION os/arch")
		}
		manifest, err := release.Build(args[0], args[1], args[2], args[3])
		if err != nil {
			return 1, err
		}
		encoded, _ := json.Marshal(manifest)
		fmt.Println(string(encoded))
		return 0, nil
	case "activate-release":
		if len(args) != 2 {
			return 2, errors.New("usage: herdr-mobile-relay activate-release RELEASE_ROOT RELEASE_DIRECTORY")
		}
		if _, err := release.Verify(args[1], release.CurrentTarget()); err != nil {
			return 1, fmt.Errorf("refusing to activate invalid release: %w", err)
		}
		return status(relayupdate.Activate(args[0], args[1]))
	case "seal-release":
		if len(args) != 1 {
			return 2, errors.New("usage: herdr-mobile-relay seal-release RELEASE_DIRECTORY")
		}
		return status(release.Seal(args[0]))
	case "prune-releases":
		if len(args) < 2 || len(args) > 3 {
			return 2, errors.New("usage: herdr-mobile-relay prune-releases RELEASE_ROOT CURRENT_RELEASE [PREVIOUS_RELEASE]")
		}
		return status(relayupdate.PruneOldReleases(args[0], args[1:]...))
	case "setup-fragment":
		if len(args) < 2 || len(args) > 3 {
			return 2, errors.New("usage: herdr-mobile-relay setup-fragment TOKEN LABEL [RELAY]")
		}
		relay := ""
		if len(args) == 3 {
			relay = args[2]
		}
		fmt.Println(setuphelper.SetupFragment(args[0], args[1], relay))
		return 0, nil
	case "normalize-origin":
		normalizeFlags := flag.NewFlagSet("normalize-origin", flag.ContinueOnError)
		normalizeFlags.SetOutput(os.Stderr)
		allowLoopback := normalizeFlags.Bool("allow-loopback-http", false, "allow HTTP loopback origins")
		if err := normalizeFlags.Parse(args); err != nil {
			return 2, err
		}
		if normalizeFlags.NArg() != 1 {
			return 2, errors.New("usage: herdr-mobile-relay normalize-origin [--allow-loopback-http] ORIGIN")
		}
		origin, err := setuphelper.NormalizeOrigin(normalizeFlags.Arg(0), *allowLoopback)
		if err != nil {
			return 1, err
		}
		fmt.Println(origin)
		return 0, nil
	case "qr":
		qrFlags := flag.NewFlagSet("qr", flag.ContinueOnError)
		qrFlags.SetOutput(os.Stderr)
		columns := qrFlags.Int("columns", 80, "maximum terminal columns")
		if err := qrFlags.Parse(args); err != nil {
			return 2, err
		}
		if qrFlags.NArg() != 1 || *columns < 1 {
			return 2, errors.New("usage: herdr-mobile-relay qr [--columns N] VALUE")
		}
		rendered, err := setuphelper.TerminalQR(qrFlags.Arg(0), *columns)
		if err != nil {
			return 1, err
		}
		fmt.Println(rendered)
		return 0, nil
	default:
		return 2, fmt.Errorf("unknown subcommand %q", command)
	}
}

func verifyReleaseIdentity(
	manifest release.Manifest,
	expectedVersion, expectedRevision, expectedTarget string,
	allowCrossTarget bool,
) error {
	if expectedVersion != "" && manifest.Version != expectedVersion {
		return fmt.Errorf("release manifest version %q does not match expected version %q", manifest.Version, expectedVersion)
	}
	if expectedRevision != "" && manifest.Revision != expectedRevision {
		return fmt.Errorf("release manifest revision %q does not match expected revision %q", manifest.Revision, expectedRevision)
	}
	if expectedTarget != "" && manifest.Target != expectedTarget {
		return fmt.Errorf("release manifest target %q does not match expected target %q", manifest.Target, expectedTarget)
	}
	if manifest.Version != version {
		return fmt.Errorf("release manifest version %q does not match binary version %q", manifest.Version, version)
	}
	if manifest.Revision != revision {
		return fmt.Errorf("release manifest revision %q does not match binary revision %q", manifest.Revision, revision)
	}
	if !allowCrossTarget && manifest.Target != release.CurrentTarget() {
		return fmt.Errorf("release manifest target %q does not match binary target %q", manifest.Target, release.CurrentTarget())
	}
	return nil
}

func runServe() (int, error) {
	cfg, err := config.Load()
	if err != nil {
		return 1, err
	}

	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	if cfg.LogFormat == "json" {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := app.New(cfg, version, revision, logger)

	if err := srv.Run(ctx); err != nil && ctx.Err() == nil {
		return 1, err
	}
	return 0, nil
}

func status(err error) (int, error) {
	if err != nil {
		return 1, err
	}
	return 0, nil
}
