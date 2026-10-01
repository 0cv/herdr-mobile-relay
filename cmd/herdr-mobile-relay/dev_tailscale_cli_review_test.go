//go:build herdr_tailscale_test && herdr_tailscale_cli_fixture_binary

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/app"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/release"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

const (
	reviewOperationID        = "11111111111111111111111111111111"
	reviewReservationID      = "22222222222222222222222222222222"
	reviewOrigin             = "https://herdr.tailnet.ts.net:8443"
	reviewUnrelatedRoute     = `{"TCP":{"443":{"HTTPS":true}},"Web":{"other.tailnet.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8080"}}}}}`
	reviewRouteWithUnrelated = `{"TCP":{"443":{"HTTPS":true},"8443":{"HTTPS":true}},"Web":{"other.tailnet.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8080"}}},"herdr.tailnet.ts.net:8443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:18377"}}}}}`
)

func requireNativeDevelopmentFixture(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("the Go-owned development command requires native Darwin/arm64")
	}
}

func TestDevelopmentCLIRecoveryCommandsThroughPublicEntrypoint(t *testing.T) {
	requireNativeDevelopmentFixture(t)

	t.Run("reconcile-present-lost-publish-ack", func(t *testing.T) {
		fixture := newDevelopmentCommandFixture(t)
		seedDevelopmentRouteJournal(t, fixture, tailscalecli.StatePublishUncertain, true)
		confirmation := fmt.Sprintf("RECONCILE DEVELOPMENT ROUTE operation=%s reservation=%s observed=present node=node-fixture origin=%s https-port=8443 backend=127.0.0.1:18377", reviewOperationID, reviewReservationID, reviewOrigin)
		code, stdout, stderr, err := dispatchMainCommand(t, []string{"dev-tailscale-cli", "reconcile"}, confirmation+"\n")
		if err != nil || code != 0 {
			t.Fatalf("public reconcile-present returned code=%d err=%v\nstderr=%s", code, err, stderr)
		}
		assertReconciledState(t, fixture, tailscalecli.StateReconciledPresent, true)
		assertReservationState(t, fixture, tailscalecli.StateReconciledPresent, "")
		assertNoRouteMutationCalls(t, fixture)
		if strings.Contains(stdout, "setup link") || strings.Contains(stdout, "https://") {
			t.Fatalf("reconcile unexpectedly published setup material: %s", stdout)
		}
	})

	for _, journalState := range []tailscalecli.RegistrationState{
		tailscalecli.StatePublishUncertain,
		tailscalecli.StateRemoveUncertain,
	} {
		journalState := journalState
		t.Run("reconcile-present-split-"+string(journalState), func(t *testing.T) {
			fixture := newDevelopmentCommandFixture(t)
			seedDevelopmentRouteJournal(t, fixture, journalState, true)
			markReservationReconciledPresent(t, fixture)
			reservationID := ""
			if journalState == tailscalecli.StatePublishUncertain {
				reservationID = reviewReservationID
			}
			confirmation := fmt.Sprintf("RECONCILE DEVELOPMENT ROUTE operation=%s reservation=%s observed=present node=node-fixture origin=%s https-port=8443 backend=127.0.0.1:18377",
				reviewOperationID, reservationID, reviewOrigin)
			code, _, stderr, err := dispatchMainCommand(t, []string{"dev-tailscale-cli", "reconcile"}, confirmation+"\n")
			if err != nil || code != 0 {
				t.Fatalf("public reconciliation after split present write returned code=%d err=%v\nstderr=%s", code, err, stderr)
			}
			assertReconciledState(t, fixture, tailscalecli.StateReconciledPresent, true)
			assertReservationState(t, fixture, tailscalecli.StateReconciledPresent, "")
			assertNoRouteMutationCalls(t, fixture)
		})
	}

	t.Run("reconcile-absent-lost-remove-ack", func(t *testing.T) {
		fixture := newDevelopmentCommandFixture(t)
		seedDevelopmentRouteJournal(t, fixture, tailscalecli.StateRemoveUncertain, false)
		writeCommandFixtureFile(t, fixture.serveFile, "{}\n", 0o600)
		confirmation := fmt.Sprintf("RECONCILE DEVELOPMENT ROUTE operation=%s reservation= observed=absent node=node-fixture origin=%s https-port=8443 backend=127.0.0.1:18377", reviewOperationID, reviewOrigin)
		code, _, stderr, err := dispatchMainCommand(t, []string{"dev-tailscale-cli", "reconcile"}, confirmation+"\n")
		if err != nil || code != 0 {
			t.Fatalf("public reconcile-absent returned code=%d err=%v\nstderr=%s", code, err, stderr)
		}
		assertReconciledState(t, fixture, tailscalecli.StateReconciledAbsent, false)
		assertReservationAbsent(t, fixture)
		assertNoRouteMutationCalls(t, fixture)
	})

	t.Run("public-unpublish-lost-ack-then-reconcile", func(t *testing.T) {
		requireDevelopmentFixturePorts(t)
		fixture := newDevelopmentCommandFixture(t)
		seedDevelopmentRouteJournal(t, fixture, tailscalecli.StateRegistered, true)
		writeCommandFixtureFile(t, fixture.serveFile+".lose-remove-ack", "fixture\n", 0o600)
		unpublishConfirmation := "UNPUBLISH DEVELOPMENT ROUTE node=node-fixture origin=" + reviewOrigin + " https-port=8443 backend=127.0.0.1:18377"
		code, _, stderr, err := dispatchMainCommand(t, []string{"dev-tailscale-cli", "unpublish"}, unpublishConfirmation+"\n")
		if err == nil || code == 0 {
			t.Fatalf("public unpublish with lost acknowledgement returned code=%d err=%v\nstderr=%s", code, err, stderr)
		}
		assertFreshJournalState(t, fixture, tailscalecli.StateRemoveUncertain, false, "")
		assertReservationState(t, fixture, tailscalecli.StateRegistered, "")
		operationID := developmentJournalOperationID(t, fixture)
		if fixtureHasPublishedRoute(fixture) {
			t.Fatal("synthetic Serve removal did not take effect before its acknowledgement was lost")
		}
		reconcileConfirmation := fmt.Sprintf("RECONCILE DEVELOPMENT ROUTE operation=%s reservation= observed=absent node=node-fixture origin=%s https-port=8443 backend=127.0.0.1:18377", operationID, reviewOrigin)
		code, _, stderr, err = dispatchMainCommand(t, []string{"dev-tailscale-cli", "reconcile"}, reconcileConfirmation+"\n")
		if err != nil || code != 0 {
			t.Fatalf("public reconciliation after lost removal acknowledgement returned code=%d err=%v\nstderr=%s", code, err, stderr)
		}
		assertFreshJournalState(t, fixture, tailscalecli.StateReconciledAbsent, false, "")
		if developmentJournalOperationID(t, fixture) != operationID {
			t.Fatal("reconciliation did not resolve the exact unpublish operation")
		}
		assertReservationAbsent(t, fixture)
		assertServeMutationCounts(t, fixture, 0, 1)
	})

	t.Run("repair-missing-is-operation-bound-and-preserves-unrelated-route", func(t *testing.T) {
		requireDevelopmentFixturePorts(t)
		fixture := newDevelopmentCommandFixture(t)
		seedDevelopmentRouteJournal(t, fixture, tailscalecli.StateRegistered, false)
		writeCommandFixtureFile(t, fixture.serveFile, reviewUnrelatedRoute+"\n", 0o600)
		oldFactory := newDevelopmentForegroundServer
		oldReservationID := developmentReservationIDGenerator
		t.Cleanup(func() {
			newDevelopmentForegroundServer = oldFactory
			developmentReservationIDGenerator = oldReservationID
		})
		developmentReservationIDGenerator = func() (string, error) { return reviewReservationID, nil }
		var server *commandFixtureForegroundServer
		newDevelopmentForegroundServer = func(cfg *config.Config, workflow *tailscalecli.DevelopmentWorkflow, _ io.Writer) (developmentForegroundServer, error) {
			server = &commandFixtureForegroundServer{cfg: cfg, workflow: workflow, bound: make(chan struct{}), armed: make(chan struct{}), stopAfterAdmission: make(chan struct{})}
			return server, nil
		}
		before, err := os.ReadFile(filepath.Join(fixture.state, "registration.json"))
		if err != nil {
			t.Fatal(err)
		}
		wrong := fmt.Sprintf("REPAIR MISSING DEVELOPMENT ROUTE operation=%s reservation=other node=node-fixture origin=%s https-port=8443 backend=127.0.0.1:18377", reviewOperationID, reviewOrigin)
		code, _, stderr, dispatchErr := dispatchMainCommand(t, []string{"dev-tailscale-cli", "repair-missing"}, wrong+"\n")
		if code == 0 || dispatchErr != nil && !strings.Contains(dispatchErr.Error(), "route-bound confirmation") {
			t.Fatalf("repair with mismatched reservation confirmation returned code=%d err=%v stderr=%s", code, dispatchErr, stderr)
		}
		if after, readErr := os.ReadFile(filepath.Join(fixture.state, "registration.json")); readErr != nil || string(after) != string(before) {
			t.Fatalf("mismatched confirmation changed the journal: err=%v before=%s after=%s", readErr, before, after)
		}
		assertReservationState(t, fixture, tailscalecli.StateRegistered, "")
		assertNoRouteMutationCalls(t, fixture)

		fixture = newDevelopmentCommandFixture(t)
		seedDevelopmentRouteJournal(t, fixture, tailscalecli.StateRegistered, false)
		writeCommandFixtureFile(t, fixture.serveFile, reviewUnrelatedRoute+"\n", 0o600)
		server = nil
		confirmation := fmt.Sprintf("REPAIR MISSING DEVELOPMENT ROUTE operation=%s reservation=%s node=node-fixture origin=%s https-port=8443 backend=127.0.0.1:18377", reviewOperationID, reviewReservationID, reviewOrigin)
		code, stdout, stderr, dispatchErr := dispatchMainCommand(t, []string{"dev-tailscale-cli", "repair-missing"}, confirmation+"\n")
		if dispatchErr != nil || code != 0 {
			t.Fatalf("public repair-missing returned code=%d err=%v\nstderr=%s", code, dispatchErr, stderr)
		}
		if server == nil || server.admitCalls.Load() != 1 || server.armCalls.Load() != 0 {
			t.Fatalf("repair admission/arm counts = server:%v admit:%d arm:%d", server, server.admitCalls.Load(), server.armCalls.Load())
		}
		if strings.Contains(stdout, "https://") || strings.Contains(stdout, "setup link") {
			t.Fatalf("route repair emitted bootstrap material: %s", stdout)
		}
		assertFreshJournalState(t, fixture, tailscalecli.StateRegistered, true, "")
		route, err := os.ReadFile(fixture.serveFile)
		if err != nil || strings.TrimSpace(string(route)) != reviewRouteWithUnrelated {
			t.Fatalf("repair did not preserve unrelated route: err=%v route=%s", err, route)
		}
		assertSingleRoutePublish(t, fixture)
		assertRegisteredReservation(t, fixture)
	})

	t.Run("drift-refuses-repair-without-dispatch", func(t *testing.T) {
		requireDevelopmentFixturePorts(t)
		fixture := newDevelopmentCommandFixture(t)
		seedDevelopmentRouteJournal(t, fixture, tailscalecli.StateRegistered, false)
		conflicting := `{"TCP":{"8443":{"HTTPS":true}},"Web":{"other.tailnet.ts.net:8443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:18377"}}}}}`
		writeCommandFixtureFile(t, fixture.serveFile, conflicting+"\n", 0o600)
		oldFactory := newDevelopmentForegroundServer
		oldReservationID := developmentReservationIDGenerator
		newDevelopmentForegroundServer = func(cfg *config.Config, workflow *tailscalecli.DevelopmentWorkflow, _ io.Writer) (developmentForegroundServer, error) {
			return &commandFixtureForegroundServer{cfg: cfg, workflow: workflow, bound: make(chan struct{}), armed: make(chan struct{})}, nil
		}
		developmentReservationIDGenerator = func() (string, error) { return reviewReservationID, nil }
		t.Cleanup(func() {
			newDevelopmentForegroundServer = oldFactory
			developmentReservationIDGenerator = oldReservationID
		})
		before, err := os.ReadFile(filepath.Join(fixture.state, "registration.json"))
		if err != nil {
			t.Fatal(err)
		}
		confirmation := fmt.Sprintf("REPAIR MISSING DEVELOPMENT ROUTE operation=%s reservation=%s node=node-fixture origin=%s https-port=8443 backend=127.0.0.1:18377", reviewOperationID, reviewReservationID, reviewOrigin)
		code, _, stderr, err := dispatchMainCommand(t, []string{"dev-tailscale-cli", "repair-missing"}, confirmation+"\n")
		const driftRefusal = "repair requires an acknowledged or previously reconciled-absent registration and fresh proof that its exact listener and backend route are absent"
		if err == nil || code == 0 || !strings.Contains(err.Error(), driftRefusal) {
			t.Fatalf("valid operation-bound repair confirmation did not receive the drift-specific refusal: code=%d err=%v stderr=%s", code, err, stderr)
		}
		if after, readErr := os.ReadFile(filepath.Join(fixture.state, "registration.json")); readErr != nil || string(after) != string(before) {
			t.Fatalf("drift refusal changed the journal: err=%v before=%s after=%s", readErr, before, after)
		}
		assertNoRouteMutationCalls(t, fixture)
		assertJournalState(t, fixture, tailscalecli.StateRegistered, true)
		routeAfter, routeErr := os.ReadFile(fixture.serveFile)
		if routeErr != nil || strings.TrimSpace(string(routeAfter)) != conflicting {
			t.Fatalf("drift refusal changed the conflicting Serve configuration: err=%v route=%s", routeErr, routeAfter)
		}
		assertReservationState(t, fixture, tailscalecli.StateRegistered, "")
	})

	t.Run("abandon-missing-is-operation-bound", func(t *testing.T) {
		fixture := newDevelopmentCommandFixture(t)
		seedDevelopmentRouteJournal(t, fixture, tailscalecli.StateRegistered, false)
		writeCommandFixtureFile(t, fixture.serveFile, "{}\n", 0o600)
		before, err := os.ReadFile(filepath.Join(fixture.state, "registration.json"))
		if err != nil {
			t.Fatal(err)
		}
		wrong := fmt.Sprintf("ABANDON MISSING DEVELOPMENT ROUTE operation=%s reservation= node=node-fixture origin=%s https-port=8443 backend=127.0.0.1:18377", "33333333333333333333333333333333", reviewOrigin)
		code, _, stderr, dispatchErr := dispatchMainCommand(t, []string{"dev-tailscale-cli", "abandon-missing"}, wrong+"\n")
		if code == 0 || dispatchErr == nil {
			t.Fatalf("wrong-operation abandon returned code=%d err=%v stderr=%s", code, dispatchErr, stderr)
		}
		if after, readErr := os.ReadFile(filepath.Join(fixture.state, "registration.json")); readErr != nil || string(after) != string(before) {
			t.Fatalf("wrong-operation abandon changed the journal: err=%v", readErr)
		}
		assertReservationState(t, fixture, tailscalecli.StateRegistered, "")
		confirmation := fmt.Sprintf("ABANDON MISSING DEVELOPMENT ROUTE operation=%s reservation= node=node-fixture origin=%s https-port=8443 backend=127.0.0.1:18377", reviewOperationID, reviewOrigin)
		code, _, stderr, dispatchErr = dispatchMainCommand(t, []string{"dev-tailscale-cli", "abandon-missing"}, confirmation+"\n")
		if dispatchErr != nil || code != 0 {
			t.Fatalf("public abandon-missing returned code=%d err=%v stderr=%s", code, dispatchErr, stderr)
		}
		assertReconciledState(t, fixture, tailscalecli.StateReconciledAbsent, false)
		assertReservationAbsent(t, fixture)
		assertNoRouteMutationCalls(t, fixture)
	})

	// Composed lifecycle: lost publish acknowledgement -> reconcile present ->
	// unpublish whose scoped off had no effect and lost its acknowledgement ->
	// reconcile present -> successful exact unpublish.
	t.Run("reconciled-present-lost-unpublish-ack-reconciles-present", func(t *testing.T) {
		requireDevelopmentFixturePorts(t)
		fixture := newDevelopmentCommandFixture(t)
		seedDevelopmentLifecycle(t, fixture, developmentLifecycleSeed{
			journal: tailscalecli.StatePublishUncertain, journalReservationID: reviewReservationID,
			reservation: tailscalecli.StatePublishPending, reservationID: reviewReservationID,
			serve: reviewRouteWithUnrelated,
		})
		mustDispatchDevelopmentCommand(t, "reconcile", reviewReconcileConfirmation(reviewOperationID, reviewReservationID, "present"))
		assertDevelopmentJournal(t, fixture, tailscalecli.StateReconciledPresent, reviewOperationID, reviewReservationID, false)
		assertReservationState(t, fixture, tailscalecli.StateReconciledPresent, "")

		writeCommandFixtureFile(t, fixture.serveFile+".remove-no-effect", "fixture\n", 0o600)
		writeCommandFixtureFile(t, fixture.serveFile+".lose-remove-ack", "fixture\n", 0o600)
		code, _, stderr, err := dispatchMainCommand(t, []string{"dev-tailscale-cli", "unpublish"}, reviewUnpublishConfirmation+"\n")
		if err == nil || code == 0 {
			t.Fatalf("unpublish with lost acknowledgement returned code=%d err=%v\nstderr=%s", code, err, stderr)
		}
		removalOperation := assertFreshRemovalJournal(t, fixture, reviewOperationID)
		assertReservationState(t, fixture, tailscalecli.StateReconciledPresent, "")
		assertServeFixture(t, fixture, reviewRouteWithUnrelated)

		mustDispatchDevelopmentCommand(t, "reconcile", reviewReconcileConfirmation(removalOperation, "", "present"))
		assertDevelopmentJournal(t, fixture, tailscalecli.StateReconciledPresent, removalOperation, "", false)
		assertReservationState(t, fixture, tailscalecli.StateReconciledPresent, "")
		assertServeMutationCounts(t, fixture, 0, 1)

		for _, flag := range []string{".remove-no-effect", ".lose-remove-ack"} {
			if err := os.Remove(fixture.serveFile + flag); err != nil {
				t.Fatal(err)
			}
		}
		mustDispatchDevelopmentCommand(t, "unpublish", reviewUnpublishConfirmation)
		if record := readDevelopmentJournal(t, fixture); record.State != tailscalecli.StateRemoved || !record.MutationAcknowledged {
			t.Fatalf("exact unpublish after reconciliation = %+v", record)
		}
		assertReservationAbsent(t, fixture)
		assertServeFixture(t, fixture, reviewUnrelatedRoute)
		assertServeMutationCounts(t, fixture, 0, 2)
	})

	t.Run("reconciled-present-route-disappeared-is-abandonable", func(t *testing.T) {
		requireDevelopmentFixturePorts(t)
		fixture := newDevelopmentCommandFixture(t)
		seedDevelopmentLifecycle(t, fixture, developmentLifecycleSeed{
			journal: tailscalecli.StatePublishUncertain, journalReservationID: reviewReservationID,
			reservation: tailscalecli.StatePublishPending, reservationID: reviewReservationID,
			serve: reviewRouteWithUnrelated,
		})
		mustDispatchDevelopmentCommand(t, "reconcile", reviewReconcileConfirmation(reviewOperationID, reviewReservationID, "present"))
		assertDevelopmentJournal(t, fixture, tailscalecli.StateReconciledPresent, reviewOperationID, reviewReservationID, false)
		// The operator removes the route outside Herdr; unrelated Serve state remains.
		writeCommandFixtureFile(t, fixture.serveFile, reviewUnrelatedRoute+"\n", 0o600)
		code, report := dispatchDevelopmentStatus(t)
		if code == 0 || report.Route.JournalState != tailscalecli.StateReconciledPresent ||
			report.Observation != "selected-listener-absent" || report.OperationID != reviewOperationID ||
			report.ReservationState != tailscalecli.StateReconciledPresent || report.ReservationAttemptID != reviewReservationID ||
			!report.RequiresOperatorAction {
			t.Fatalf("status after reconciled-present route disappeared = code %d report %+v", code, report)
		}
		mustDispatchDevelopmentCommand(t, "abandon-missing", reviewAbandonConfirmation(reviewOperationID, reviewReservationID))
		assertDevelopmentJournal(t, fixture, tailscalecli.StateReconciledAbsent, reviewOperationID, reviewReservationID, false)
		assertReservationAbsent(t, fixture)
		assertServeFixture(t, fixture, reviewUnrelatedRoute)
		assertNoRouteMutationCalls(t, fixture)
	})

	for _, reservationState := range []tailscalecli.RegistrationState{
		tailscalecli.StateRegistered,
		tailscalecli.StateReconciledPresent,
	} {
		reservationState := reservationState
		t.Run("removed-journal-retained-"+string(reservationState)+"-reservation-is-abandonable", func(t *testing.T) {
			requireDevelopmentFixturePorts(t)
			fixture := newDevelopmentCommandFixture(t)
			// Unpublish verified the removal and journaled it, but releasing the
			// shared reservation failed afterwards.
			seedDevelopmentLifecycle(t, fixture, developmentLifecycleSeed{
				journal: tailscalecli.StateRemoved, reservation: reservationState, serve: reviewUnrelatedRoute,
			})
			code, report := dispatchDevelopmentStatus(t)
			if code == 0 || report.Route.JournalState != tailscalecli.StateRemoved ||
				report.Observation != "selected-listener-absent" || report.OperationID != reviewOperationID ||
				report.ReservationState != reservationState || report.ReservationAttemptID != "" || !report.RequiresOperatorAction {
				t.Fatalf("status for removed journal with retained reservation = code %d report %+v", code, report)
			}
			before, err := os.ReadFile(filepath.Join(fixture.state, "registration.json"))
			if err != nil {
				t.Fatal(err)
			}
			mustDispatchDevelopmentCommand(t, "abandon-missing", reviewAbandonConfirmation(reviewOperationID, ""))
			if after, err := os.ReadFile(filepath.Join(fixture.state, "registration.json")); err != nil || string(after) != string(before) {
				t.Fatalf("releasing the retained reservation rewrote the removal receipt: err=%v", err)
			}
			assertReservationAbsent(t, fixture)
			assertServeFixture(t, fixture, reviewUnrelatedRoute)
			assertNoRouteMutationCalls(t, fixture)
		})
	}

	t.Run("reconciled-absent-retained-reconciled-present-reservation-is-abandonable", func(t *testing.T) {
		requireDevelopmentFixturePorts(t)
		fixture := newDevelopmentCommandFixture(t)
		// An absent reconciliation journaled its result before the reconciled-
		// present reservation of its source registration could be released.
		seedDevelopmentLifecycle(t, fixture, developmentLifecycleSeed{
			journal: tailscalecli.StateReconciledAbsent, journalReservationID: reviewReservationID,
			reservation: tailscalecli.StateReconciledPresent, serve: reviewUnrelatedRoute,
		})
		before, err := os.ReadFile(filepath.Join(fixture.state, "registration.json"))
		if err != nil {
			t.Fatal(err)
		}
		mustDispatchDevelopmentCommand(t, "abandon-missing", reviewAbandonConfirmation(reviewOperationID, ""))
		if after, err := os.ReadFile(filepath.Join(fixture.state, "registration.json")); err != nil || string(after) != string(before) {
			t.Fatalf("releasing the retained reservation rewrote the reconciled-absent journal: err=%v", err)
		}
		assertReservationAbsent(t, fixture)
		assertServeFixture(t, fixture, reviewUnrelatedRoute)
		assertNoRouteMutationCalls(t, fixture)
	})

	for _, journalState := range []tailscalecli.RegistrationState{
		tailscalecli.StatePublishUncertain,
		tailscalecli.StateRemoveUncertain,
	} {
		journalState := journalState
		t.Run("reconcile-absent-after-split-present-write-"+string(journalState), func(t *testing.T) {
			requireDevelopmentFixturePorts(t)
			fixture := newDevelopmentCommandFixture(t)
			seedDevelopmentRouteJournal(t, fixture, journalState, false)
			markReservationReconciledPresent(t, fixture)
			// The route disappeared before the interrupted present reconciliation
			// could be completed.
			writeCommandFixtureFile(t, fixture.serveFile, reviewUnrelatedRoute+"\n", 0o600)
			reservationID := ""
			if journalState == tailscalecli.StatePublishUncertain {
				reservationID = reviewReservationID
			}
			mustDispatchDevelopmentCommand(t, "reconcile", reviewReconcileConfirmation(reviewOperationID, reservationID, "absent"))
			assertReconciledState(t, fixture, tailscalecli.StateReconciledAbsent, false)
			assertReservationAbsent(t, fixture)
			assertServeFixture(t, fixture, reviewUnrelatedRoute)
			assertNoRouteMutationCalls(t, fixture)
		})
	}

	t.Run("registered-receipt-with-unpromoted-reservation-is-abandonable", func(t *testing.T) {
		requireDevelopmentFixturePorts(t)
		fixture := newDevelopmentCommandFixture(t)
		// Publish committed its acknowledged receipt, but promoting the pending
		// reservation failed; the route later disappeared.
		seedDevelopmentLifecycle(t, fixture, developmentLifecycleSeed{
			journal: tailscalecli.StateRegistered, reservation: tailscalecli.StatePublishPending,
			reservationID: reviewReservationID, serve: reviewUnrelatedRoute,
		})
		mustDispatchDevelopmentCommand(t, "abandon-missing", reviewAbandonConfirmation(reviewOperationID, reviewReservationID))
		assertDevelopmentJournal(t, fixture, tailscalecli.StateReconciledAbsent, reviewOperationID, "", false)
		assertReservationAbsent(t, fixture)
		assertServeFixture(t, fixture, reviewUnrelatedRoute)
		assertNoRouteMutationCalls(t, fixture)
	})

	t.Run("registered-receipt-with-unpromoted-reservation-recovers-lost-unpublish-ack", func(t *testing.T) {
		requireDevelopmentFixturePorts(t)
		fixture := newDevelopmentCommandFixture(t)
		seedDevelopmentLifecycle(t, fixture, developmentLifecycleSeed{
			journal: tailscalecli.StateRegistered, reservation: tailscalecli.StatePublishPending,
			reservationID: reviewReservationID, serve: reviewRouteWithUnrelated,
		})
		writeCommandFixtureFile(t, fixture.serveFile+".lose-remove-ack", "fixture\n", 0o600)
		code, _, stderr, err := dispatchMainCommand(t, []string{"dev-tailscale-cli", "unpublish"}, reviewUnpublishConfirmation+"\n")
		if err == nil || code == 0 {
			t.Fatalf("unpublish with lost acknowledgement returned code=%d err=%v\nstderr=%s", code, err, stderr)
		}
		removalOperation := assertFreshRemovalJournal(t, fixture, reviewOperationID)
		// The acknowledged receipt's reservation was promoted before dispatch.
		assertReservationState(t, fixture, tailscalecli.StateRegistered, "")
		assertServeFixture(t, fixture, reviewUnrelatedRoute)
		mustDispatchDevelopmentCommand(t, "reconcile", reviewReconcileConfirmation(removalOperation, "", "absent"))
		assertDevelopmentJournal(t, fixture, tailscalecli.StateReconciledAbsent, removalOperation, "", false)
		assertReservationAbsent(t, fixture)
		assertServeFixture(t, fixture, reviewUnrelatedRoute)
		assertServeMutationCounts(t, fixture, 0, 1)
	})

	t.Run("cleared-unconfigured-journal-releases-later-setup-reservation", func(t *testing.T) {
		requireDevelopmentFixturePorts(t)
		fixture := newDevelopmentCommandFixture(t)
		// A non-dispatched publication left its attempt-bound unconfigured
		// journal and pending reservation.
		seedDevelopmentLifecycle(t, fixture, developmentLifecycleSeed{
			journal: tailscalecli.StateUnconfigured, journalReservationID: reviewReservationID,
			reservation: tailscalecli.StatePublishPending, reservationID: reviewReservationID,
		})
		mustDispatchDevelopmentCommand(t, "release-reservation", "RELEASE BACKEND RESERVATION "+reviewReservationID)
		assertDevelopmentJournal(t, fixture, tailscalecli.StateUnconfigured, reviewOperationID, "", false)
		assertReservationAbsent(t, fixture)

		const laterReservationID = "44444444444444444444444444444444"
		oldFactory := newDevelopmentForegroundServer
		oldReservationID := developmentReservationIDGenerator
		t.Cleanup(func() {
			newDevelopmentForegroundServer = oldFactory
			developmentReservationIDGenerator = oldReservationID
		})
		developmentReservationIDGenerator = func() (string, error) { return laterReservationID, nil }
		constructionFailure := errors.New("fixture relay construction failed before publication")
		newDevelopmentForegroundServer = func(*config.Config, *tailscalecli.DevelopmentWorkflow, io.Writer) (developmentForegroundServer, error) {
			return nil, constructionFailure
		}
		setupConfirmation := tailscalecli.PublishRouteConfirmation("node-fixture", reviewOrigin, 8443, 18377)
		code, _, stderr, err := dispatchMainCommand(t, []string{"dev-tailscale-cli", "setup"}, setupConfirmation+"\n")
		if code == 0 || !errors.Is(err, constructionFailure) {
			t.Fatalf("setup with failing relay construction returned code=%d err=%v\nstderr=%s", code, err, stderr)
		}
		if strings.Contains(stderr, "Backend reservation retained") {
			t.Fatalf("setup could not release its own unpublished reservation: %s", stderr)
		}
		assertReservationAbsent(t, fixture)
		assertDevelopmentJournal(t, fixture, tailscalecli.StateUnconfigured, reviewOperationID, "", false)
		assertNoRouteMutationCalls(t, fixture)
	})

	t.Run("interrupted-unconfigured-release-then-failed-setup-releases-new-attempt", func(t *testing.T) {
		requireDevelopmentFixturePorts(t)
		fixture := newDevelopmentCommandFixture(t)
		// An earlier release deleted attempt A's reservation but was interrupted
		// before it cleared the unconfigured journal's attempt binding.
		seedDevelopmentLifecycle(t, fixture, developmentLifecycleSeed{
			journal: tailscalecli.StateUnconfigured, journalReservationID: reviewReservationID,
			serve: reviewUnrelatedRoute,
		})
		const laterReservationID = "44444444444444444444444444444444"
		oldFactory := newDevelopmentForegroundServer
		oldReservationID := developmentReservationIDGenerator
		t.Cleanup(func() {
			newDevelopmentForegroundServer = oldFactory
			developmentReservationIDGenerator = oldReservationID
		})
		developmentReservationIDGenerator = func() (string, error) { return laterReservationID, nil }
		constructionFailure := errors.New("fixture relay construction failed before publication")
		newDevelopmentForegroundServer = func(*config.Config, *tailscalecli.DevelopmentWorkflow, io.Writer) (developmentForegroundServer, error) {
			// Setup B holds its own pending reservation; the stale binding was
			// finalized under the shared lock before B was claimed.
			assertDevelopmentJournal(t, fixture, tailscalecli.StateUnconfigured, reviewOperationID, "", false)
			assertReservationState(t, fixture, tailscalecli.StatePublishPending, laterReservationID)
			return nil, constructionFailure
		}
		setupConfirmation := tailscalecli.PublishRouteConfirmation("node-fixture", reviewOrigin, 8443, 18377)
		code, _, stderr, err := dispatchMainCommand(t, []string{"dev-tailscale-cli", "setup"}, setupConfirmation+"\n")
		if code == 0 || !errors.Is(err, constructionFailure) {
			t.Fatalf("setup with failing relay construction returned code=%d err=%v\nstderr=%s", code, err, stderr)
		}
		if strings.Contains(stderr, "Backend reservation retained") {
			t.Fatalf("setup could not release its own unpublished reservation: %s", stderr)
		}
		assertReservationAbsent(t, fixture)
		assertDevelopmentJournal(t, fixture, tailscalecli.StateUnconfigured, reviewOperationID, "", false)
		assertServeFixture(t, fixture, reviewUnrelatedRoute)
		assertNoRouteMutationCalls(t, fixture)
		if statusCode, report := dispatchDevelopmentStatus(t); statusCode != 0 || report.ReservationState != "" || report.RequiresOperatorAction {
			t.Fatalf("status after releasing the later attempt = code %d report %+v", statusCode, report)
		}
	})

	t.Run("stale-unconfigured-attempt-beside-later-reservation-is-releasable", func(t *testing.T) {
		requireDevelopmentFixturePorts(t)
		fixture := newDevelopmentCommandFixture(t)
		// The stranded pair an earlier binary could leave: journal bound to
		// released attempt A beside later setup B's pending reservation.
		const laterReservationID = "44444444444444444444444444444444"
		seedDevelopmentLifecycle(t, fixture, developmentLifecycleSeed{
			journal: tailscalecli.StateUnconfigured, journalReservationID: reviewReservationID,
			reservation: tailscalecli.StatePublishPending, reservationID: laterReservationID,
			serve: reviewUnrelatedRoute,
		})
		code, report := dispatchDevelopmentStatus(t)
		if code == 0 || report.ReservationState != tailscalecli.StatePublishPending ||
			report.ReservationAttemptID != laterReservationID || !report.ReservationReleasable {
			t.Fatalf("status for stale binding beside later reservation = code %d report %+v", code, report)
		}
		mustDispatchDevelopmentCommand(t, "release-reservation", "RELEASE BACKEND RESERVATION "+laterReservationID)
		assertDevelopmentJournal(t, fixture, tailscalecli.StateUnconfigured, reviewOperationID, "", false)
		assertReservationAbsent(t, fixture)
		assertServeFixture(t, fixture, reviewUnrelatedRoute)
		assertNoRouteMutationCalls(t, fixture)
	})

	t.Run("release-interrupted-after-journal-cleanup-is-retryable", func(t *testing.T) {
		requireDevelopmentFixturePorts(t)
		fixture := newDevelopmentCommandFixture(t)
		// Release clears the journal binding before deleting the reservation.
		seedDevelopmentLifecycle(t, fixture, developmentLifecycleSeed{
			journal:     tailscalecli.StateUnconfigured,
			reservation: tailscalecli.StatePublishPending, reservationID: reviewReservationID,
			serve: reviewUnrelatedRoute,
		})
		mustDispatchDevelopmentCommand(t, "release-reservation", "RELEASE BACKEND RESERVATION "+reviewReservationID)
		assertDevelopmentJournal(t, fixture, tailscalecli.StateUnconfigured, reviewOperationID, "", false)
		assertReservationAbsent(t, fixture)
		assertServeFixture(t, fixture, reviewUnrelatedRoute)
		assertNoRouteMutationCalls(t, fixture)
	})
}

func TestDevelopmentCLIForegroundSetupIntegration(t *testing.T) {
	requireNativeDevelopmentFixture(t)
	for _, name := range []string{"success", "consent-cancellation", "ambiguous-publication", "lost-publish-and-unpublish-acknowledgements"} {
		name := name
		t.Run(name, func(t *testing.T) {
			fixture := newDevelopmentCommandFixture(t)
			oldReservationIDGenerator := developmentReservationIDGenerator
			developmentReservationIDGenerator = func() (string, error) { return reviewReservationID, nil }
			t.Cleanup(func() { developmentReservationIDGenerator = oldReservationIDGenerator })
			setDevelopmentReleaseIdentity(t, "0.9.0", "review-fixture-revision")
			webRoot := filepath.Join(fixture.root, "current", "web")
			writeExactWebFixture(t, webRoot, "0.9.0", "review-fixture-revision")
			setDevelopmentFixturePhoneOrigin(t, fixture, reviewOrigin)
			requireDevelopmentFixturePorts(t)
			if name == "consent-cancellation" {
				before, err := os.ReadFile(fixture.relayEnv)
				if err != nil {
					t.Fatal(err)
				}
				code, stdout, stderr, runErr := dispatchMainCommand(t, []string{"dev-tailscale-cli", "setup"}, "NO\n")
				if code == 0 || runErr == nil || !strings.Contains(runErr.Error(), "route-bound confirmation") {
					t.Fatalf("cancelled setup returned code=%d err=%v stderr=%s", code, runErr, stderr)
				}
				if after, readErr := os.ReadFile(fixture.relayEnv); readErr != nil || string(after) != string(before) {
					t.Fatalf("cancelled setup changed relay.env: err=%v", readErr)
				}
				if strings.Contains(stdout, "https://") || fileExists(filepath.Join(fixture.state, "registration.json")) {
					t.Fatalf("cancelled setup emitted a link or journal: stdout=%s", stdout)
				}
				assertNoRouteMutationCalls(t, fixture)
				return
			}

			if name == "ambiguous-publication" || name == "lost-publish-and-unpublish-acknowledgements" {
				composed := name == "lost-publish-and-unpublish-acknowledgements"
				if composed {
					writeCommandFixtureFile(t, fixture.serveFile, reviewUnrelatedRoute+"\n", 0o600)
				}
				writeCommandFixtureFile(t, fixture.serveFile+".lose-ack", "fixture\n", 0o600)
				var callbackCount atomic.Int32
				installFixtureAppFactory(t, nil, nil, func(string) { callbackCount.Add(1) })
				input := tailscalecli.PublishRouteConfirmation("node-fixture", reviewOrigin, 8443, 18377) + "\n"
				code, stdout, stderr, err := dispatchMainCommand(t, []string{"dev-tailscale-cli", "setup"}, input)
				if err == nil || code == 0 {
					t.Fatalf("ambiguous publication returned code=%d err=%v stderr=%s", code, err, stderr)
				}
				assertFreshJournalState(t, fixture, tailscalecli.StatePublishUncertain, false, reviewReservationID)
				assertReservationState(t, fixture, tailscalecli.StatePublishPending, reviewReservationID)
				assertSingleRoutePublish(t, fixture)
				if callbackCount.Load() != 0 || strings.Contains(stdout, "https://") || strings.Contains(stdout, "setup link") {
					t.Fatalf("ambiguous publication admitted/armed or emitted setup link: callbacks=%d stdout=%s", callbackCount.Load(), stdout)
				}
				if fileExists(filepath.Join(fixture.root, "runtime", "device-auth")) {
					t.Fatal("ambiguous publication created a device/bootstrap store without readiness")
				}
				operationID := developmentJournalOperationID(t, fixture)
				if !fixtureHasPublishedRoute(fixture) {
					t.Fatal("lost publish acknowledgement did not leave the synthetic exact route present")
				}
				reconcileConfirmation := fmt.Sprintf("RECONCILE DEVELOPMENT ROUTE operation=%s reservation=%s observed=present node=node-fixture origin=%s https-port=8443 backend=127.0.0.1:18377", operationID, reviewReservationID, reviewOrigin)
				code, recoveryStdout, recoveryStderr, recoveryErr := dispatchMainCommand(t, []string{"dev-tailscale-cli", "reconcile"}, reconcileConfirmation+"\n")
				if recoveryErr != nil || code != 0 {
					t.Fatalf("public reconciliation after lost publish acknowledgement returned code=%d err=%v\nstderr=%s", code, recoveryErr, recoveryStderr)
				}
				assertFreshJournalState(t, fixture, tailscalecli.StateReconciledPresent, false, reviewReservationID)
				if developmentJournalOperationID(t, fixture) != operationID {
					t.Fatal("reconciliation did not resolve the exact setup operation")
				}
				assertReservationState(t, fixture, tailscalecli.StateReconciledPresent, "")
				assertServeMutationCounts(t, fixture, 1, 0)
				if strings.Contains(recoveryStdout, "setup link") || strings.Contains(recoveryStdout, "https://") {
					t.Fatalf("reconciliation emitted setup material: %s", recoveryStdout)
				}
				if !composed {
					return
				}

				// The reconciled-present route is then unpublished: the scoped off
				// takes effect but its acknowledgement is lost.
				assertServeFixture(t, fixture, reviewRouteWithUnrelated)
				writeCommandFixtureFile(t, fixture.serveFile+".lose-remove-ack", "fixture\n", 0o600)
				code, _, unpublishStderr, unpublishErr := dispatchMainCommand(t, []string{"dev-tailscale-cli", "unpublish"}, reviewUnpublishConfirmation+"\n")
				if unpublishErr == nil || code == 0 {
					t.Fatalf("unpublish with lost acknowledgement returned code=%d err=%v\nstderr=%s", code, unpublishErr, unpublishStderr)
				}
				removalOperation := assertFreshRemovalJournal(t, fixture, operationID)
				// The shared reservation keeps its pre-removal reconciled-present
				// attribution; the removal is not bound to the publication attempt.
				assertReservationState(t, fixture, tailscalecli.StateReconciledPresent, "")
				assertServeFixture(t, fixture, reviewUnrelatedRoute)
				statusCode, report := dispatchDevelopmentStatus(t)
				if statusCode == 0 || report.Route.JournalState != tailscalecli.StateRemoveUncertain ||
					report.Observation != "selected-listener-absent" || report.OperationID != removalOperation ||
					report.ReservationState != tailscalecli.StateReconciledPresent || report.ReservationAttemptID != "" ||
					!report.RequiresOperatorAction {
					t.Fatalf("status after lost removal acknowledgement = code %d report %+v", statusCode, report)
				}
				mustDispatchDevelopmentCommand(t, "reconcile", reviewReconcileConfirmation(removalOperation, "", "absent"))
				assertDevelopmentJournal(t, fixture, tailscalecli.StateReconciledAbsent, removalOperation, "", false)
				assertReservationAbsent(t, fixture)
				assertServeFixture(t, fixture, reviewUnrelatedRoute)
				// Neither reconciliation replayed a Serve mutation.
				assertServeMutationCounts(t, fixture, 1, 1)
				statusCode, report = dispatchDevelopmentStatus(t)
				if statusCode != 0 || report.Route.JournalState != tailscalecli.StateReconciledAbsent ||
					report.ReservationState != "" || report.RequiresOperatorAction {
					t.Fatalf("status after absent removal reconciliation = code %d report %+v", statusCode, report)
				}
				return
			}

			server := startTrustedDevelopmentHTTPSFixture(t, fixture, webRoot)
			var eventMu sync.Mutex
			var events []string
			healthChecks := &server.healthRequests
			var bundleChecks atomic.Int32
			bundleEntered := make(chan struct{})
			releaseBundle := make(chan struct{})
			var releaseBundleOnce sync.Once
			releaseReadinessBundle := func() { releaseBundleOnce.Do(func() { close(releaseBundle) }) }
			t.Cleanup(releaseReadinessBundle)
			factory := func(cfg *config.Config, workflow *tailscalecli.DevelopmentWorkflow, _ io.Writer) (developmentForegroundServer, error) {
				return app.NewDevelopmentCLIWithFixtureHooks(cfg, version, revision,
					slog.New(slog.NewTextHandler(io.Discard, nil)), workflow,
					app.DevelopmentCLIFixtureHooks{
						HealthClient:       func(time.Duration) *http.Client { return server.client() },
						MarkInventoryReady: true,
						VerifyPublicBundle: func(ctx context.Context, gotRoot, gotOrigin, gotVersion, gotRevision string) error {
							if gotRoot != webRoot || gotOrigin != reviewOrigin || gotVersion != version || gotRevision != revision {
								return fmt.Errorf("bundle hook tuple changed: root=%q origin=%q version=%q revision=%q", gotRoot, gotOrigin, gotVersion, gotRevision)
							}
							if err := verifyDevelopmentFixtureListenersHeld(); err != nil {
								return err
							}
							if bundleChecks.Add(1) == 1 {
								close(bundleEntered)
								<-releaseBundle
							}
							return verifyTrustedExactPublishedBundle(ctx, server.client(), gotRoot, gotOrigin, gotVersion, gotRevision)
						},
						ControlCallbackObserver: func(name string) {
							eventMu.Lock()
							events = append(events, name)
							eventMu.Unlock()
						},
					})
			}
			oldFactory := newDevelopmentForegroundServer
			oldContextFactory := developmentCLICommandContext
			newDevelopmentForegroundServer = factory
			commandContext, cancel := context.WithCancel(context.Background())
			developmentCLICommandContext = func() (context.Context, context.CancelFunc) { return commandContext, cancel }
			t.Cleanup(func() {
				newDevelopmentForegroundServer = oldFactory
				developmentCLICommandContext = oldContextFactory
			})
			input := tailscalecli.PublishRouteConfirmation("node-fixture", reviewOrigin, 8443, 18377) + "\n"
			result, stdoutPath := startDevelopmentCommandDispatch(t, []string{"dev-tailscale-cli", "setup"}, input)
			commandDone := false
			t.Cleanup(func() {
				cancel()
				releaseReadinessBundle()
				if commandDone {
					return
				}
				select {
				case <-result:
				case <-time.After(10 * time.Second):
					t.Error("public setup command did not stop during fixture cleanup")
				}
			})
			select {
			case <-bundleEntered:
			case result := <-result:
				commandDone = true
				t.Fatalf("public setup exited before readiness bundle verification: code=%d err=%v stderr=%s", result.code, result.err, result.stderr)
			case <-time.After(20 * time.Second):
				cancel()
				t.Fatal("public setup did not reach the trusted bundle verification hook")
			}
			outputBeforeReadiness := readCommandFixtureOutput(t, stdoutPath)
			if strings.Contains(outputBeforeReadiness, "https://") || strings.Contains(outputBeforeReadiness, "setup link") {
				cancel()
				t.Fatalf("setup link appeared before readiness completed: %s", outputBeforeReadiness)
			}
			if fileExists(filepath.Join(fixture.root, "runtime", "device-auth")) {
				cancel()
				t.Fatal("device/bootstrap store was created before trusted HTTPS and exact bundle readiness")
			}
			eventMu.Lock()
			eventsBeforeReadiness := append([]string(nil), events...)
			eventMu.Unlock()
			if len(eventsBeforeReadiness) != 1 || eventsBeforeReadiness[0] != "admit" {
				cancel()
				t.Fatalf("bootstrap arm occurred before successful readiness: events=%v", eventsBeforeReadiness)
			}
			if healthChecks.Load() == 0 || bundleChecks.Load() == 0 {
				cancel()
				t.Fatalf("readiness did not verify trusted HTTPS and exact bundle: health=%d bundle=%d", healthChecks.Load(), bundleChecks.Load())
			}
			releaseReadinessBundle()
			waitForDevelopmentCommandOutput(t, stdoutPath, "Owner phone setup link")
			outputAfterReadiness := readCommandFixtureOutput(t, stdoutPath)
			if !strings.Contains(outputAfterReadiness, "Owner phone setup link") || !strings.Contains(outputAfterReadiness, "https://") {
				cancel()
				t.Fatalf("successful public setup omitted its post-readiness link: %s", outputAfterReadiness)
			}
			if !fileExists(filepath.Join(fixture.root, "runtime", "device-auth")) {
				cancel()
				t.Fatal("successful readiness and arm did not persist the local bootstrap/device store")
			}
			if err := verifyTrustedExactPublishedBundle(context.Background(), server.client(), webRoot, reviewOrigin, version, revision); err != nil {
				cancel()
				t.Fatalf("fixture did not retain trusted exact public release before shutdown: %v", err)
			}
			cancel()
			select {
			case done := <-result:
				commandDone = true
				if done.code != 0 || done.err != nil {
					t.Fatalf("public setup completed code=%d err=%v stderr=%s", done.code, done.err, done.stderr)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("public setup did not stop after command context cancellation")
			}
			eventMu.Lock()
			finalEvents := append([]string(nil), events...)
			eventMu.Unlock()
			if len(finalEvents) < 2 || finalEvents[0] != "admit" || finalEvents[1] != "arm_bootstrap" {
				t.Fatalf("foreground control order = %v", finalEvents)
			}
			assertFreshJournalState(t, fixture, tailscalecli.StateRegistered, true, "")
			assertReservationState(t, fixture, tailscalecli.StateRegistered, "")
			assertSingleRoutePublish(t, fixture)
		})
	}
}

type developmentCommandDispatchResult struct {
	code           int
	stdout, stderr string
	err            error
}

func startDevelopmentCommandDispatch(t *testing.T, args []string, input string) (<-chan developmentCommandDispatchResult, string) {
	t.Helper()
	stdin, err := os.CreateTemp(t.TempDir(), "review-stdin-")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	stdout, err := os.CreateTemp(t.TempDir(), "review-stdout-")
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.CreateTemp(t.TempDir(), "review-stderr-")
	if err != nil {
		t.Fatal(err)
	}
	oldStdin, oldStdout, oldStderr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = stdin, stdout, stderr
	finished := make(chan developmentCommandDispatchResult, 1)
	go func() {
		code, runErr := run(args)
		if runErr != nil {
			reportError(os.Stderr, args, runErr)
		}
		read := func(file *os.File) string {
			_, _ = file.Seek(0, io.SeekStart)
			data, _ := io.ReadAll(file)
			return string(data)
		}
		result := developmentCommandDispatchResult{code: code, stdout: read(stdout), stderr: read(stderr), err: runErr}
		os.Stdin, os.Stdout, os.Stderr = oldStdin, oldStdout, oldStderr
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		finished <- result
	}()
	return finished, stdout.Name()
}

func readCommandFixtureOutput(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func waitForDevelopmentCommandOutput(t *testing.T, path, substring string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(readCommandFixtureOutput(t, path), substring) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("command output did not include %q: %s", substring, readCommandFixtureOutput(t, path))
}

func setDevelopmentFixturePhoneOrigin(t *testing.T, fixture *developmentCommandFixture, origin string) {
	t.Helper()
	data, err := os.ReadFile(fixture.relayEnv)
	if err != nil {
		t.Fatal(err)
	}
	oldValue := "HERDR_PHONE_APP_URL='https://app.example.test'"
	newValue := "HERDR_PHONE_APP_URL='" + origin + "'"
	if !strings.Contains(string(data), oldValue) {
		t.Fatalf("private fixture relay.env is missing phone-app origin setting: %s", data)
	}
	writeCommandFixtureFile(t, fixture.relayEnv, strings.Replace(string(data), oldValue, newValue, 1), 0o600)
	t.Setenv("HERDR_PHONE_APP_URL", origin)
}

func setDevelopmentReleaseIdentity(t *testing.T, nextVersion, nextRevision string) {
	t.Helper()
	oldVersion, oldRevision := version, revision
	version, revision = nextVersion, nextRevision
	t.Cleanup(func() { version, revision = oldVersion, oldRevision })
}

func writeExactWebFixture(t *testing.T, root, releaseVersion, releaseRevision string) {
	t.Helper()
	js := []byte("export const reviewFixture = true;\n")
	css := []byte("body { color: rgb(17, 34, 51); }\n")
	jsHash := sha256.Sum256(js)
	cssHash := sha256.Sum256(css)
	jsHex, cssHex := hex.EncodeToString(jsHash[:]), hex.EncodeToString(cssHash[:])
	entryPath := "builds/" + releaseVersion + "-384-abcdef0123456789/index.html"
	jsPath, cssPath := "assets/app-"+jsHex+".js", "assets/app-"+cssHex+".css"
	integrity := func(sum []byte) string { return "sha256-" + base64.StdEncoding.EncodeToString(sum) }
	entry := []byte(fmt.Sprintf("<!doctype html><html><head><link rel=\"stylesheet\" href=\"/%s\" integrity=\"%s\"></head><body><script type=\"module\" src=\"/%s\" integrity=\"%s\"></script></body></html>\n",
		cssPath, integrity(cssHash[:]), jsPath, integrity(jsHash[:])))
	entryHash := sha256.Sum256(entry)
	files := map[string]release.WebDescriptorFile{
		"entry":      {Path: entryPath, SHA256: hex.EncodeToString(entryHash[:]), Integrity: integrity(entryHash[:])},
		"javascript": {Path: jsPath, SHA256: jsHex, Integrity: integrity(jsHash[:])},
		"stylesheet": {Path: cssPath, SHA256: cssHex, Integrity: integrity(cssHash[:])},
	}
	buildHash := sha256.Sum256([]byte("review native foreground bundle"))
	descriptor := release.WebDescriptor{
		Schema: release.WebDescriptorSchema, Version: releaseVersion, Assets: 384,
		Build: hex.EncodeToString(buildHash[:]), Entry: "/" + entryPath, Files: files,
	}
	descriptorData, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	versionData := []byte(fmt.Sprintf("{\"release_version\":%q,\"revision\":%q}\n", releaseVersion, releaseRevision))
	for path, data := range map[string][]byte{
		"version.json": versionData, "release.json": append(descriptorData, '\n'),
		entryPath: entry, jsPath: js, cssPath: css,
	} {
		writeCommandFixtureFile(t, filepath.Join(root, filepath.FromSlash(path)), string(data), 0o600)
	}
	if _, err := release.VerifyWebDescriptor(os.DirFS(root), releaseVersion); err != nil {
		t.Fatalf("constructed web fixture is invalid: %v", err)
	}
}

func verifyDevelopmentFixtureListenersHeld() error {
	backend, err := net.Listen("tcp", "127.0.0.1:18377")
	if err == nil {
		_ = backend.Close()
		return errors.New("foreground backend TCP listener was not held through readiness")
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("probe foreground backend TCP listener: %w", err)
	}
	plugin, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 18378})
	if err == nil {
		_ = plugin.Close()
		return errors.New("foreground plugin UDP listener was not held through readiness")
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		return fmt.Errorf("probe foreground plugin UDP listener: %w", err)
	}
	return nil
}

func requireDevelopmentFixturePorts(t *testing.T) {
	t.Helper()
	backend, err := net.Listen("tcp", "127.0.0.1:18377")
	if err != nil {
		t.Fatalf("required isolated backend port is unavailable: %v", err)
	}
	plugin, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 18378})
	if err != nil {
		_ = backend.Close()
		t.Fatalf("required isolated plugin port is unavailable: %v", err)
	}
	_ = backend.Close()
	_ = plugin.Close()
}

func installFixtureAppFactory(t *testing.T, health func(time.Duration) *http.Client,
	verify func(context.Context, string, string, string, string) error, observer func(string)) {
	t.Helper()
	oldFactory := newDevelopmentForegroundServer
	newDevelopmentForegroundServer = func(cfg *config.Config, workflow *tailscalecli.DevelopmentWorkflow, _ io.Writer) (developmentForegroundServer, error) {
		return app.NewDevelopmentCLIWithFixtureHooks(cfg, version, revision,
			slog.New(slog.NewTextHandler(io.Discard, nil)), workflow,
			app.DevelopmentCLIFixtureHooks{HealthClient: health, VerifyPublicBundle: verify, ControlCallbackObserver: observer, MarkInventoryReady: true})
	}
	t.Cleanup(func() { newDevelopmentForegroundServer = oldFactory })
}

type trustedDevelopmentHTTPSFixture struct {
	listener       net.Listener
	server         *http.Server
	address        string
	hostport       string
	roots          *x509.CertPool
	fixture        *developmentCommandFixture
	healthRequests atomic.Int32
}

func startTrustedDevelopmentHTTPSFixture(t *testing.T, fixture *developmentCommandFixture, webRoot string) *trustedDevelopmentHTTPSFixture {
	t.Helper()
	parsed, err := url.Parse(reviewOrigin)
	if err != nil {
		t.Fatal(err)
	}
	rawListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	certificate := generateReviewCertificate(t)
	roots := x509.NewCertPool()
	roots.AddCert(certificate.Leaf)
	listener := tls.NewListener(rawListener, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
	backend, err := url.Parse("http://127.0.0.1:18377")
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(backend)
	public := &trustedDevelopmentHTTPSFixture{
		listener: rawListener, server: nil, address: rawListener.Addr().String(),
		hostport: net.JoinHostPort(parsed.Hostname(), parsed.Port()), roots: roots, fixture: fixture,
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			public.healthRequests.Add(1)
		}
		if !fixtureHasPublishedRoute(fixture) {
			http.Error(w, "synthetic Serve route is absent", http.StatusServiceUnavailable)
			return
		}
		proxy.ServeHTTP(w, r)
	})}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	public.server = server
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("HTTPS fixture shutdown: %v", err)
		}
		_ = rawListener.Close()
	})
	return public
}

func (f *trustedDevelopmentHTTPSFixture) client() *http.Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: f.roots, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if address == "127.0.0.1:18377" {
				return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, address)
			}
			if address != f.hostport {
				return nil, fmt.Errorf("fixture refuses unexpected HTTPS destination %q", address)
			}
			if !fixtureHasPublishedRoute(f.fixture) {
				return nil, errors.New("fixture refused HTTPS while the exact route was absent")
			}
			return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, f.address)
		},
	}
	return &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("fixture HTTPS client refuses redirects")
	}}
}

func generateReviewCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: "herdr.tailnet.ts.net"},
		DNSNames: []string{"herdr.tailnet.ts.net"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	cert.Leaf, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func fixtureHasPublishedRoute(fixture *developmentCommandFixture) bool {
	data, err := os.ReadFile(fixture.serveFile)
	return err == nil && strings.Contains(string(data), `"Proxy":"http://127.0.0.1:18377"`) &&
		strings.Contains(string(data), `"herdr.tailnet.ts.net:8443"`)
}

func verifyTrustedExactPublishedBundle(ctx context.Context, client *http.Client, root, origin, version, revision string) error {
	if origin != reviewOrigin {
		return fmt.Errorf("unexpected bundle origin %q", origin)
	}
	local, err := release.VerifyWebDescriptor(os.DirFS(root), version)
	if err != nil {
		return err
	}
	localDescriptor, err := os.ReadFile(filepath.Join(root, "release.json"))
	if err != nil {
		return err
	}
	localVersion, err := os.ReadFile(filepath.Join(root, "version.json"))
	if err != nil {
		return err
	}
	var identity struct {
		ReleaseVersion string `json:"release_version"`
		Revision       string `json:"revision"`
	}
	if err := json.Unmarshal(localVersion, &identity); err != nil || identity.ReleaseVersion != version || identity.Revision != revision {
		return fmt.Errorf("local web version/revision mismatch: %+v err=%v", identity, err)
	}
	resources := map[string][]byte{"release.json": localDescriptor, "version.json": localVersion}
	for _, file := range local.Files {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file.Path)))
		if err != nil {
			return err
		}
		resources[file.Path] = data
	}
	remote := make(map[string][]byte, len(resources))
	for path, expected := range resources {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(origin, "/")+"/"+path, nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("trusted HTTPS request for %s: %w", path, err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 8<<20))
		_ = response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK {
			return fmt.Errorf("public bundle resource %s returned %d: %v", path, response.StatusCode, readErr)
		}
		if string(body) != string(expected) {
			return fmt.Errorf("public bundle resource %s differs from exact local release bytes", path)
		}
		remote[path] = body
	}
	if _, err := release.VerifyWebDescriptorData(remote["release.json"], remote, version); err != nil {
		return err
	}
	if local.Version != version {
		return fmt.Errorf("local descriptor version changed: %s", local.Version)
	}
	return nil
}

func seedDevelopmentRouteJournal(t *testing.T, fixture *developmentCommandFixture, state tailscalecli.RegistrationState, routePresent bool) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	record := map[string]any{
		"schema": 1, "installation_id": "fixture-instance", "scope": "development",
		"node_id": "node-fixture", "dns_name": "herdr.tailnet.ts.net",
		"profile":     string(tailscalecli.ProfileAppStoreSupplied),
		"binary_path": fixture.cli, "https_port": 8443, "backend_port": 18377, "path": "/",
		"backend": "http://127.0.0.1:18377", "consent_scope": "persistent-route-and-four-risks-v1",
		"operation_id": reviewOperationID, "state": state, "updated_at": now,
		"mutation_acknowledged": state == tailscalecli.StateRegistered,
	}
	if state == tailscalecli.StatePublishPending || state == tailscalecli.StatePublishUncertain {
		record["reservation_id"] = reviewReservationID
	}
	if state == tailscalecli.StateRemovePending || state == tailscalecli.StateRemoveUncertain {
		record["consent_scope"] = "explicit-route-removal-and-no-remote-drain-v1"
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	writeCommandFixtureFile(t, filepath.Join(fixture.state, "registration.json"), string(data)+"\n", 0o600)
	if routePresent {
		writeCommandFixtureFile(t, fixture.serveFile, commandFixtureRoute+"\n", 0o600)
	}
	reservation := map[string]any{
		"schema": 1, "installation_id": "fixture-instance", "scope": "development", "node_id": "node-fixture",
		"https_port": 8443, "backend_port": 18377, "origin": reviewOrigin,
		"state": tailscalecli.StateRegistered, "updated_at": now,
	}
	if state == tailscalecli.StatePublishPending || state == tailscalecli.StatePublishUncertain {
		reservation["reservation_id"] = reviewReservationID
		reservation["state"] = tailscalecli.StatePublishPending
	}
	reservationData, err := json.Marshal(reservation)
	if err != nil {
		t.Fatal(err)
	}
	writeCommandFixtureFile(t, filepath.Join(fixture.coordination, "backend-port-18377.json"), string(reservationData)+"\n", 0o600)
}

func markReservationReconciledPresent(t *testing.T, fixture *developmentCommandFixture) {
	t.Helper()
	path := filepath.Join(fixture.coordination, "backend-port-18377.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pending backend reservation: %v", err)
	}
	var reservation map[string]any
	if err := json.Unmarshal(data, &reservation); err != nil {
		t.Fatalf("decode pending backend reservation: %v", err)
	}
	reservation["state"] = tailscalecli.StateReconciledPresent
	delete(reservation, "reservation_id")
	data, err = json.Marshal(reservation)
	if err != nil {
		t.Fatalf("encode split backend reservation: %v", err)
	}
	writeCommandFixtureFile(t, path, string(data)+"\n", 0o600)
}

func developmentJournalOperationID(t *testing.T, fixture *developmentCommandFixture) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixture.state, "registration.json"))
	if err != nil {
		t.Fatalf("read registration operation: %v", err)
	}
	var record struct {
		OperationID string `json:"operation_id"`
	}
	if err := json.Unmarshal(data, &record); err != nil || record.OperationID == "" {
		t.Fatalf("decode registration operation ID: operation=%q err=%v", record.OperationID, err)
	}
	return record.OperationID
}

func assertFreshJournalState(t *testing.T, fixture *developmentCommandFixture, want tailscalecli.RegistrationState, acknowledged bool, reservationID string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixture.state, "registration.json"))
	if err != nil {
		t.Fatalf("read registration: %v", err)
	}
	var record struct {
		State                tailscalecli.RegistrationState `json:"state"`
		OperationID          string                         `json:"operation_id"`
		ReservationID        string                         `json:"reservation_id"`
		MutationAcknowledged bool                           `json:"mutation_acknowledged"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	operationID, decodeErr := hex.DecodeString(record.OperationID)
	if record.State != want || decodeErr != nil || len(operationID) != 16 ||
		record.ReservationID != reservationID || record.MutationAcknowledged != acknowledged {
		t.Fatalf("new journal after command = %+v; want state=%s reservation=%s ack=%t and a fresh operation ID: decode err=%v", record, want, reservationID, acknowledged, decodeErr)
	}
}

func assertJournalState(t *testing.T, fixture *developmentCommandFixture, want tailscalecli.RegistrationState, acknowledged bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixture.state, "registration.json"))
	if err != nil {
		t.Fatalf("read registration: %v", err)
	}
	var record struct {
		State                tailscalecli.RegistrationState `json:"state"`
		OperationID          string                         `json:"operation_id"`
		ReservationID        string                         `json:"reservation_id"`
		MutationAcknowledged bool                           `json:"mutation_acknowledged"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.State != want || record.OperationID != reviewOperationID || record.MutationAcknowledged != acknowledged {
		t.Fatalf("journal after command = %+v; want state=%s ack=%t", record, want, acknowledged)
	}
}

func assertReconciledState(t *testing.T, fixture *developmentCommandFixture, state tailscalecli.RegistrationState, present bool) {
	t.Helper()
	assertJournalState(t, fixture, state, false)
	if present != fixtureHasPublishedRoute(fixture) {
		t.Fatalf("reconcile changed route presence: present=%t actual=%t", present, fixtureHasPublishedRoute(fixture))
	}
}

func assertRegisteredReservation(t *testing.T, fixture *developmentCommandFixture) {
	t.Helper()
	path := filepath.Join(fixture.coordination, "backend-port-18377.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read backend reservation: %v", err)
	}
	var reservation struct {
		State         tailscalecli.RegistrationState `json:"state"`
		ReservationID string                         `json:"reservation_id"`
	}
	if err := json.Unmarshal(data, &reservation); err != nil || reservation.State != tailscalecli.StateRegistered || reservation.ReservationID != "" {
		t.Fatalf("repair reservation outcome = %+v err=%v", reservation, err)
	}
}

func assertReservationState(t *testing.T, fixture *developmentCommandFixture, want tailscalecli.RegistrationState, reservationID string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixture.coordination, "backend-port-18377.json"))
	if err != nil {
		t.Fatalf("read backend reservation: %v", err)
	}
	var reservation struct {
		State         tailscalecli.RegistrationState `json:"state"`
		ReservationID string                         `json:"reservation_id"`
	}
	if err := json.Unmarshal(data, &reservation); err != nil || reservation.State != want || reservation.ReservationID != reservationID {
		t.Fatalf("backend reservation = %+v; want state=%s reservation=%s err=%v", reservation, want, reservationID, err)
	}
}

func assertReservationAbsent(t *testing.T, fixture *developmentCommandFixture) {
	t.Helper()
	path := filepath.Join(fixture.coordination, "backend-port-18377.json")
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent-route operation retained backend reservation: stat err=%v", err)
	}
}

func assertSingleRoutePublish(t *testing.T, fixture *developmentCommandFixture) {
	t.Helper()
	data, err := os.ReadFile(fixture.cliLog)
	if err != nil {
		t.Fatalf("read public-command CLI log: %v", err)
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "serve --bg ") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("public command dispatched %d Serve writes; CLI log=%s", count, data)
	}
}

func assertServeMutationCounts(t *testing.T, fixture *developmentCommandFixture, publishes, removals int) {
	t.Helper()
	data, err := os.ReadFile(fixture.cliLog)
	if err != nil {
		t.Fatalf("read public-command CLI log: %v", err)
	}
	publishCount := strings.Count(string(data), "serve --bg --https=8443 --set-path=/ http://127.0.0.1:18377")
	removeCount := strings.Count(string(data), "serve --bg --https=8443 --set-path=/ off")
	if publishCount != publishes || removeCount != removals {
		t.Fatalf("public command Serve writes = publish:%d remove:%d; want publish:%d remove:%d; CLI log=%s", publishCount, removeCount, publishes, removals, data)
	}
}

func assertNoRouteMutationCalls(t *testing.T, fixture *developmentCommandFixture) {
	t.Helper()
	data, err := os.ReadFile(fixture.cliLog)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "serve --bg ") || strings.HasPrefix(line, "serve --bg") {
			t.Fatalf("public command performed route mutation: %s", line)
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

const reviewUnpublishConfirmation = "UNPUBLISH DEVELOPMENT ROUTE node=node-fixture origin=" + reviewOrigin + " https-port=8443 backend=127.0.0.1:18377"

func reviewReconcileConfirmation(operationID, reservationID, observed string) string {
	return fmt.Sprintf("RECONCILE DEVELOPMENT ROUTE operation=%s reservation=%s observed=%s node=node-fixture origin=%s https-port=8443 backend=127.0.0.1:18377",
		operationID, reservationID, observed, reviewOrigin)
}

func reviewAbandonConfirmation(operationID, reservationID string) string {
	return fmt.Sprintf("ABANDON MISSING DEVELOPMENT ROUTE operation=%s reservation=%s node=node-fixture origin=%s https-port=8443 backend=127.0.0.1:18377",
		operationID, reservationID, reviewOrigin)
}

// developmentLifecycleSeed writes one journal/reservation pair that the
// manager can reach through an interrupted write between two lifecycle steps.
type developmentLifecycleSeed struct {
	journal              tailscalecli.RegistrationState
	journalReservationID string
	reservation          tailscalecli.RegistrationState // empty: no shared reservation
	reservationID        string
	serve                string // empty: no synthetic Serve configuration
}

func seedDevelopmentLifecycle(t *testing.T, fixture *developmentCommandFixture, seed developmentLifecycleSeed) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	consentScope := "persistent-route-and-four-risks-v1"
	switch seed.journal {
	case tailscalecli.StateRemovePending, tailscalecli.StateRemoveUncertain, tailscalecli.StateRemoved:
		consentScope = "explicit-route-removal-and-no-remote-drain-v1"
	case tailscalecli.StateReconciledPresent, tailscalecli.StateReconciledAbsent:
		consentScope = "explicit-journal-reconciliation-v1"
	}
	record := map[string]any{
		"schema": 1, "installation_id": "fixture-instance", "scope": "development",
		"node_id": "node-fixture", "dns_name": "herdr.tailnet.ts.net",
		"profile":     string(tailscalecli.ProfileAppStoreSupplied),
		"binary_path": fixture.cli, "https_port": 8443, "backend_port": 18377, "path": "/",
		"backend": "http://127.0.0.1:18377", "consent_scope": consentScope,
		"operation_id": reviewOperationID, "state": seed.journal, "updated_at": now,
		"mutation_acknowledged": seed.journal == tailscalecli.StateRegistered || seed.journal == tailscalecli.StateRemoved,
	}
	if seed.journalReservationID != "" {
		record["reservation_id"] = seed.journalReservationID
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	writeCommandFixtureFile(t, filepath.Join(fixture.state, "registration.json"), string(data)+"\n", 0o600)
	if seed.reservation != "" {
		reservation := map[string]any{
			"schema": 1, "installation_id": "fixture-instance", "scope": "development", "node_id": "node-fixture",
			"https_port": 8443, "backend_port": 18377, "origin": reviewOrigin,
			"state": seed.reservation, "updated_at": now,
		}
		if seed.reservationID != "" {
			reservation["reservation_id"] = seed.reservationID
		}
		reservationData, err := json.Marshal(reservation)
		if err != nil {
			t.Fatal(err)
		}
		writeCommandFixtureFile(t, filepath.Join(fixture.coordination, "backend-port-18377.json"), string(reservationData)+"\n", 0o600)
	}
	if seed.serve != "" {
		writeCommandFixtureFile(t, fixture.serveFile, seed.serve+"\n", 0o600)
	}
}

type developmentJournalSnapshot struct {
	State                tailscalecli.RegistrationState `json:"state"`
	OperationID          string                         `json:"operation_id"`
	ReservationID        string                         `json:"reservation_id"`
	MutationAcknowledged bool                           `json:"mutation_acknowledged"`
}

func readDevelopmentJournal(t *testing.T, fixture *developmentCommandFixture) developmentJournalSnapshot {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixture.state, "registration.json"))
	if err != nil {
		t.Fatalf("read registration: %v", err)
	}
	var record developmentJournalSnapshot
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	return record
}

func assertDevelopmentJournal(t *testing.T, fixture *developmentCommandFixture, state tailscalecli.RegistrationState,
	operationID, reservationID string, acknowledged bool) {
	t.Helper()
	record := readDevelopmentJournal(t, fixture)
	if record.State != state || record.OperationID != operationID || record.ReservationID != reservationID ||
		record.MutationAcknowledged != acknowledged {
		t.Fatalf("journal = %+v; want state=%s operation=%s reservation=%s ack=%t", record, state, operationID, reservationID, acknowledged)
	}
}

// assertFreshRemovalJournal checks that unpublish recorded a new removal
// operation, unbound from any earlier publication attempt, without fabricating
// an acknowledgement.
func assertFreshRemovalJournal(t *testing.T, fixture *developmentCommandFixture, previousOperationID string) string {
	t.Helper()
	record := readDevelopmentJournal(t, fixture)
	operationID, err := hex.DecodeString(record.OperationID)
	if record.State != tailscalecli.StateRemoveUncertain || record.MutationAcknowledged || record.ReservationID != "" ||
		err != nil || len(operationID) != 16 || record.OperationID == previousOperationID {
		t.Fatalf("removal journal = %+v; want a fresh unacknowledged remove-uncertain operation without a publication attempt (previous operation %s, decode err=%v)",
			record, previousOperationID, err)
	}
	return record.OperationID
}

func assertServeFixture(t *testing.T, fixture *developmentCommandFixture, want string) {
	t.Helper()
	data, err := os.ReadFile(fixture.serveFile)
	if want == "" {
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("synthetic Serve configuration = %q err=%v; want absent", data, err)
		}
		return
	}
	if err != nil || strings.TrimSpace(string(data)) != want {
		t.Fatalf("synthetic Serve configuration = %q err=%v; want %s", data, err, want)
	}
}

func mustDispatchDevelopmentCommand(t *testing.T, action, input string) {
	t.Helper()
	code, _, stderr, err := dispatchMainCommand(t, []string{"dev-tailscale-cli", action}, input+"\n")
	if err != nil || code != 0 {
		t.Fatalf("public %s returned code=%d err=%v\nstderr=%s", action, code, err, stderr)
	}
}

func dispatchDevelopmentStatus(t *testing.T) (int, tailscalecli.RecoveryReport) {
	t.Helper()
	code, stdout, stderr, _ := dispatchMainCommand(t, []string{"dev-tailscale-cli", "status"}, "")
	var report tailscalecli.RecoveryReport
	if err := json.NewDecoder(strings.NewReader(stdout)).Decode(&report); err != nil {
		t.Fatalf("decode public status report: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
	}
	return code, report
}
