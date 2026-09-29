package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

func runTailscaleCLI(args []string, stdout, stderr io.Writer) (int, error) {
	return runTailscaleCLIWithInput(args, os.Stdin, stdout, stderr)
}

func runTailscaleCLIWithInput(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(args) > 0 && args[0] == "check-transport-switch" {
		return runTailscaleCLITransportSwitchCheck(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "activation-check" {
		flags := flag.NewFlagSet("tailscale-cli activation-check", flag.ContinueOnError)
		flags.SetOutput(stderr)
		scope := flags.String("scope", "production", "production or development activation scope")
		if err := flags.Parse(args[1:]); err != nil {
			return 2, err
		}
		if flags.NArg() != 0 {
			return 2, errors.New("activation-check accepts no positional arguments")
		}
		if *scope == "development" && config.TailscaleCLIDevelopmentQualificationEnabled() {
			_, err := fmt.Fprintln(stdout, "development-qualification-enabled; runtime qualification remains pending")
			return status(err)
		}
		if *scope == "production" && config.TailscaleCLIProfilesEnabled() {
			return 0, nil
		}
		return 2, errors.New("production activation remains disabled pending physical-phone qualification and separate production enablement; no CLI or service was contacted")
	}
	if len(args) == 0 {
		return 2, tailscaleCLIUsageError()
	}
	operation, operationArgs := args[0], args[1:]
	if operation == "resolve-binary" {
		flags := flag.NewFlagSet("tailscale-cli resolve-binary", flag.ContinueOnError)
		flags.SetOutput(stderr)
		binary := flags.String("binary", os.Getenv("HERDR_TAILSCALE_CLI_BIN"), "explicit absolute Tailscale CLI override")
		if err := flags.Parse(operationArgs); err != nil {
			return 2, err
		}
		if flags.NArg() != 0 {
			return 2, errors.New("resolve-binary accepts no positional arguments")
		}
		selected, err := tailscalecli.ResolveBinary(*binary, os.Getenv("PATH"), runtime.GOOS)
		if err != nil {
			return 1, err
		}
		_, err = fmt.Fprintln(stdout, selected)
		return status(err)
	}
	if operation == "preflight" {
		flags := flag.NewFlagSet("tailscale-cli preflight", flag.ContinueOnError)
		flags.SetOutput(stderr)
		binary := flags.String("binary", os.Getenv("HERDR_TAILSCALE_CLI_BIN"), "selected absolute Tailscale CLI path")
		scope := flags.String("scope", "", "development only")
		httpsPort := flags.Int("https-port", 443, "Tailscale HTTPS Serve listener port")
		if err := flags.Parse(operationArgs); err != nil {
			return 2, err
		}
		if flags.NArg() != 0 {
			return 2, errors.New("preflight accepts no positional arguments")
		}
		if *scope != "development" || !config.TailscaleCLIDevelopmentQualificationEnabled() {
			return 2, errors.New("Tailscale CLI preflight is limited to development scope")
		}
		client, err := selectedCLIClient(*binary)
		if err != nil {
			return 1, err
		}
		report, err := client.Preflight(context.Background(), *httpsPort)
		if err != nil {
			return tailscalePreflightExitCode(err), err
		}
		if report.Profile != tailscalecli.ProfileAppStoreSupplied || !report.DevelopmentQualificationEnabled {
			return 78, tailscalecli.ErrUnsupported
		}
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			return 1, err
		}
		return 0, nil
	}

	flags := flag.NewFlagSet("tailscale-cli "+operation, flag.ContinueOnError)
	flags.SetOutput(stderr)
	binary := flags.String("binary", os.Getenv("HERDR_TAILSCALE_CLI_BIN"), "selected absolute Tailscale CLI path")
	developmentRoot := flags.String("development-root", "", "private marked CLI development root")
	stateRoot := flags.String("state-root", os.Getenv("HERDR_TAILSCALE_CLI_STATE_ROOT"), "private registration root")
	coordinationRoot := flags.String("coordination-root", os.Getenv("HERDR_TAILSCALE_CLI_COORDINATION_ROOT"), "shared private node-lock root")
	scope := flags.String("scope", os.Getenv("HERDR_TAILSCALE_CLI_SCOPE"), "production or development registration scope")
	installationID := flags.String("installation-id", os.Getenv("HERDR_RELAY_INSTANCE_ID"), "stable relay installation identifier")
	nodeID := flags.String("node-id", "", "expected authenticated node ID")
	origin := flags.String("origin", os.Getenv("HERDR_TAILSCALE_CLI_ORIGIN"), "canonical HTTPS origin bound to the selected node")
	httpsPort := flags.Int("https-port", 443, "Tailscale HTTPS Serve listener port")
	backendPort := flags.Int("backend-port", 0, "loopback relay backend port")
	operationID := flags.String("operation-id", "", "exact pending journal operation identifier")
	reservationID := flags.String("reservation-id", "", "exact backend reservation attempt identifier")
	recoveryObservation := flags.String("confirm-observed-route", "", "operator confirmation of exact observed route state: present or absent")
	accepted := flags.Bool("accepted", false, "affirm the exact scoped operation")
	persistentRoute := flags.Bool("accept-persistent-route", false, "accept persistence after relay stop")
	checkToWriteRace := flags.Bool("accept-check-to-write-race", false, "accept the non-atomic CLI check-to-write limit")
	portReuse := flags.Bool("accept-port-reuse", false, "accept local backend port reuse risk")
	noRollback := flags.Bool("accept-no-rollback", false, "accept no automatic global rollback")
	noRemoteDrain := flags.Bool("accept-no-remote-drain", false, "accept that remote connections may not be drained")
	removeRoute := flags.Bool("accept-route-removal", false, "authorize removal of only the journaled route")
	reconcile := flags.Bool("accept-journal-reconciliation", false, "authorize local journal reconciliation for the exact pending operation")
	serviceStopped := flags.Bool("service-stopped", false, "confirm the relay service is stopped/disabled before reservation release")
	if err := flags.Parse(operationArgs); err != nil {
		return 2, err
	}
	if flags.NArg() != 0 {
		return 2, fmt.Errorf("unexpected arguments for tailscale-cli %s", operation)
	}
	if *scope != "development" || !config.TailscaleCLIDevelopmentQualificationEnabled() {
		return 2, errors.New("real Tailscale CLI operations are limited to isolated development scope; production activation remains disabled")
	}
	client, err := selectedCLIClient(*binary)
	if err != nil {
		return 1, err
	}
	manager, err := tailscalecli.NewDevelopmentManager(*developmentRoot, *stateRoot, *coordinationRoot, client)
	if err != nil {
		return 1, err
	}
	ctx := context.Background()
	switch operation {
	case "reserve-backend-port":
		return status(manager.ReserveBackendPort(ctx, *installationID, *scope, *nodeID, *origin, *httpsPort, *backendPort, *reservationID))
	case "release-backend-port":
		return status(manager.ReleaseBackendPort(ctx, *installationID, *scope, *nodeID, *origin, *httpsPort, *backendPort, *reservationID, *serviceStopped))
	case "status", "recover", "assert-ready":
		report, recoverErr := manager.Recover(ctx, *scope, *installationID, *origin, *httpsPort, *backendPort)
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			return 1, err
		}
		if recoverErr != nil {
			return 1, recoverErr
		}
		qualificationEnabled := report.Route.RuntimeQualified
		if *scope == "development" {
			qualificationEnabled = report.Route.DevelopmentQualificationEnabled
		}
		if operation == "assert-ready" && (report.Route.JournalState != tailscalecli.StateRegistered ||
			report.Route.Readiness != tailscalecli.ReadinessReady || !qualificationEnabled) {
			return 1, errors.New("persistent route is not registered, development-enabled or runtime-qualified, and ready")
		}
		return 0, nil
	case "publish":
		preflight, err := client.Preflight(ctx, *httpsPort)
		if err != nil {
			return tailscalePreflightExitCode(err), err
		}
		if preflight.Profile != tailscalecli.ProfileAppStoreSupplied || !preflight.DevelopmentQualificationEnabled ||
			preflight.NodeID != *nodeID || preflight.Origin != *origin {
			return 78, errors.New("read-only App Store 1.102.4 development preflight does not match the requested route")
		}
		confirmation := tailscalecli.PublishRouteConfirmation(preflight.NodeID, preflight.Origin, *httpsPort, *backendPort)
		_, _ = fmt.Fprintln(stderr, "Development qualification is enabled for this exact profile; this is not runtime qualification.")
		_, _ = fmt.Fprintln(stderr, "The route persists after stop; consent includes the CLI check-to-write race, local port reuse, no global rollback, and no remote-drain guarantee.")
		_, _ = fmt.Fprintf(stderr, "Type this exact route-bound confirmation on stdin:\n%s\n", confirmation)
		typedConfirmation, err := readRouteConfirmation(stdin, confirmation)
		if err != nil {
			return 2, err
		}
		consent := tailscalecli.Consent{
			Accepted:                 *accepted,
			RouteConfirmation:        typedConfirmation,
			Scope:                    *scope,
			NodeID:                   *nodeID,
			Origin:                   *origin,
			HTTPSPort:                *httpsPort,
			BackendPort:              *backendPort,
			PersistentRouteAccepted:  *persistentRoute,
			CheckToWriteRaceAccepted: *checkToWriteRace,
			PortReuseRiskAccepted:    *portReuse,
			NoRollbackAccepted:       *noRollback,
			NoRemoteDrainAccepted:    *noRemoteDrain,
		}
		err = manager.Publish(ctx, tailscalecli.PublishRequest{
			InstallationID: *installationID,
			Scope:          *scope,
			ExpectedNodeID: *nodeID,
			Origin:         *origin,
			HTTPSPort:      *httpsPort,
			BackendPort:    *backendPort,
			ReservationID:  *reservationID,
			Consent:        consent,
		})
		if errors.Is(err, tailscalecli.ErrPublishNotDispatched) {
			return 3, err
		}
		return status(err)
	case "unpublish":
		consent := tailscalecli.Consent{
			Accepted:                 *accepted,
			Scope:                    *scope,
			NodeID:                   *nodeID,
			Origin:                   *origin,
			HTTPSPort:                *httpsPort,
			BackendPort:              *backendPort,
			RouteRemovalAccepted:     *removeRoute,
			CheckToWriteRaceAccepted: *checkToWriteRace,
			NoRemoteDrainAccepted:    *noRemoteDrain,
		}
		return status(manager.Unpublish(ctx, consent))
	case "reconcile":
		consent := tailscalecli.Consent{
			Accepted:            *accepted,
			Scope:               *scope,
			NodeID:              *nodeID,
			Origin:              *origin,
			HTTPSPort:           *httpsPort,
			BackendPort:         *backendPort,
			OperationID:         *operationID,
			RecoveryObservation: *recoveryObservation,
			RecoveryAccepted:    *reconcile,
		}
		return status(manager.Reconcile(ctx, *scope, *installationID, *origin, *httpsPort, *backendPort, consent))
	default:
		return 2, tailscaleCLIUsageError()
	}
}

func runTailscaleCLITransportSwitchCheck(args []string, stdout, stderr io.Writer) (int, error) {
	flags := flag.NewFlagSet("tailscale-cli check-transport-switch", flag.ContinueOnError)
	flags.SetOutput(stderr)
	stateRoot := flags.String("state-root", os.Getenv("HERDR_TAILSCALE_CLI_STATE_ROOT"), "private registration root")
	coordinationRoot := flags.String("coordination-root", os.Getenv("HERDR_TAILSCALE_CLI_COORDINATION_ROOT"), "shared private node-lock root")
	installationID := flags.String("installation-id", os.Getenv("HERDR_RELAY_INSTANCE_ID"), "stable relay installation identifier")
	backendPort := flags.Int("backend-port", 0, "loopback relay backend port")
	if err := flags.Parse(args); err != nil {
		return 2, err
	}
	if flags.NArg() != 0 {
		return 2, errors.New("check-transport-switch accepts no positional arguments")
	}
	if err := tailscalecli.CheckTransportSwitch(*stateRoot, *coordinationRoot, *installationID, *backendPort); err != nil {
		return 1, err
	}
	return 0, nil
}

func tailscalePreflightExitCode(err error) int {
	if errors.Is(err, tailscalecli.ErrTransientUnavailable) {
		return 75
	}
	return 78
}

func selectedCLIClient(binary string) (*tailscalecli.Client, error) {
	selected, err := tailscalecli.ResolveBinary(binary, os.Getenv("PATH"), runtime.GOOS)
	if err != nil {
		return nil, err
	}
	return tailscalecli.NewClient(selected)
}

func readRouteConfirmation(stdin io.Reader, expected string) (string, error) {
	if stdin == nil {
		return "", errors.New("route-bound confirmation must be supplied on stdin")
	}
	reader := bufio.NewReaderSize(stdin, 512)
	line, err := reader.ReadSlice('\n')
	if err != nil || len(line) > 512 {
		return "", errors.New("route-bound confirmation line is missing or oversized")
	}
	typed := strings.TrimSuffix(string(line), "\n")
	if typed != expected {
		return "", errors.New("route-bound confirmation did not exactly match the selected node, listener and backend")
	}
	return typed, nil
}

func tailscaleCLIUsageError() error {
	return errors.New("usage: herdr-mobile-relay tailscale-cli {activation-check [--scope development]|resolve-binary|preflight --scope development|check-transport-switch|reserve-backend-port|release-backend-port|status|recover|reconcile|assert-ready|publish|unpublish} [--development-root <marked-private-root>] [options]")
}
