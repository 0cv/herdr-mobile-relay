package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/push"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
	"github.com/coder/websocket"
)

func deviceActionServer(t *testing.T, withStore bool) (*Server, *deviceauth.Store) {
	t.Helper()
	s := testServerWithCacheDir(t.TempDir())
	s.pushM = newPushManager(t, t.TempDir())
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

func credentialIdentity(credential deviceauth.Credential) transport.AuthenticatedIdentity {
	return transport.AuthenticatedIdentity{
		DeviceID: credential.DeviceID, CredentialID: credential.CredentialID, Role: string(credential.Role),
		Locale: credential.Locale, CredentialVersion: credential.Version,
	}
}

func commandData(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	data, ok := response["data"].(map[string]any)
	if !ok {
		t.Fatalf("response carries no data: %#v", response)
	}
	return data
}

func requireCompleted(t *testing.T, response map[string]any, action string) {
	t.Helper()
	if response["type"] != "command_result" || response["action"] != action ||
		response["ok"] != true || response["phase"] != "completed" {
		t.Fatalf("response = %#v, want %s completed", response, action)
	}
}

func unwritablePushManager(t *testing.T) *push.Manager {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not stop root")
	}
	dir := t.TempDir()
	manager := newPushManager(t, dir)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	return manager
}

func TestDeviceListAndInvitationWithIdentity(t *testing.T) {
	s, store := deviceActionServer(t, true)
	conn, client := connectedClient(t, s)
	credentials := store.ListCredentials("")
	current, other := credentials[0], credentials[1]

	t.Run("list skips revoked credentials and reports the caller", func(t *testing.T) {
		if _, err := store.RevokeCredential(other.CredentialID); err != nil {
			t.Fatal(err)
		}
		s.handleDeviceList(client, protocol.Inbound{RequestID: "req-1"}, "device_list", credentialIdentity(current), true)
		response := readFrame(t, conn)
		requireCompleted(t, response, "device_list")
		data := commandData(t, response)
		if data["current_device_id"] != current.DeviceID || data["role"] != "controller" {
			t.Fatalf("data = %#v", data)
		}
		devices, _ := data["devices"].([]any)
		if len(devices) != 1 {
			t.Fatalf("devices = %#v, want only the active one", devices)
		}
		device, _ := devices[0].(map[string]any)
		if device["device_id"] != current.DeviceID || device["current"] != true {
			t.Fatalf("device = %#v", device)
		}
	})

	t.Run("list reports a reader role", func(t *testing.T) {
		identity := credentialIdentity(current)
		identity.Role = "reader"
		s.handleDeviceList(client, protocol.Inbound{RequestID: "req-1"}, "device_list", identity, true)
		if role := commandData(t, readFrame(t, conn))["role"]; role != "reader" {
			t.Fatalf("role = %v, want reader", role)
		}
	})

	t.Run("invitation uses the caller's locale", func(t *testing.T) {
		identity := credentialIdentity(current)
		identity.Locale = "fr"
		inbound := protocol.Inbound{Type: "create_device_invitation", RequestID: "req-1", Name: "Tablet", Role: "reader"}
		s.handleCreateDeviceInvitation(client, inbound, identity, true)
		response := readFrame(t, conn)
		requireCompleted(t, response, "create_device_invitation")
		invitation, _ := commandData(t, response)["invitation"].(map[string]any)
		if invitation["name"] != "Tablet" || invitation["role"] != "reader" || invitation["locale"] != "fr" ||
			invitation["invitation_id"] == "" || invitation["secret"] == "" {
			t.Fatalf("invitation = %#v", invitation)
		}
		_, err := store.ResolveE2EESecret(t.Context(), transport.E2EEAuthSelector{
			Kind: transport.E2EEAuthInvitation, ID: invitation["invitation_id"].(string), Version: 1, Locale: "fr",
		})
		if err != nil {
			t.Fatalf("store does not hold the invitation: %v", err)
		}
	})

	t.Run("invitation passes the store's error through", func(t *testing.T) {
		_, wantErr := store.CreateInvitation("Tablet", "admin", "en")
		if wantErr == nil {
			t.Fatal("store accepted an invalid role")
		}
		inbound := protocol.Inbound{Type: "create_device_invitation", RequestID: "req-1", Name: "Tablet", Role: "admin"}
		s.handleCreateDeviceInvitation(client, inbound, credentialIdentity(current), true)
		requireFailedResult(t, readFrame(t, conn), "create_device_invitation", wantErr.Error())
	})
}

func TestDeviceResetWithIdentity(t *testing.T) {
	secret := strings.Repeat("s", 32)
	resetServer := func(t *testing.T) (*Server, *deviceauth.Store, *websocket.Conn, *transport.ClientConn) {
		s, store := deviceActionServer(t, true)
		cfg := *s.cfg
		cfg.Token = secret
		s.cfg = &cfg
		s.hostname = "relay"
		conn, client := connectedClient(t, s)
		return s, store, conn, client
	}
	reset := func(s *Server, store *deviceauth.Store, client *transport.ClientConn) {
		caller := credentialIdentity(store.ListCredentials("")[0])
		s.handleResetDevices(client, protocol.Inbound{RequestID: "req-1", Type: "reset_devices"}, caller, true)
	}

	t.Run("reset leaves the bootstrap invitation and clears every device's push state", func(t *testing.T) {
		s, store, conn, client := resetServer(t)
		for _, credential := range store.ListCredentials("") {
			subscribeDevice(t, s.pushM, credential.DeviceID, "https://fcm.googleapis.com/fcm/send/"+credential.DeviceID)
			policy := s.pushM.Policy(credential.DeviceID, "en")
			policy.Snoozed = true
			if err := s.pushM.SetPolicy(policy); err != nil {
				t.Fatal(err)
			}
			if !s.reservePushTest(credential.DeviceID, time.Now()) {
				t.Fatal("first push test was rate limited")
			}
		}
		devices := store.ListCredentials("")

		reset(s, store, client)

		requireCompleted(t, readFrame(t, conn), "reset_devices")
		if credentials := store.ListCredentials(""); len(credentials) != 0 {
			t.Fatalf("credentials after reset = %#v", credentials)
		}
		bootstrap, err := store.ResolveE2EESecret(t.Context(), transport.E2EEAuthSelector{
			Kind: transport.E2EEAuthInvitation, ID: "bootstrap", Version: 1, Locale: "en",
		})
		if err != nil || string(bootstrap) != secret {
			t.Fatalf("bootstrap invitation = %q, %v; want the configured secret", bootstrap, err)
		}
		if subscriptions := s.pushM.Subscriptions(); len(subscriptions) != 0 {
			t.Fatalf("push subscriptions after reset = %#v", subscriptions)
		}
		for _, device := range devices {
			if s.pushM.Policy(device.DeviceID, "en").Snoozed {
				t.Fatalf("device %s kept its push policy", device.DeviceID)
			}
		}
		s.pushTestMu.Lock()
		throttled := len(s.pushTestLast)
		s.pushTestMu.Unlock()
		if throttled != 0 {
			t.Fatalf("push test throttle entries after reset = %d", throttled)
		}
	})

	t.Run("reset reports a push cleanup that could not be persisted", func(t *testing.T) {
		s, store, conn, client := resetServer(t)
		s.pushM = unwritablePushManager(t)

		reset(s, store, client)

		requireFailedResult(t, readFrame(t, conn), "reset_devices", "Devices reset, but notification cleanup could not be persisted")
		if credentials := store.ListCredentials(""); len(credentials) != 0 {
			t.Fatalf("credentials after reset = %#v, want the reset to stand", credentials)
		}
	})

	t.Run("revoke reports a push cleanup that could not be persisted", func(t *testing.T) {
		s, store, conn, _ := resetServer(t)
		s.pushM = unwritablePushManager(t)
		other := store.ListCredentials("")[1]

		response := roundTrip(t, conn, deviceRequest("revoke_device", map[string]any{"device_id": other.DeviceID}))

		requireFailedResult(t, response, "revoke_device", "Device revoked, but notification cleanup could not be persisted")
		device, _ := commandData(t, response)["device"].(map[string]any)
		if device["device_id"] != other.DeviceID {
			t.Fatalf("device = %#v, want the revoked device", device)
		}
		for _, credential := range store.ListCredentials("") {
			if credential.DeviceID == other.DeviceID && !credential.Revoked {
				t.Fatal("revoked credential is still active")
			}
		}
	})
}
