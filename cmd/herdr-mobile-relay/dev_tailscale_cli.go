package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	osSignal "os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/app"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/setuphelper"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

func runDevelopmentTailscaleCLI(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	action := "setup"
	if len(args) > 0 {
		action, args = args[0], args[1:]
	}
	if action == "foreground" {
		flags := flag.NewFlagSet("dev-tailscale-cli foreground", flag.ContinueOnError)
		flags.SetOutput(stderr)
		selected := flags.String("action", "setup", "setup or update")
		if err := flags.Parse(args); err != nil {
			return 2, err
		}
		if flags.NArg() != 0 || (*selected != "setup" && *selected != "update") {
			return 2, errors.New("usage: herdr-mobile-relay dev-tailscale-cli foreground --action {setup|update}")
		}
		action = *selected
	} else if len(args) != 0 {
		return 2, errors.New("dev-tailscale-cli action accepts no extra arguments")
	}
	if action == "stop" {
		_, err := fmt.Fprintln(stdout, "This relay is foreground-only. Send Ctrl-C; the persistent route is retained.")
		return status(err)
	}
	if action != "status" && action != "recover" && action != "assert-ready" &&
		action != "release-reservation" && action != "unpublish" && action != "setup" && action != "update" {
		return 2, errors.New("usage: herdr-mobile-relay dev-tailscale-cli {setup|update|status|recover|assert-ready|release-reservation|unpublish|stop|foreground}")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workflow, cfg, err := developmentCLIFromEnvironment(ctx, action)
	if err != nil {
		return 1, err
	}
	switch action {
	case "status", "recover":
		report, err := workflow.Recover(ctx, cfg.InstanceID, cfg.TailscaleCLIOrigin)
		if encodeErr := json.NewEncoder(stdout).Encode(report); encodeErr != nil {
			return 1, encodeErr
		}
		return status(err)
	case "assert-ready":
		route, err := workflow.VerifyRegisteredRoute(ctx, "development", cfg.InstanceID,
			cfg.TailscaleCLIOrigin, tailscalecli.DevelopmentHTTPSPort, tailscalecli.DevelopmentBackendPort)
		if err == nil && (route.JournalState != tailscalecli.StateRegistered || route.Readiness != tailscalecli.ReadinessReady ||
			!route.DevelopmentQualificationEnabled || route.RuntimeQualified) {
			err = errors.New("persistent development route is not registered and ready")
		}
		if encodeErr := json.NewEncoder(stdout).Encode(route); encodeErr != nil {
			return 1, encodeErr
		}
		return status(err)
	case "release-reservation":
		return releaseDevelopmentReservation(ctx, workflow, cfg, stdin, stdout, stderr)
	case "unpublish":
		return unpublishDevelopmentRoute(ctx, workflow, cfg, stdin, stdout, stderr)
	case "setup", "update":
		return runDevelopmentForeground(ctx, action, workflow, cfg, stdin, stdout, stderr)
	default:
		return 2, errors.New("usage: herdr-mobile-relay dev-tailscale-cli {setup|update|status|recover|assert-ready|release-reservation|unpublish|stop|foreground}")
	}
}

func developmentCLIFromEnvironment(ctx context.Context, action string) (*tailscalecli.DevelopmentWorkflow, *config.Config, error) {
	for _, name := range []string{
		"HERDR_DEV_TAILSCALE_CLI_PORT",
		"HERDR_DEV_TAILSCALE_CLI_PLUGIN_PORT",
		"HERDR_DEV_TAILSCALE_CLI_HTTPS_PORT",
	} {
		if os.Getenv(name) != "" {
			return nil, nil, fmt.Errorf("%s is refused; the development tuple is fixed at HTTPS 8443/backend 18377/plugin 18378", name)
		}
	}
	root := os.Getenv("HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT")
	stateRoot := os.Getenv("HERDR_TAILSCALE_CLI_STATE_ROOT")
	coordinationRoot := os.Getenv("HERDR_TAILSCALE_CLI_COORDINATION_ROOT")
	requireStopped := action == "setup" || action == "update" || action == "unpublish" || action == "release-reservation"
	requireHerdrSocket := action == "setup" || action == "update"
	if err := tailscalecli.ValidateDevelopmentOperationEnvironment(root, stateRoot, coordinationRoot, requireStopped, requireHerdrSocket); err != nil {
		return nil, nil, err
	}
	binary := os.Getenv("HERDR_TAILSCALE_CLI_BIN")
	if binary == "" {
		return nil, nil, tailscalecli.ErrProfileUnavailable
	}
	workflow, report, err := tailscalecli.NewDevelopmentWorkflow(ctx, root, stateRoot, coordinationRoot, binary)
	if err != nil {
		return nil, nil, err
	}
	if expectedNode := os.Getenv("HERDR_TAILSCALE_CLI_NODE_ID"); expectedNode != "" && expectedNode != report.NodeID {
		return nil, nil, tailscalecli.ErrConflict
	}
	if expectedOrigin := os.Getenv("HERDR_TAILSCALE_CLI_ORIGIN"); expectedOrigin != "" && expectedOrigin != report.Origin {
		return nil, nil, tailscalecli.ErrConflict
	}
	if err := os.Setenv("HERDR_TAILSCALE_CLI_ORIGIN", report.Origin); err != nil {
		return nil, nil, err
	}
	if err := os.Setenv("HERDR_TAILSCALE_CLI_SCOPE", "development"); err != nil {
		return nil, nil, err
	}
	cfg, err := config.LoadDevelopmentCLI(workflow)
	if err != nil {
		return nil, nil, err
	}
	return workflow, cfg, nil
}

func runDevelopmentForeground(parent context.Context, action string, workflow *tailscalecli.DevelopmentWorkflow, cfg *config.Config, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if cfg.PairingSocketPath == "" || !filepath.IsAbs(cfg.PairingSocketPath) {
		return 1, tailscalecli.ErrWorkflowRequired
	}
	if action == "update" {
		route, err := workflow.VerifyRegisteredRoute(parent, "development", cfg.InstanceID,
			cfg.TailscaleCLIOrigin, tailscalecli.DevelopmentHTTPSPort, tailscalecli.DevelopmentBackendPort)
		if err != nil || route.JournalState != tailscalecli.StateRegistered || route.Readiness != tailscalecli.ReadinessReady ||
			!route.DevelopmentQualificationEnabled || route.RuntimeQualified {
			if err == nil {
				err = errors.New("exact registered development route is not ready for foreground update")
			}
			return 1, err
		}
	}
	reservationID := ""
	reservationHeld := false
	routeCommitted := false
	if action == "setup" {
		var err error
		reservationID, err = newDevelopmentReservationID()
		if err != nil {
			return 1, err
		}
		if err := workflow.ReserveBackendPort(parent, cfg.InstanceID, workflow.Preflight().NodeID,
			cfg.TailscaleCLIOrigin, reservationID); err != nil {
			return 1, err
		}
		reservationHeld = true
		defer func() {
			if !reservationHeld || routeCommitted {
				return
			}
			if releaseErr := workflow.ReleaseBackendPort(parent, cfg.InstanceID, workflow.Preflight().NodeID,
				cfg.TailscaleCLIOrigin, reservationID, true); releaseErr != nil {
				_, _ = fmt.Fprintf(stderr, "Backend reservation retained because safe release was not proven: %v\\n", releaseErr)
			}
		}()
	}
	server, err := app.NewDevelopmentCLI(cfg, version, revision, newRelayLogger(stderr, cfg.LogFormat, cfg.LogLevel, stderrIsJournal(os.Stderr)), workflow)
	if err != nil {
		return 1, err
	}
	ctx, stop := osSignal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	if err := waitDevelopmentBackend(ctx, cfg); err != nil {
		stop()
		_ = waitDevelopmentServer(done)
		return 1, err
	}
	if action == "setup" {
		preflight := workflow.Preflight()
		confirmation := tailscalecli.PublishRouteConfirmation(preflight.NodeID, preflight.Origin,
			tailscalecli.DevelopmentHTTPSPort, tailscalecli.DevelopmentBackendPort)
		_, _ = fmt.Fprintln(stderr, "This is development enablement, not runtime or phone qualification.")
		_, _ = fmt.Fprintln(stderr, "The HTTPS route persists after the foreground relay stops.")
		_, _ = fmt.Fprintln(stderr, "Consent includes the CLI check-to-write race, backend port reuse, no global rollback, and no remote-drain guarantee.")
		_, _ = fmt.Fprintf(stderr, "Type exactly on stdin:\n%s\n", confirmation)
		typed, err := readRouteConfirmation(stdin, confirmation)
		if err != nil {
			stop()
			_ = waitDevelopmentServer(done)
			return 2, err
		}
		consent := tailscalecli.Consent{
			Accepted: true, RouteConfirmation: typed, Scope: "development", NodeID: preflight.NodeID,
			Origin: preflight.Origin, HTTPSPort: tailscalecli.DevelopmentHTTPSPort,
			BackendPort: tailscalecli.DevelopmentBackendPort, PersistentRouteAccepted: true,
			CheckToWriteRaceAccepted: true, PortReuseRiskAccepted: true,
			NoRollbackAccepted: true, NoRemoteDrainAccepted: true,
		}
		request := tailscalecli.PublishRequest{
			InstallationID: cfg.InstanceID, Scope: "development", ExpectedNodeID: preflight.NodeID,
			Origin: preflight.Origin, HTTPSPort: tailscalecli.DevelopmentHTTPSPort,
			BackendPort: tailscalecli.DevelopmentBackendPort, ReservationID: reservationID, Consent: consent,
		}
		if err := workflow.Publish(ctx, request); err != nil {
			stop()
			_ = waitDevelopmentServer(done)
			return 1, err
		}
		routeCommitted = true
	}
	admitted, err := localcontrol.Request(ctx, cfg.PairingSocketPath, "admit", cfg.ControlRunID, cfg.InstanceID)
	if err != nil || !admitted.OK || !admitted.Ready || !admitted.PersistentRouteReady {
		stop()
		_ = waitDevelopmentServer(done)
		if err == nil {
			err = errors.New("development admission did not confirm the exact registered route")
		}
		return 1, err
	}
	if action == "setup" {
		armed, err := localcontrol.Request(ctx, cfg.PairingSocketPath, "arm_bootstrap", cfg.ControlRunID, cfg.InstanceID)
		if err != nil || !armed.OK || !armed.InvitationArmed {
			stop()
			_ = waitDevelopmentServer(done)
			if err == nil {
				err = errors.New("development invitation was not durably armed")
			}
			return 1, err
		}
		if err := printDevelopmentSetupLink(cfg, stdout); err != nil {
			stop()
			_ = waitDevelopmentServer(done)
			return 1, err
		}
	}
	_, _ = fmt.Fprintln(stdout, "Development route is ready. Ctrl-C stops only this foreground relay; the route and journal remain configured.")
	if err := <-done; err != nil && ctx.Err() == nil {
		return 1, err
	}
	return 0, nil
}

func printDevelopmentSetupLink(cfg *config.Config, stdout io.Writer) error {
	if cfg == nil || cfg.Token == "" || cfg.PhoneAppOrigin == "" || cfg.TailscaleCLIOrigin == "" {
		return errors.New("development phone setup identity is incomplete")
	}
	token, err := hex.DecodeString(cfg.Token)
	if err != nil || len(token) != 16 {
		return errors.New("development phone setup token is invalid")
	}
	phoneOrigin, err := setuphelper.NormalizeExternalHTTPSOrigin(cfg.PhoneAppOrigin)
	if err != nil || phoneOrigin != cfg.PhoneAppOrigin {
		return errors.New("development phone app origin is not canonical HTTPS")
	}
	relayOrigin, err := setuphelper.NormalizeExternalHTTPSOrigin(cfg.TailscaleCLIOrigin)
	if err != nil || relayOrigin != cfg.TailscaleCLIOrigin {
		return errors.New("development relay origin is not canonical HTTPS")
	}
	relayURL, err := url.Parse(relayOrigin)
	if err != nil || relayURL.Port() != strconv.Itoa(tailscalecli.DevelopmentHTTPSPort) {
		return errors.New("development relay origin does not use HTTPS 8443")
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "relay"
	}
	label := strings.SplitN(host, ".", 2)[0]
	fragment := setuphelper.SetupFragment(cfg.Token, label, "wss://"+strings.TrimPrefix(relayOrigin, "https://")+"/ws")
	setupURL := phoneOrigin + "/#" + fragment
	if qr, qrErr := setuphelper.TerminalQR(setupURL, 80); qrErr == nil {
		if _, err := fmt.Fprintln(stdout, "Scan this one-use, secret setup QR with the authorized owner phone:"); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(stdout, qr); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(stdout, "Owner phone setup link (secret; do not share or log):"); err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, setupURL)
	return err
}

func waitDevelopmentBackend(ctx context.Context, cfg *config.Config) error {
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/healthz", tailscalecli.DevelopmentBackendPort), nil)
		if err == nil {
			response, requestErr := client.Do(request)
			if requestErr == nil {
				var health struct {
					Status    string `json:"status"`
					Readiness string `json:"readiness"`
					Instance  string `json:"instance"`
					Origin    string `json:"tailscale_cli_origin"`
				}
				decodeErr := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&health)
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK && decodeErr == nil && health.Status == "ok" &&
					health.Readiness == "ready" && health.Instance == cfg.InstanceID && health.Origin == cfg.TailscaleCLIOrigin {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("isolated development backend did not become ready; no Serve route was published")
		case <-ticker.C:
		}
	}
}

func waitDevelopmentServer(done <-chan error) error {
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		return errors.New("development relay did not stop within the bounded shutdown period")
	}
}

func releaseDevelopmentReservation(ctx context.Context, workflow *tailscalecli.DevelopmentWorkflow, cfg *config.Config, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	report, err := workflow.Recover(ctx, cfg.InstanceID, cfg.TailscaleCLIOrigin)
	if err != nil && report.ReservationAttemptID == "" {
		return 1, err
	}
	if report.ReservationAttemptID == "" || report.ReservationState != tailscalecli.StatePublishPending || !report.ReservationReleasable {
		return 1, errors.New("no exact stopped pending development reservation is releasable")
	}
	if _, err := os.Lstat(cfg.PairingSocketPath); err == nil {
		return 1, errors.New("stop the foreground development relay before releasing its backend reservation")
	} else if !errors.Is(err, os.ErrNotExist) {
		return 1, errors.New("pairing control socket state is uncertain; reservation retained")
	}
	_, _ = fmt.Fprintf(stdout, "Type exactly to release only reservation %s:\nRELEASE BACKEND RESERVATION %s\n", report.ReservationAttemptID, report.ReservationAttemptID)
	if _, err := readRouteConfirmation(stdin, "RELEASE BACKEND RESERVATION "+report.ReservationAttemptID); err != nil {
		return 2, err
	}
	return status(workflow.ReleaseBackendPort(ctx, cfg.InstanceID, workflow.Preflight().NodeID,
		cfg.TailscaleCLIOrigin, report.ReservationAttemptID, true))
}

func unpublishDevelopmentRoute(ctx context.Context, workflow *tailscalecli.DevelopmentWorkflow, cfg *config.Config, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	preflight := workflow.Preflight()
	confirmation := fmt.Sprintf("UNPUBLISH DEVELOPMENT ROUTE node=%s origin=%s https-port=%d backend=127.0.0.1:%d",
		preflight.NodeID, preflight.Origin, tailscalecli.DevelopmentHTTPSPort, tailscalecli.DevelopmentBackendPort)
	_, _ = fmt.Fprintln(stderr, "This removes only the exact journaled route; it does not reset unrelated Serve state.")
	_, _ = fmt.Fprintln(stderr, "Separate authorization is required. The check-to-write interval is not atomic and remote connections may not drain.")
	_, _ = fmt.Fprintf(stderr, "Type exactly on stdin:\n%s\n", confirmation)
	typed, err := readRouteConfirmation(stdin, confirmation)
	if err != nil {
		return 2, err
	}
	if typed != confirmation {
		return 2, errors.New("route removal consent did not match the selected route")
	}
	consent := tailscalecli.Consent{
		Accepted: true, Scope: "development", NodeID: preflight.NodeID, Origin: preflight.Origin,
		HTTPSPort: tailscalecli.DevelopmentHTTPSPort, BackendPort: tailscalecli.DevelopmentBackendPort,
		RouteRemovalAccepted: true, CheckToWriteRaceAccepted: true, NoRemoteDrainAccepted: true,
	}
	return status(workflow.Unpublish(ctx, consent))
}

func newDevelopmentReservationID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}
