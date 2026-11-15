package app

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

const deviceManagementUnavailable = "Device management is unavailable"

func (s *Server) handleDeviceList(client *transport.ClientConn, inbound protocol.Inbound, action string, identity transport.AuthenticatedIdentity, authenticated bool) {
	reply := s.reply(client, inbound.RequestID, action, "")
	if s.deviceAuth == nil || !authenticated {
		reply.fail(deviceManagementUnavailable)
		return
	}
	reply.complete(map[string]any{
		"devices":           activeDeviceCredentials(s.deviceAuth, identity.CredentialID),
		"current_device_id": identity.DeviceID,
		"role":              identity.Role,
	})
}

func (s *Server) handleCreateDeviceInvitation(client *transport.ClientConn, inbound protocol.Inbound, msg map[string]any, action string, identity transport.AuthenticatedIdentity, authenticated bool) {
	reply := s.auditedReply(client, msg, inbound.RequestID, action, "")
	if s.deviceAuth == nil || !authenticated {
		reply.fail(deviceManagementUnavailable)
		return
	}
	invitation, invitationErr := s.deviceAuth.CreateInvitation(inbound.Name, deviceauth.Role(inbound.Role), identity.Locale)
	if invitationErr != nil {
		reply.fail(invitationErr.Error())
		return
	}
	reply.complete(map[string]any{"invitation": invitation})
}

func (s *Server) handleRenameDevice(client *transport.ClientConn, inbound protocol.Inbound, msg map[string]any, action string) {
	reply := s.auditedReply(client, msg, inbound.RequestID, action, "")
	if s.deviceAuth == nil {
		reply.fail(deviceManagementUnavailable)
		return
	}
	credentialID, found := s.credentialIDOrFail(reply, inbound.DeviceID)
	if !found {
		return
	}
	credential, renameErr := s.deviceAuth.RenameCredential(credentialID, inbound.Name)
	if renameErr != nil {
		reply.fail(renameErr.Error())
		return
	}
	reply.complete(map[string]any{"device": credential})
}

func (s *Server) handleRevokeDevice(client *transport.ClientConn, inbound protocol.Inbound, msg map[string]any, action string) {
	reply := s.auditedReply(client, msg, inbound.RequestID, action, "")
	if s.deviceAuth == nil {
		reply.fail(deviceManagementUnavailable)
		return
	}
	credentialID, found := s.credentialIDOrFail(reply, inbound.DeviceID)
	if !found {
		return
	}
	credential, revokeErr := s.deviceAuth.RevokeCredential(credentialID)
	if revokeErr != nil {
		reply.fail(revokeErr.Error())
		return
	}
	s.forgetPushTest(credential.DeviceID)
	pushCleanupErr := s.pushM.RemoveDevice(credential.DeviceID)
	time.AfterFunc(250*time.Millisecond, func() {
		s.hub.DisconnectCredential(credential.CredentialID, credential.Version)
	})
	if pushCleanupErr != nil {
		reply.failWithData("Device revoked, but notification cleanup could not be persisted", map[string]any{"device": credential})
		return
	}
	reply.complete(map[string]any{"device": credential})
}

func (s *Server) handleResetDevices(client *transport.ClientConn, inbound protocol.Inbound, msg map[string]any, action string, identity transport.AuthenticatedIdentity, authenticated bool) {
	reply := s.auditedReply(client, msg, inbound.RequestID, action, "")
	if s.deviceAuth == nil || !authenticated {
		reply.fail(deviceManagementUnavailable)
		return
	}
	credentials := activeDeviceCredentials(s.deviceAuth, "")
	if resetErr := s.deviceAuth.ResetWithBootstrap([]byte(s.cfg.Token), s.hostname, identity.Locale); resetErr != nil {
		reply.fail(resetErr.Error())
		return
	}
	for _, credential := range credentials {
		s.forgetPushTest(credential.DeviceID)
	}
	var resetPushErr error
	for _, credential := range credentials {
		if pushErr := s.pushM.RemoveDevice(credential.DeviceID); pushErr != nil && resetPushErr == nil {
			resetPushErr = pushErr
		}
	}
	time.AfterFunc(250*time.Millisecond, func() {
		s.disconnectCredentials(credentials)
	})
	if resetPushErr != nil {
		reply.fail("Devices reset, but notification cleanup could not be persisted")
		return
	}
	reply.complete(nil)
}

func deviceCredentialID(store *deviceauth.Store, deviceID string) (string, bool) {
	if store == nil || strings.TrimSpace(deviceID) == "" {
		return "", false
	}
	for _, credential := range store.ListCredentials("") {
		if credential.DeviceID == deviceID {
			return credential.CredentialID, true
		}
	}
	return "", false
}

func (s *Server) credentialIDOrFail(reply commandReply, deviceID string) (string, bool) {
	credentialID, found := deviceCredentialID(s.deviceAuth, deviceID)
	if !found {
		reply.fail("Device credential was not found")
	}
	return credentialID, found
}

func activeDeviceCredentials(store *deviceauth.Store, currentCredentialID string) []deviceauth.Credential {
	credentials := store.ListCredentials(currentCredentialID)
	active := credentials[:0]
	for _, credential := range credentials {
		if !credential.Revoked {
			active = append(active, credential)
		}
	}
	return active
}

func (s *Server) disconnectCredentials(credentials []deviceauth.Credential) {
	for _, credential := range credentials {
		s.hub.DisconnectCredential(credential.CredentialID, credential.Version)
	}
}

// armBootstrap prepares the relay's one-use pairing invitation. A re-armed
// launch serves the app from a new hostname, so every device enrolled under
// the previous one is stranded; starting from an empty device list keeps those
// dead entries out of Settings instead of accumulating one per launch.
func armBootstrap(store *deviceauth.Store, cfg *config.Config, hostname string) error {
	if cfg.RearmBootstrap {
		return store.ResetWithBootstrap([]byte(cfg.Token), hostname, "en")
	}
	return store.EnsureBootstrapInvitation([]byte(cfg.Token), hostname, "en")
}

// armBootstrapLoop re-arms the one-use bootstrap invitation on SIGUSR1. The
// setup scripts send it right before printing the setup link, so every printed
// QR pairs one more phone without revoking the devices already enrolled. Only a
// local process can signal the relay, which is the trust the printed key needs.
func (s *Server) armBootstrapLoop(ctx context.Context, signals <-chan os.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-signals:
			s.logger.Info("bootstrap invitation re-arm requested", "outcome", s.armBootstrapInvitation())
		}
	}
}

func (s *Server) armBootstrapInvitation() string {
	if s.deviceAuth == nil {
		return "no relay key configured"
	}
	if err := s.deviceAuth.ArmBootstrapInvitation([]byte(s.cfg.Token), s.hostname, "en"); err != nil {
		s.recordSafeError("bootstrap invitation re-arm failed", err)
		return err.Error()
	}
	return "armed for one more device"
}
