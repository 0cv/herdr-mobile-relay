package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/url"
	"strconv"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

// These are the same owned manager and journal used by installed setup/resume.
// The command never infers a registration from a matching observed Serve route.
func runInstalledRouteCommand(operation string, args []string, stdout, stderr io.Writer) (int, error) {
	refused := errors.New("installed CLI route operation refused")
	if !config.TailscaleCLIProfilesEnabled() {
		return 2, tailscalecli.ErrWorkflowRequired
	}
	f := flag.NewFlagSet("tailscale-cli "+operation, flag.ContinueOnError)
	f.SetOutput(stderr)
	binary := f.String("binary", "", "selected CLI")
	state := f.String("state-root", "", "private journal")
	coordination := f.String("coordination-root", "", "shared coordination")
	scope := f.String("scope", "", "selected scope")
	installation := f.String("installation-id", "", "installed identity")
	origin := f.String("origin", "", "selected origin")
	https := f.Int("https-port", 0, "HTTPS listener")
	backend := f.Int("backend-port", 0, "loopback backend")
	node := f.String("node-id", "", "approved node")
	accepted := f.Bool("accepted", false, "operator consent")
	remove := f.Bool("accept-route-removal", false, "scoped route removal")
	race := f.Bool("accept-check-to-write-race", false, "non-atomic mutation")
	drain := f.Bool("accept-no-remote-drain", false, "no remote drain")
	reservation := f.String("reservation-id", "", "exact pending reservation")
	stopped := f.Bool("service-stopped", false, "stopped service confirmation")
	operationID := f.String("operation-id", "", "exact pending operation")
	observed := f.String("confirm-observed-route", "", "exact observed disposition")
	reconcileAccepted := f.Bool("accept-journal-reconciliation", false, "journal reconciliation consent")
	if err := f.Parse(args); err != nil || f.NArg() != 0 {
		return 2, refused
	}
	cfg, err := config.LoadInstalled()
	if err != nil {
		return 1, refused
	}
	selected, err := config.BindInstalledCLI(cfg)
	if err != nil {
		return 1, refused
	}
	u, err := url.Parse(cfg.TailscaleCLIOrigin)
	if err != nil {
		return 1, refused
	}
	port := 443
	if u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil {
			return 1, refused
		}
	}
	if *binary != cfg.TailscaleCLIBin || *state != cfg.TailscaleCLIStateRoot ||
		*coordination != cfg.TailscaleCLICoordinationRoot || *scope != "production" ||
		*scope != cfg.TailscaleCLIScope || *installation != cfg.InstanceID ||
		*origin != cfg.TailscaleCLIOrigin || *https != port || *backend != cfg.Port ||
		(operation != "unpublish" && operation != "release-backend-port" && operation != "reconcile" && (*node != "" || *accepted || *remove || *race || *drain)) ||
		(operation != "release-backend-port" && operation != "reconcile" && (*reservation != "" || *stopped || *operationID != "" || *observed != "" || *reconcileAccepted)) {

		return 1, refused
	}
	m, err := productionInstalledManager(selected)
	if err != nil {
		return 1, refused
	}
	ctx, stop := installedCommandContext()
	defer stop()
	if err := ctx.Err(); err != nil {
		return 1, refused
	}
	switch operation {
	case "status", "assert-ready":
		route, err := m.VerifyRegisteredRoute(ctx, *scope, *installation, *origin, *https, *backend)
		if operation == "status" {
			if encodeErr := json.NewEncoder(stdout).Encode(route); encodeErr != nil {
				return 1, refused
			}
		}
		if err != nil || route.JournalState != tailscalecli.StateRegistered || route.Readiness != tailscalecli.ReadinessReady || !route.RuntimeQualified {
			return 1, refused
		}
	case "recover":
		report, err := m.Recover(ctx, *scope, *installation, *origin, *https, *backend)
		if encodeErr := json.NewEncoder(stdout).Encode(report); encodeErr != nil {
			return 1, refused
		}
		if err != nil {
			return 1, refused
		}
	case "release-backend-port":
		if *node == "" || *reservation == "" || !*stopped || *accepted || *remove || *race || *drain || *operationID != "" || *observed != "" || *reconcileAccepted {
			return 2, refused
		}
		if err := m.ReleaseBackendPort(ctx, *installation, *scope, *node, *origin, *https, *backend, *reservation, true); err != nil {
			return 1, refused
		}
	case "reconcile":
		if *node == "" || *operationID == "" || !*accepted || !*reconcileAccepted || (*observed != "present" && *observed != "absent") || *remove || *race || *drain || *stopped {
			return 2, refused
		}
		if err := m.Reconcile(ctx, *scope, *installation, *origin, *https, *backend, tailscalecli.Consent{
			Accepted: true, Scope: *scope, NodeID: *node, Origin: *origin, HTTPSPort: *https, BackendPort: *backend,
			RecoveryAccepted: true, RecoveryObservation: *observed, OperationID: *operationID, ReservationID: *reservation,
		}); err != nil {
			return 1, refused
		}
	case "unpublish":
		if *accepted && (*reservation != "" || *stopped || *operationID != "" || *observed != "" || *reconcileAccepted) {
			return 2, refused
		}
		if !*accepted || !*remove || !*race || !*drain || *node == "" {
			return 2, refused
		}
		// The exact journal, node, registration and selected listener are
		// rechecked inside Unpublish before its scoped CLI command.
		if err := m.Unpublish(ctx, tailscalecli.Consent{
			Accepted: true, Scope: *scope, NodeID: *node, Origin: *origin,
			HTTPSPort: *https, BackendPort: *backend, RouteRemovalAccepted: true,
			CheckToWriteRaceAccepted: true, NoRemoteDrainAccepted: true,
		}); err != nil {
			return 1, refused
		}
	default:
		return 2, refused
	}
	if ctx.Err() != nil || selected.Validate(cfg) != nil {
		return 1, refused
	}
	return 0, nil
}
