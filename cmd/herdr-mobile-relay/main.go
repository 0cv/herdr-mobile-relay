package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/app"
	"github.com/0cv/herdr-mobile-relay/internal/appdeploy"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/eventhook"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/processsupervisor"
	"github.com/0cv/herdr-mobile-relay/internal/release"
	"github.com/0cv/herdr-mobile-relay/internal/setuphelper"
	"github.com/0cv/herdr-mobile-relay/internal/speech"
	"github.com/0cv/herdr-mobile-relay/internal/stablestate"
	"github.com/0cv/herdr-mobile-relay/internal/support"
	"github.com/0cv/herdr-mobile-relay/internal/tailscale"
	relayupdate "github.com/0cv/herdr-mobile-relay/internal/update"
)

var (
	version  = "dev"
	revision = "unknown"
)

func main() {
	exitCode, err := run(os.Args[1:])
	if err != nil {
		reportError(os.Stderr, os.Args[1:], err)
	}
	// Always propagate the code the subcommand chose. Some subcommands (for
	// example managed-state) return a meaningful non-zero code with a nil error,
	// so returning normally here would silently turn every refusal into success.
	os.Exit(exitCode)
}

func run(args []string) (int, error) {
	command := "serve"
	if len(args) > 0 {
		command, args = args[0], args[1:]
	}
	switch command {
	case "supervise":
		return runSupervise(args, os.Stdin, os.Stdout, os.Stderr)
	case "__private-process-anchor":
		if len(args) != 0 {
			return 2, errors.New("private process anchor accepts no arguments")
		}
		return processsupervisor.RunAnchor()
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
	case "verify-public":
		verifyFlags := flag.NewFlagSet("verify-public", flag.ContinueOnError)
		verifyFlags.SetOutput(os.Stderr)
		webRoot := verifyFlags.String("web-root", "web", "local web release root")
		origin := verifyFlags.String("origin", "", "public app origin")
		version := verifyFlags.String("version", "", "expected release version (defaults to the local descriptor)")
		revision := verifyFlags.String("revision", "", "expected release revision (defaults to local version metadata)")
		if err := verifyFlags.Parse(args); err != nil {
			return 2, err
		}
		if verifyFlags.NArg() != 0 || *origin == "" {
			return 2, errors.New("usage: herdr-mobile-relay verify-public --origin ORIGIN [--web-root DIRECTORY] [--version VERSION] [--revision REVISION]")
		}
		return status(appdeploy.VerifyPublic(context.Background(), *webRoot, *origin, *version, *revision))
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
	case "speech-voices":
		err := speech.Run(context.Background(), args, os.Stdout, os.Stderr)
		if errors.Is(err, speech.ErrUsage) {
			return 2, err
		}
		return status(err)
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
	case "json-field":
		return runJSONField(args, os.Stdin, os.Stdout)
	case "tailscale", "tailscale-inspect":
		if command == "tailscale" {
			if len(args) == 0 || args[0] != "inspect" {
				return 2, errors.New("usage: herdr-mobile-relay tailscale inspect [--binary PATH] [--https-port PORT]")
			}
			args = args[1:]
		}
		inspectFlags := flag.NewFlagSet("tailscale-inspect", flag.ContinueOnError)
		inspectFlags.SetOutput(os.Stderr)
		binary := inspectFlags.String("binary", "tailscale", "Tailscale CLI path")
		httpsPort := inspectFlags.Int("https-port", tailscale.DefaultHTTPSPort, "Tailscale HTTPS Serve port")
		if err := inspectFlags.Parse(args); err != nil {
			return 2, err
		}
		if inspectFlags.NArg() != 0 {
			return 2, errors.New("usage: herdr-mobile-relay tailscale inspect [--binary PATH] [--https-port PORT]")
		}
		inspection, err := tailscale.Inspect(context.Background(), *binary, *httpsPort)
		if err != nil {
			return 1, err
		}
		encoded, err := json.Marshal(inspection)
		if err != nil {
			return 1, err
		}
		fmt.Println(string(encoded))
		return 0, nil
	case "tailscale-route-check":
		routeFlags := flag.NewFlagSet("tailscale-route-check", flag.ContinueOnError)
		routeFlags.SetOutput(os.Stderr)
		binary := routeFlags.String("binary", "tailscale", "Tailscale CLI path")
		origin := routeFlags.String("origin", "", "expected canonical HTTPS origin")
		httpsPort := routeFlags.Int("https-port", tailscale.DefaultHTTPSPort, "expected Tailscale HTTPS Serve port")
		backendPort := routeFlags.Int("backend-port", 0, "expected loopback relay backend port")
		if err := routeFlags.Parse(args); err != nil {
			return 2, err
		}
		if routeFlags.NArg() != 0 || *origin == "" || *backendPort < 1 || *backendPort > 65535 {
			return 2, errors.New("usage: herdr-mobile-relay tailscale-route-check --origin HTTPS_ORIGIN --backend-port PORT [--binary PATH] [--https-port PORT]")
		}
		inspection, err := tailscale.Inspect(context.Background(), *binary, *httpsPort)
		if err != nil {
			return 1, err
		}
		if !tailscale.ManagedRouteMatches(inspection, *origin, *httpsPort, *backendPort) {
			return 1, errors.New("observed Tailscale route does not match the authenticated managed relay")
		}
		return 0, nil
	case "pairing-control":
		controlFlags := flag.NewFlagSet("pairing-control", flag.ContinueOnError)
		controlFlags.SetOutput(os.Stderr)
		socket := controlFlags.String("socket", "", "managed pairing control socket")
		op := controlFlags.String("operation", "status", "status, activate, arm_bootstrap, or retire")
		runID := controlFlags.String("run-id", "", "managed foreground run identifier")
		instance := controlFlags.String("instance", "", "relay instance identifier")
		if err := controlFlags.Parse(args); err != nil {
			return 2, err
		}
		if controlFlags.NArg() != 0 || *socket == "" || *runID == "" || *instance == "" {
			return 2, errors.New("usage: herdr-mobile-relay pairing-control --socket PATH --operation status|activate|arm_bootstrap|retire --run-id ID --instance ID")
		}
		response, err := localcontrol.Request(context.Background(), *socket, *op, *runID, *instance)
		if err != nil && response.Error == "" {
			return 1, err
		}
		encoded, err := json.Marshal(response)
		if err != nil {
			return 1, err
		}
		fmt.Println(string(encoded))
		// Keep decoded negative replies available for identity and operation-
		// outcome inspection. Transport/decode failures still exit non-zero.
		return 0, nil
	case "check-port":
		portFlags := flag.NewFlagSet("check-port", flag.ContinueOnError)
		portFlags.SetOutput(os.Stderr)
		host := portFlags.String("host", "127.0.0.1", "address to check")
		port := portFlags.Int("port", 0, "port to check")
		protocol := portFlags.String("protocol", "tcp", "tcp or udp")
		if err := portFlags.Parse(args); err != nil {
			return 2, err
		}
		if portFlags.NArg() != 0 || *port < 1 || *port > 65535 || (*protocol != "tcp" && *protocol != "udp") {
			return 2, errors.New("usage: herdr-mobile-relay check-port --host HOST --port PORT [--protocol tcp|udp]")
		}
		address := net.JoinHostPort(*host, fmt.Sprint(*port))
		var closeListener func() error
		var err error
		if *protocol == "udp" {
			var listener net.PacketConn
			listener, err = net.ListenPacket("udp", address)
			if listener != nil {
				closeListener = listener.Close
			}
		} else {
			var listener net.Listener
			listener, err = net.Listen("tcp", address)
			if listener != nil {
				closeListener = listener.Close
			}
		}
		if err != nil {
			return 1, fmt.Errorf("port %s:%d is occupied: %w", *host, *port, err)
		}
		_ = closeListener()
		return 0, nil
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
	case "normalize-external-origin":
		if len(args) != 1 {
			return 2, errors.New("usage: herdr-mobile-relay normalize-external-origin CANONICAL_HTTPS_ORIGIN")
		}
		origin, err := setuphelper.NormalizeExternalHTTPSOrigin(args[0])
		if err != nil {
			return 1, err
		}
		fmt.Println(origin)
		return 0, nil
	case "managed-state":
		if len(args) > 0 && args[0] == "reprint" {
			return runManagedReprint(args[1:], os.Stdout, os.Stderr), nil
		}
		if len(args) > 0 && args[0] == "recover" {
			return runManagedRecover(args[1:], os.Stdout, os.Stderr), nil
		}
		return runManagedState(args, os.Stdout, os.Stderr, managedStateSignals(), os.Getppid, 200*time.Millisecond), nil
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

	logger := newRelayLogger(os.Stderr, cfg.LogFormat, cfg.LogLevel, stderrIsJournal(os.Stderr))
	slog.SetDefault(logger)

	// O is acquired before managed app/control resources. Tailscale ownership
	// is released only after exact route clearing and local watch closure; an
	// unresolved result never falls through this defer to retire O.
	var owner *app.ManagedOwner
	if cfg.ManagedRunID != "" {
		acquired, acquireErr := app.AcquireManagedOwner(cfg.RuntimeDir)
		if acquireErr != nil {
			return 1, fmt.Errorf("acquire managed ownership of %s: %w", cfg.RuntimeDir, acquireErr)
		}
		owner = acquired
	}
	var srv *app.Server
	defer func() {
		if owner == nil {
			return
		}
		if cfg.Transport == config.TransportTailscale && srv != nil && !srv.ManagedOwnerReleaseSafe() {
			logger.Error("Tailscale cleanup is unresolved; retained owner lock must not be released", "runtime_dir", cfg.RuntimeDir)
			return
		}
		if retireErr := app.RetireManagedOwner(owner, logger); retireErr != nil {
			logger.Error("managed ownership retirement was refused; retained lock evidence must be inspected",
				"runtime_dir", cfg.RuntimeDir, "error", retireErr)
		}
	}()

	if cfg.Transport == config.TransportTailscale {
		srv, err = app.NewOwned(cfg, version, revision, logger, owner)
		if err != nil {
			return 1, err
		}
		signals := make(chan os.Signal, 2)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(signals)
		done := make(chan struct{})
		defer close(done)
		go func() {
			for {
				select {
				case <-done:
					return
				case <-signals:
					retireCtx, cancel := context.WithTimeout(context.Background(), localcontrol.RetireTimeout)
					retireErr := srv.RetireManagedTailscale(retireCtx)
					cancel()
					if retireErr == nil {
						srv.CompleteManagedTailscaleRetirement()
						return
					}
					logger.Error("Tailscale shutdown is quarantined; process and owner remain live for explicit cleanup", "error", retireErr)
				}
			}
		}()
		if err := srv.Run(context.Background()); err != nil {
			return 1, err
		}
		if !srv.ManagedOwnerReleaseSafe() {
			return 1, errors.New("Tailscale owner returned before route cleanup was proven")
		}
		return 0, nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	srv, err = app.NewOwned(cfg, version, revision, logger, owner)
	if err != nil {
		return 1, err
	}
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

// managedStateSignals builds the SIGTERM/SIGINT channel for `managed-state
// hold`. It lives here rather than in managed_state.go so the frozen S9A
// mutant overlay (which replaces that file) still links this package.
func managedStateSignals() <-chan os.Signal {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	return signals
}
