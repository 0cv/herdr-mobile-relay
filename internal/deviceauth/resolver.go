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

func (s *Store) ResolveE2EESecret(_ context.Context, selector transport.E2EEAuthSelector) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	switch selector.Kind {
	case transport.E2EEAuthInvitation:
		record := s.state.Invitation
		if record == nil || record.InvitationID != selector.ID || record.Version != selector.Version {
			return nil, ErrAuthentication
		}
		if !now.Before(record.ExpiresAt) {
			if record.InvitationID == bootstrapInvitationID && (len(s.state.Credentials) == 0 || s.rearmBootstrap) {
				previous := record.ExpiresAt
				record.ExpiresAt = now.Add(invitationLifetime)
				if err := s.persistLocked(); err != nil {
					record.ExpiresAt = previous
					return nil, fmt.Errorf("refresh bootstrap invitation: %w", err)
				}
			} else {
				s.state.Invitation = nil
				if err := s.persistLocked(); err != nil {
					s.state.Invitation = record
					return nil, fmt.Errorf("expire invitation: %w", err)
				}
				return nil, ErrInvitationExpired
			}
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
	record := s.state.Invitation
	if record == nil || record.InvitationID != selector.ID || record.Version != selector.Version {
		return ErrAuthentication
	}
	now := s.now().UTC()
	if !now.Before(record.ExpiresAt) {
		previous := record
		s.state.Invitation = nil
		if err := s.persistLocked(); err != nil {
			s.state.Invitation = previous
			return err
		}
		return ErrInvitationExpired
	}
	previous := *record
	record.FailedAttempts++
	burned := record.FailedAttempts >= maxInviteAttempts
	if burned {
		s.state.Invitation = nil
	} else {
		delay := time.Second << (record.FailedAttempts - 1)
		record.NextAttemptAt = now.Add(delay)
	}
	if err := s.persistLocked(); err != nil {
		if burned {
			s.state.Invitation = &previous
		} else {
			*record = previous
		}
		return fmt.Errorf("persist invitation attempt: %w", err)
	}
	if burned {
		return ErrInvitationBurned
	}
	return ErrAuthentication
}

func (s *Store) redeemInvitationLocked(selector transport.E2EEAuthSelector) (transport.E2EEAuthResult, error) {
	record := s.state.Invitation
	if record == nil || record.InvitationID != selector.ID || record.Version != selector.Version {
		return transport.E2EEAuthResult{}, ErrAuthentication
	}
	now := s.now().UTC()
	if !now.Before(record.ExpiresAt) {
		previous := record
		s.state.Invitation = nil
		if err := s.persistLocked(); err != nil {
			s.state.Invitation = previous
			return transport.E2EEAuthResult{}, err
		}
		return transport.E2EEAuthResult{}, ErrInvitationExpired
	}
	deviceID, err := s.uniqueDeviceIDLocked()
	if err != nil {
		return transport.E2EEAuthResult{}, err
	}
	credentialID, err := s.uniqueCredentialIDLocked()
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
	previousInvitation := record
	s.state.Invitation = nil
	s.state.Credentials = append(s.state.Credentials, credential)
	if err := s.persistLocked(); err != nil {
		s.state.Credentials = s.state.Credentials[:len(s.state.Credentials)-1]
		s.state.Invitation = previousInvitation
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
	record := &s.state.Credentials[index]
	if record.Revoked || record.Version != selector.Version {
		return transport.E2EEAuthResult{}, ErrAuthentication
	}
	previousLastSeen := record.LastSeenAt
	previousLocale := record.Locale
	record.LastSeenAt = s.now().UTC()
	record.Locale = string(localize.NormalizeLocale(selector.Locale))
	if err := s.persistLocked(); err != nil {
		record.LastSeenAt = previousLastSeen
		record.Locale = previousLocale
		return transport.E2EEAuthResult{}, fmt.Errorf("persist credential last seen: %w", err)
	}
	return transport.E2EEAuthResult{Identity: transport.AuthenticatedIdentity{
		DeviceID: record.DeviceID, CredentialID: record.CredentialID, Role: string(record.Role),
		Locale: record.Locale, CredentialVersion: record.Version,
	}}, nil
}

func (s *Store) uniqueDeviceIDLocked() (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		id, err := s.randomValue(identifierBytes)
		if err != nil {
			return "", err
		}
		unique := true
		for index := range s.state.Credentials {
			if s.state.Credentials[index].DeviceID == id {
				unique = false
				break
			}
		}
		if unique {
			return id, nil
		}
	}
	return "", errors.New("could not generate unique device identifier")
}

func (s *Store) uniqueCredentialIDLocked() (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		id, err := s.randomValue(identifierBytes)
		if err != nil {
			return "", err
		}
		if s.credentialIndex(id) < 0 {
			return id, nil
		}
	}
	return "", errors.New("could not generate unique credential identifier")
}

func decodeStoredSecret(encoded string) ([]byte, error) {
	secret, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(secret) != secretBytes {
		clear(secret)
		return nil, ErrAuthentication
	}
	return secret, nil
}
