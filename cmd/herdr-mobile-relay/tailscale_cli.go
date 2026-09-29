package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

func runTailscaleCLI(args []string, stdout, stderr io.Writer) (int, error) {
	if len(args) == 1 && args[0] == "activation-check" {
		if !config.TailscaleCLIProfilesEnabled() {
			return 2, errors.New("tailscale-cli is not enabled: candidate profiles remain unqualified pending P6; no CLI or service was contacted")
		}
		return 0, nil
	}
	if !config.TailscaleCLIProfilesEnabled() {
		return 2, errors.New("tailscale-cli is not enabled: candidate profiles remain unqualified pending P6; no CLI or service was contacted")
	}
	if len(args) == 0 {
		return 2, errors.New("usage: herdr-mobile-relay tailscale-cli {activation-check|status|recover|assert-ready|publish|unpublish} [options]")
	}
	operation, args := args[0], args[1:]
	flags := flag.NewFlagSet("tailscale-cli "+operation, flag.ContinueOnError)
	flags.SetOutput(stderr)
	binary := flags.String("binary", os.Getenv("HERDR_TAILSCALE_CLI_BIN"), "selected absolute Tailscale CLI path")
	stateRoot := flags.String("state-root", os.Getenv("HERDR_TAILSCALE_CLI_STATE_ROOT"), "private registration root")
	coordinationRoot := flags.String("coordination-root", os.Getenv("HERDR_TAILSCALE_CLI_COORDINATION_ROOT"), "shared private node-lock root")
	scope := flags.String("scope", os.Getenv("HERDR_TAILSCALE_CLI_SCOPE"), "production or development registration scope")
	installationID := flags.String("installation-id", os.Getenv("HERDR_RELAY_INSTANCE_ID"), "stable relay installation identifier")
	nodeID := flags.String("node-id", "", "expected authenticated node ID")
	httpsPort := flags.Int("https-port", 443, "Tailscale HTTPS Serve listener port")
	backendPort := flags.Int("backend-port", 0, "loopback relay backend port")
	accepted := flags.Bool("accepted", false, "affirm the exact scoped operation")
	persistentRoute := flags.Bool("accept-persistent-route", false, "accept persistence after relay stop")
	checkToWriteRace := flags.Bool("accept-check-to-write-race", false, "accept the non-atomic CLI check-to-write limit")
	portReuse := flags.Bool("accept-port-reuse", false, "accept local backend port reuse risk")
	noRollback := flags.Bool("accept-no-rollback", false, "accept no automatic global rollback")
	noRemoteDrain := flags.Bool("accept-no-remote-drain", false, "accept that remote connections may not be drained")
	removeRoute := flags.Bool("accept-route-removal", false, "authorize removal of only the journaled route")
	if err := flags.Parse(args); err != nil {
		return 2, err
	}
	if flags.NArg() != 0 {
		return 2, fmt.Errorf("unexpected arguments for tailscale-cli %s", operation)
	}
	client, err := tailscalecli.NewClient(*binary)
	if err != nil {
		return 1, err
	}
	manager, err := tailscalecli.NewManager(*stateRoot, *coordinationRoot, client)
	if err != nil {
		return 1, err
	}
	ctx := context.Background()
	switch operation {
	case "status", "recover", "assert-ready":
		report, recoverErr := manager.Recover(ctx, *scope, *installationID, *httpsPort, *backendPort)
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			return 1, err
		}
		if recoverErr != nil {
			return 1, recoverErr
		}
		if operation == "assert-ready" && (report.Route.JournalState != tailscalecli.StateRegistered ||
			report.Route.Readiness != tailscalecli.ReadinessReady || !report.Route.RuntimeQualified) {
			return 1, errors.New("persistent route is not registered, runtime-qualified and ready")
		}
		return 0, nil
	case "publish":
		consent := tailscalecli.Consent{
			Accepted:                 *accepted,
			Scope:                    *scope,
			NodeID:                   *nodeID,
			HTTPSPort:                *httpsPort,
			BackendPort:              *backendPort,
			PersistentRouteAccepted:  *persistentRoute,
			CheckToWriteRaceAccepted: *checkToWriteRace,
			PortReuseRiskAccepted:    *portReuse,
			NoRollbackAccepted:       *noRollback,
			NoRemoteDrainAccepted:    *noRemoteDrain,
		}
		err := manager.Publish(ctx, tailscalecli.PublishRequest{
			InstallationID: *installationID,
			Scope:          *scope,
			ExpectedNodeID: *nodeID,
			HTTPSPort:      *httpsPort,
			BackendPort:    *backendPort,
			Consent:        consent,
		})
		return status(err)
	case "unpublish":
		consent := tailscalecli.Consent{
			Accepted:              *accepted,
			Scope:                 *scope,
			NodeID:                *nodeID,
			HTTPSPort:             *httpsPort,
			BackendPort:           *backendPort,
			RouteRemovalAccepted:  *removeRoute,
			NoRemoteDrainAccepted: *noRemoteDrain,
		}
		return status(manager.Unpublish(ctx, consent))
	default:
		return 2, errors.New("usage: herdr-mobile-relay tailscale-cli {activation-check|status|recover|assert-ready|publish|unpublish} [options]")
	}
}
