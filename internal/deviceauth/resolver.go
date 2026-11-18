package deviceauth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/0cv/herdr-mobile-relay/internal/localize"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

// IsE2EEAuthRejected reports whether retrying the same selector can ever
// succeed. Store and rate-limit failures remain transient so a phone keeps its
// valid credential.
func (s *Store) IsE2EEAuthRejected(err error) bool {
	return errors.Is(err, ErrAuthentication) ||
		errors.Is(err, ErrRevoked) ||
		errors.Is(err, ErrInvitationExpired) ||
		errors.Is(err, ErrInvitationBurned)
}

func (s *Store) matchingInvitationLocked(selector transport.E2EEAuthSelector) *invitationRecord {
	record := s.state.Invitation
	if record == nil || record.InvitationID != selector.ID || record.Version != selector.Version {
		return nil
	}
	return record
}

func (s *Store) expireInvitationLocked() error {
	next := s.state.clone()
	next.Invitation = nil
	if err := s.commitLocked(next); err != nil {
		return fmt.Errorf("expire invitation: %w", err)
	}
	return nil
}

func (s *Store) ResolveE2EESecret(_ context.Context, selector transport.E2EEAuthSelector) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	switch selector.Kind {
	case transport.E2EEAuthInvitation:
		record := s.matchingInvitationLocked(selector)
		if record == nil {
			return nil, ErrAuthentication
		}
		if !now.Before(record.ExpiresAt) {
			if record.InvitationID != bootstrapInvitationID || (len(s.state.Credentials) > 0 && !s.rearmBootstrap) {
				if err := s.expireInvitationLocked(); err != nil {
					return nil, err
				}
				return nil, ErrInvitationExpired
			}
			next := s.state.clone()
			next.Invitation.ExpiresAt = now.Add(invitationLifetime)
			if err := s.commitLocked(next); err != nil {
				return nil, fmt.Errorf("refresh bootstrap invitation: %w", err)
			}
			record = s.state.Invitation
		}
		if record.FailedAttempts >= maxInviteAttempts {
			return nil, ErrInvitationBurned
		}
		if now.Before(record.NextAttemptAt) {
			return nil, ErrRateLimited
		}
		return decodeStoredSecret(record.Secret)
	case transport.E2EEAuthCredential:
		index := s.credentialIndex(selector.ID)
		if index < 0 {
			return nil, ErrAuthentication
		}
		record := s.state.Credentials[index]
		if record.Revoked {
			return nil, ErrRevoked
		}
		if record.Version != selector.Version {
			return nil, ErrAuthentication
		}
		return decodeStoredSecret(record.Secret)
	default:
		return nil, ErrAuthentication
	}
}

func (s *Store) CompleteE2EEAuth(_ context.Context, selector transport.E2EEAuthSelector, authenticated bool) (transport.E2EEAuthResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !authenticated {
		if selector.Kind != transport.E2EEAuthInvitation {
			return transport.E2EEAuthResult{}, ErrAuthentication
		}
		return transport.E2EEAuthResult{}, s.recordFailedInvitationLocked(selector)
	}
	switch selector.Kind {
	case transport.E2EEAuthInvitation:
		return s.redeemInvitationLocked(selector)
	case transport.E2EEAuthCredential:
		return s.completeCredentialLocked(selector)
	default:
		return transport.E2EEAuthResult{}, ErrAuthentication
	}
}

func (s *Store) recordFailedInvitationLocked(selector transport.E2EEAuthSelector) error {
	record := s.matchingInvitationLocked(selector)
	if record == nil {
		return ErrAuthentication
	}
	if record.InvitationID == bootstrapInvitationID {
		// Bootstrap invitations carry a full-entropy secret. Counting unauthenticated
		// guesses only lets remote clients deny service to the legitimate first device.
		return ErrAuthentication
	}
	now := s.now().UTC()
	if !now.Before(record.ExpiresAt) {
		if err := s.expireInvitationLocked(); err != nil {
			return err
		}
		return ErrInvitationExpired
	}
	next := s.state.clone()
	next.Invitation.FailedAttempts++
	burned := next.Invitation.FailedAttempts >= maxInviteAttempts
	if burned {
		next.Invitation = nil
	} else {
		delay := time.Second << (next.Invitation.FailedAttempts - 1)
		next.Invitation.NextAttemptAt = now.Add(delay)
	}
	if err := s.commitLocked(next); err != nil {
		return fmt.Errorf("persist invitation attempt: %w", err)
	}
	if burned {
		return ErrInvitationBurned
	}
	return ErrAuthentication
}

func (s *Store) redeemInvitationLocked(selector transport.E2EEAuthSelector) (transport.E2EEAuthResult, error) {
	record := s.matchingInvitationLocked(selector)
	if record == nil {
		return transport.E2EEAuthResult{}, ErrAuthentication
	}
	now := s.now().UTC()
	if !now.Before(record.ExpiresAt) {
		if err := s.expireInvitationLocked(); err != nil {
			return transport.E2EEAuthResult{}, err
		}
		return transport.E2EEAuthResult{}, ErrInvitationExpired
	}
	if record.PendingCredentialID != "" {
		index := s.credentialIndex(record.PendingCredentialID)
		if index < 0 {
			return transport.E2EEAuthResult{}, ErrAuthentication
		}
		return credentialAuthResult(s.state.Credentials[index], true)
	}
	deviceID, err := s.randomValue(identifierBytes)
	if err != nil {
		return transport.E2EEAuthResult{}, err
	}
	credentialID, err := s.randomValue(identifierBytes)
	if err != nil {
		return transport.E2EEAuthResult{}, err
	}
	secret, err := s.randomValue(secretBytes)
	if err != nil {
		return transport.E2EEAuthResult{}, err
	}
	secretBytesValue, err := decodeStoredSecret(secret)
	if err != nil {
		return transport.E2EEAuthResult{}, err
	}
	locale := string(localize.NormalizeLocale(selector.Locale))
	credential := credentialRecord{
		Credential: Credential{
			DeviceID: deviceID, CredentialID: credentialID, Name: record.Name,
			Role: record.Role, Locale: locale, PairedAt: now, LastSeenAt: now, Version: 1,
		},
		Secret: secret,
	}
	next := s.state.clone()
	next.Invitation.PendingCredentialID = credentialID
	next.Credentials = append(next.Credentials, credential)
	if err := s.commitLocked(next); err != nil {
		clear(secretBytesValue)
		return transport.E2EEAuthResult{}, fmt.Errorf("persist invitation redemption: %w", err)
	}
	return transport.E2EEAuthResult{
		Identity: transport.AuthenticatedIdentity{
			DeviceID: deviceID, CredentialID: credentialID, Role: string(record.Role),
			Locale: locale, CredentialVersion: credential.Version,
		},
		CredentialSecret: secretBytesValue,
	}, nil
}

func (s *Store) completeCredentialLocked(selector transport.E2EEAuthSelector) (transport.E2EEAuthResult, error) {
	index := s.credentialIndex(selector.ID)
	if index < 0 {
		return transport.E2EEAuthResult{}, ErrAuthentication
	}
	current := s.state.Credentials[index]
	if current.Revoked || current.Version != selector.Version {
		return transport.E2EEAuthResult{}, ErrAuthentication
	}
	next := s.state.clone()
	record := &next.Credentials[index]
	record.LastSeenAt = s.now().UTC()
	record.Locale = string(localize.NormalizeLocale(selector.Locale))
	if invitation := next.Invitation; invitation != nil &&
		invitation.PendingCredentialID == record.CredentialID {
		next.Invitation = nil
	}
	if err := s.commitLocked(next); err != nil {
		return transport.E2EEAuthResult{}, fmt.Errorf("persist credential last seen: %w", err)
	}
	return credentialAuthResult(s.state.Credentials[index], false)
}

func credentialAuthResult(record credentialRecord, issueSecret bool) (transport.E2EEAuthResult, error) {
	result := transport.E2EEAuthResult{Identity: transport.AuthenticatedIdentity{
		DeviceID: record.DeviceID, CredentialID: record.CredentialID, Role: string(record.Role),
		Locale: record.Locale, CredentialVersion: record.Version,
	}}
	if !issueSecret {
		return result, nil
	}
	secret, err := decodeStoredSecret(record.Secret)
	if err != nil {
		return transport.E2EEAuthResult{}, err
	}
	result.CredentialSecret = secret
	return result, nil
}

func decodeStoredSecret(encoded string) ([]byte, error) {
	secret, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(secret) != secretBytes {
		clear(secret)
		return nil, ErrAuthentication
	}
	return secret, nil
}
