package app

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/push"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

func deviceActionServer(t *testing.T, withStore bool) (*Server, *deviceauth.Store) {
	t.Helper()
	s := testServerWithCacheDir(t.TempDir())
	pushManager, err := push.NewManager(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.pushM = pushManager
	useProductionHandler(t, s)
	if !withStore {
		return s, nil
	}
	runtimeDir := t.TempDir()
	enrollBootstrapDevice(t, runtimeDir, strings.Repeat("k", 32))
	store, err := deviceauth.Open(filepath.Join(runtimeDir, "device-auth"))
	if err != nil {
		t.Fatal(err)
	}
	s.deviceAuth = store
	enrollSecondController(t, store)
	return s, store
}

func enrollSecondController(t *testing.T, store *deviceauth.Store) {
	t.Helper()
	invitation, err := store.CreateInvitation("Second controller", deviceauth.RoleController, "en")
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.CompleteE2EEAuth(t.Context(), transport.E2EEAuthSelector{
		Kind: transport.E2EEAuthInvitation, ID: invitation.InvitationID, Version: invitation.Version, Locale: "en",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteE2EEAuth(t.Context(), transport.E2EEAuthSelector{
		Kind: transport.E2EEAuthCredential, ID: result.Identity.CredentialID,
		Version: result.Identity.CredentialVersion, Locale: result.Identity.Locale,
	}, true); err != nil {
		t.Fatal(err)
	}
}

func deviceRequest(action string, fields map[string]any) map[string]any {
	message := map[string]any{"type": action, "request_id": "req-1", "protocol": protocol.Version}
	for key, value := range fields {
		message[key] = value
	}
	return message
}

func requireFailedResult(t *testing.T, response map[string]any, action, wantError string) {
	t.Helper()
	if response["type"] != "command_result" || response["action"] != action ||
		response["ok"] != false || response["error"] != wantError {
		t.Fatalf("response = %#v, want %s failed with %q", response, action, wantError)
	}
}

func TestDeviceActionsThroughHandler(t *testing.T) {
	s, store := deviceActionServer(t, true)
	conn := dialHub(t, s)
	credentials := store.ListCredentials("")
	if len(credentials) != 2 {
		t.Fatalf("enrolled credentials = %#v, want two", credentials)
	}
	known := credentials[0]

	t.Run("rename the known device", func(t *testing.T) {
		response := roundTrip(t, conn, deviceRequest("rename_device", map[string]any{"device_id": known.DeviceID, "name": "Kitchen iPad"}))
		if response["type"] != "command_result" || response["ok"] != true || response["phase"] != "completed" {
			t.Fatalf("response = %#v", response)
		}
		device, _ := response["data"].(map[string]any)["device"].(map[string]any)
		if device["name"] != "Kitchen iPad" || device["device_id"] != known.DeviceID {
			t.Fatalf("renamed device = %#v", device)
		}
		if got := store.ListCredentials("")[0].Name; got != "Kitchen iPad" {
			t.Fatalf("stored name = %q", got)
		}
	})

	t.Run("rename an unknown device", func(t *testing.T) {
		response := roundTrip(t, conn, deviceRequest("rename_device", map[string]any{"device_id": "missing", "name": "Ghost"}))
		requireFailedResult(t, response, "rename_device", "Device credential was not found")
	})

	t.Run("revoke an unknown device", func(t *testing.T) {
		response := roundTrip(t, conn, deviceRequest("revoke_device", map[string]any{"device_id": "missing"}))
		requireFailedResult(t, response, "revoke_device", "Device credential was not found")
	})

	t.Run("device management actions need an authenticated client", func(t *testing.T) {
		for _, action := range []string{"device_list", "create_device_invitation", "reset_devices"} {
			response := roundTrip(t, conn, deviceRequest(action, nil))
			requireFailedResult(t, response, action, "Device management is unavailable")
		}
		if got := len(activeDeviceCredentials(store, "")); got != 2 {
			t.Fatalf("active credentials = %d, want both devices untouched", got)
		}
	})

	t.Run("revoke the known device", func(t *testing.T) {
		subscription := push.Subscription{Endpoint: "https://fcm.googleapis.com/fcm/send/known", DeviceID: known.DeviceID}
		subscription.Keys.P256dh = "p256dh"
		subscription.Keys.Auth = "auth"
		if err := s.pushM.Subscribe(subscription); err != nil {
			t.Fatal(err)
		}
		if !s.reservePushTest(known.DeviceID, time.Now()) {
			t.Fatal("first push test was rate limited")
		}

		response := roundTrip(t, conn, deviceRequest("revoke_device", map[string]any{"device_id": known.DeviceID}))
		if response["type"] != "command_result" || response["ok"] != true || response["phase"] != "completed" {
			t.Fatalf("response = %#v", response)
		}
		if _, usable := store.AuthorizeCredential(known.CredentialID, known.Version); usable {
			t.Fatal("revoked credential is still usable")
		}
		if subscriptions := s.pushM.Subscriptions(); len(subscriptions) != 0 {
			t.Fatalf("push subscriptions after revoke = %#v", subscriptions)
		}
		s.pushTestMu.Lock()
		_, throttled := s.pushTestLast[known.DeviceID]
		s.pushTestMu.Unlock()
		if throttled {
			t.Fatal("revoked device kept its push test throttle entry")
		}
	})

	t.Run("without a store", func(t *testing.T) {
		bare, _ := deviceActionServer(t, false)
		bareConn := dialHub(t, bare)
		for _, action := range []string{"rename_device", "revoke_device"} {
			response := roundTrip(t, bareConn, deviceRequest(action, map[string]any{"device_id": "device-1", "name": "Phone"}))
			requireFailedResult(t, response, action, "Device management is unavailable")
		}
	})
}
