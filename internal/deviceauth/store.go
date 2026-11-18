package deviceauth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/0cv/herdr-mobile-relay/internal/atomicfile"
)

const (
	storeSchemaVersion    = 1
	storeFilename         = "devices.json"
	secretBytes           = 32
	identifierBytes       = 18
	invitationLifetime    = 10 * time.Minute
	bootstrapInvitationID = "bootstrap"
	maxInviteAttempts     = 5
	maxNameBytes          = 80
	maxLocaleBytes        = 32
)

type Role string

const (
	RoleController Role = "controller"
	RoleReader     Role = "reader"
)

var (
	ErrNotFound          = errors.New("device credential not found")
	ErrRevoked           = errors.New("device credential revoked")
	ErrLastController    = errors.New("cannot revoke the last controller")
	ErrInvalidRole       = errors.New("invalid device role")
	ErrInvalidName       = errors.New("invalid device name")
	ErrInvalidLocale     = errors.New("invalid device locale")
	ErrInvitationExpired = errors.New("invitation expired")
	ErrInvitationBurned  = errors.New("invitation burned")
	ErrRateLimited       = errors.New("invitation attempt rate limited")
	ErrAuthentication    = errors.New("device authentication failed")
)

type Credential struct {
	DeviceID     string    `json:"device_id"`
	CredentialID string    `json:"credential_id"`
	Name         string    `json:"name"`
	Role         Role      `json:"role"`
	Locale       string    `json:"locale"`
	PairedAt     time.Time `json:"paired_at"`
	LastSeenAt   time.Time `json:"last_seen_at,omitempty"`
	Version      uint64    `json:"version"`
	Revoked      bool      `json:"revoked"`
	Current      bool      `json:"current,omitempty"`
}

type Invitation struct {
	InvitationID string    `json:"invitation_id"`
	Version      uint64    `json:"version"`
	Secret       string    `json:"secret"`
	ExpiresAt    time.Time `json:"expires_at"`
	Name         string    `json:"name"`
	Role         Role      `json:"role"`
	Locale       string    `json:"locale"`
}

type credentialRecord struct {
	Credential
	Secret string `json:"secret,omitempty"`
}

type invitationRecord struct {
	Invitation
	FailedAttempts      int       `json:"failed_attempts"`
	NextAttemptAt       time.Time `json:"next_attempt_at,omitempty"`
	PendingCredentialID string    `json:"pending_credential_id,omitempty"`
}

type diskState struct {
	SchemaVersion int                `json:"schema_version"`
	Invitation    *invitationRecord  `json:"invitation,omitempty"`
	Credentials   []credentialRecord `json:"credentials"`
}

func (state diskState) clone() diskState {
	state.Credentials = slices.Clone(state.Credentials)
	if state.Invitation != nil {
		invitation := *state.Invitation
		state.Invitation = &invitation
	}
	return state
}

type Option func(*Store)

// WithBootstrapReenrollment lets every process start mint a fresh one-use
// bootstrap invitation even when devices are already enrolled. The quick
// tunnel serves the phone app from a hostname that changes on every launch,
// so a phone stores its credential under an origin that no longer exists and
// can only ever return through a new enrollment. Stable installs do not set
// this: their bootstrap stays strictly one-use.
func WithBootstrapReenrollment() Option {
	return func(store *Store) { store.rearmBootstrap = true }
}

type Store struct {
	mu             sync.Mutex
	dir            string
	path           string
	now            func() time.Time
	random         io.Reader
	rearmBootstrap bool
	state          diskState
}

func Open(dir string, options ...Option) (*Store, error) {
	store := &Store{
		dir:    dir,
		path:   filepath.Join(dir, storeFilename),
		now:    time.Now,
		random: rand.Reader,
		state: diskState{
			SchemaVersion: storeSchemaVersion,
			Credentials:   make([]credentialRecord, 0),
		},
	}
	for _, option := range options {
		option(store)
	}
	if err := protectDirectory(dir); err != nil {
		return nil, err
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	if err := store.commitLocked(store.state); err != nil {
		return nil, fmt.Errorf("initialize device store: %w", err)
	}
	return store, nil
}

func (s *Store) CreateInvitation(name string, role Role, locale string) (Invitation, error) {
	name, locale, err := validateMetadata(name, role, locale)
	if err != nil {
		return Invitation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := s.randomValue(identifierBytes)
	if err != nil {
		return Invitation{}, err
	}
	secret, err := s.randomValue(secretBytes)
	if err != nil {
		return Invitation{}, err
	}
	now := s.now().UTC()
	record := &invitationRecord{Invitation: Invitation{
		InvitationID: id,
		Version:      1,
		Secret:       secret,
		ExpiresAt:    now.Add(invitationLifetime),
		Name:         name,
		Role:         role,
		Locale:       locale,
	}}
	if err := s.replaceInvitationLocked(record); err != nil {
		return Invitation{}, fmt.Errorf("persist invitation: %w", err)
	}
	return record.Invitation, nil
}

func newBootstrapInvitation(secret []byte, name, locale string, now time.Time) (*invitationRecord, error) {
	name, locale, err := validateMetadata(name, RoleController, locale)
	if err != nil {
		return nil, err
	}
	if len(secret) != secretBytes {
		return nil, errors.New("bootstrap device secret must be 32 bytes")
	}
	return &invitationRecord{Invitation: Invitation{
		InvitationID: bootstrapInvitationID,
		Version:      1,
		Secret:       base64.RawURLEncoding.EncodeToString(secret),
		ExpiresAt:    now.Add(invitationLifetime),
		Name:         name,
		Role:         RoleController,
		Locale:       locale,
	}}, nil
}

func (s *Store) EnsureBootstrapInvitation(secret []byte, name, locale string) error {
	now := s.now().UTC()
	bootstrap, err := newBootstrapInvitation(secret, name, locale, now)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.state.Credentials) > 0 && !s.rearmBootstrap {
		return nil
	}
	if record := s.state.Invitation; record != nil && now.Before(record.ExpiresAt) &&
		(!s.rearmBootstrap || record.FailedAttempts < maxInviteAttempts) {
		return nil
	}
	if err := s.replaceInvitationLocked(bootstrap); err != nil {
		return fmt.Errorf("persist bootstrap invitation: %w", err)
	}
	return nil
}

// ArmBootstrapInvitation mints a bootstrap invitation while keeping every
// enrolled device. The operator asks for it by printing the setup link again,
// so each print pairs exactly one more phone.
func (s *Store) ArmBootstrapInvitation(secret []byte, name, locale string) error {
	bootstrap, err := newBootstrapInvitation(secret, name, locale, s.now().UTC())
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.replaceInvitationLocked(bootstrap); err != nil {
		return fmt.Errorf("persist bootstrap invitation: %w", err)
	}
	return nil
}

func (s *Store) replaceInvitationLocked(record *invitationRecord) error {
	next := s.state.clone()
	next.Invitation = record
	return s.commitLocked(next)
}

func (s *Store) ListCredentials(currentCredentialID string) []Credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	credentials := make([]Credential, len(s.state.Credentials))
	for index := range s.state.Credentials {
		credentials[index] = s.state.Credentials[index].Credential
		credentials[index].Current = credentials[index].CredentialID == currentCredentialID
	}
	return credentials
}
func (s *Store) AuthorizeCredential(credentialID string, version uint64) (Credential, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.credentialIndex(credentialID)
	if index < 0 {
		return Credential{}, false
	}
	credential := s.state.Credentials[index].Credential
	if credential.Revoked || credential.Version != version {
		return Credential{}, false
	}
	return credential, true
}

func (s *Store) RenameCredential(credentialID, name string) (Credential, error) {
	name = strings.TrimSpace(name)
	if !validText(name, maxNameBytes) {
		return Credential{}, ErrInvalidName
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.credentialIndex(credentialID)
	if index < 0 {
		return Credential{}, ErrNotFound
	}
	next := s.state.clone()
	next.Credentials[index].Name = name
	if err := s.commitLocked(next); err != nil {
		return Credential{}, fmt.Errorf("persist credential rename: %w", err)
	}
	return s.state.Credentials[index].Credential, nil
}

func (s *Store) RevokeCredential(credentialID string) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.credentialIndex(credentialID)
	if index < 0 {
		return Credential{}, ErrNotFound
	}
	current := s.state.Credentials[index]
	if !current.Revoked && current.Role == RoleController && s.activeControllerCountLocked() == 1 {
		return Credential{}, ErrLastController
	}
	next := s.state.clone()
	record := &next.Credentials[index]
	if !record.Revoked {
		record.Revoked = true
		record.Version++
		record.Secret = ""
	}
	if invitation := next.Invitation; invitation != nil &&
		invitation.PendingCredentialID == credentialID {
		next.Invitation = nil
	}
	if err := s.commitLocked(next); err != nil {
		return Credential{}, fmt.Errorf("persist credential revocation: %w", err)
	}
	return s.state.Credentials[index].Credential, nil
}

func (s *Store) ResetWithBootstrap(secret []byte, name, locale string) error {
	bootstrap, err := newBootstrapInvitation(secret, name, locale, s.now().UTC())
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := diskState{
		SchemaVersion: storeSchemaVersion,
		Invitation:    bootstrap,
		Credentials:   make([]credentialRecord, 0),
	}
	if err := s.commitLocked(next); err != nil {
		return fmt.Errorf("persist device reset: %w", err)
	}
	return nil
}

func (s *Store) activeControllerCountLocked() int {
	count := 0
	for index := range s.state.Credentials {
		record := s.state.Credentials[index]
		if !record.Revoked && record.Role == RoleController {
			count++
		}
	}
	return count
}

func (s *Store) credentialIndex(credentialID string) int {
	for index := range s.state.Credentials {
		if s.state.Credentials[index].CredentialID == credentialID {
			return index
		}
	}
	return -1
}

func (s *Store) randomValue(size int) (string, error) {
	value := make([]byte, size)
	if _, err := io.ReadFull(s.random, value); err != nil {
		return "", fmt.Errorf("generate device credential: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (s *Store) load() error {
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect device store: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("device store is not a regular file")
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		return fmt.Errorf("protect device store: %w", err)
	}
	file, err := os.Open(s.path)
	if err != nil {
		return fmt.Errorf("open device store: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 4<<20))
	decoder.DisallowUnknownFields()
	var state diskState
	if err := decoder.Decode(&state); err != nil {
		return fmt.Errorf("decode device store: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return err
	}
	if state.SchemaVersion != storeSchemaVersion {
		return fmt.Errorf("unsupported device store schema %d", state.SchemaVersion)
	}
	if state.Credentials == nil {
		state.Credentials = make([]credentialRecord, 0)
	}
	if err := validateState(state); err != nil {
		return fmt.Errorf("invalid device store: %w", err)
	}
	s.state = state
	return nil
}

func (s *Store) commitLocked(next diskState) error {
	if err := s.writeState(next); err != nil {
		return err
	}
	s.state = next
	return nil
}

func (s *Store) writeState(state diskState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = atomicfile.Replace(s.dir, s.path, ".devices-*.tmp", data, 0o600)
	return err
}

func protectDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create device store directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect device store directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("device store path is not a directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("protect device store directory: %w", err)
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("device store contains multiple JSON values")
		}
		return fmt.Errorf("decode device store trailer: %w", err)
	}
	return nil
}

func validateState(state diskState) error {
	seenDevices := make(map[string]struct{}, len(state.Credentials))
	seenCredentials := make(map[string]struct{}, len(state.Credentials))
	activeCredentials := make(map[string]struct{}, len(state.Credentials))
	for _, record := range state.Credentials {
		if record.DeviceID == "" || record.CredentialID == "" || record.Version == 0 {
			return errors.New("credential identity is incomplete")
		}
		if _, exists := seenDevices[record.DeviceID]; exists {
			return errors.New("duplicate device identifier")
		}
		if _, exists := seenCredentials[record.CredentialID]; exists {
			return errors.New("duplicate credential identifier")
		}
		seenDevices[record.DeviceID] = struct{}{}
		seenCredentials[record.CredentialID] = struct{}{}
		if _, _, err := validateMetadata(record.Name, record.Role, record.Locale); err != nil {
			return err
		}
		if record.Revoked {
			if record.Secret != "" {
				return errors.New("revoked credential retains a secret")
			}
		} else {
			if !validSecret(record.Secret) {
				return errors.New("credential secret is invalid")
			}
			activeCredentials[record.CredentialID] = struct{}{}
		}
	}
	if invitation := state.Invitation; invitation != nil {
		if invitation.InvitationID == "" || invitation.Version == 0 || !validSecret(invitation.Secret) {
			return errors.New("invitation identity is invalid")
		}
		if invitation.FailedAttempts < 0 || invitation.FailedAttempts >= maxInviteAttempts {
			return errors.New("invitation attempt count is invalid")
		}
		if _, _, err := validateMetadata(invitation.Name, invitation.Role, invitation.Locale); err != nil {
			return err
		}
		if invitation.PendingCredentialID != "" {
			if _, exists := activeCredentials[invitation.PendingCredentialID]; !exists {
				return errors.New("invitation pending credential is invalid")
			}
		}
	}
	return nil
}

func validateMetadata(name string, role Role, locale string) (string, string, error) {
	name = strings.TrimSpace(name)
	locale = strings.TrimSpace(locale)
	if !validText(name, maxNameBytes) {
		return "", "", ErrInvalidName
	}
	if role != RoleController && role != RoleReader {
		return "", "", ErrInvalidRole
	}
	if !validText(locale, maxLocaleBytes) || strings.ContainsAny(locale, " /\\") {
		return "", "", ErrInvalidLocale
	}
	return name, locale, nil
}

func validText(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validSecret(encoded string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	return err == nil && len(decoded) == secretBytes
}
