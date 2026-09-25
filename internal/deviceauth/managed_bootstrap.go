package deviceauth

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrManagedArmRecovery means an invitation write failed and the exact prior
// device-store state could not be restored without risking foreign state.
var ErrManagedArmRecovery = errors.New("managed device-store rollback requires recovery")

type managedStoreSnapshot struct {
	dirExists  bool
	dirInfo    os.FileInfo
	dirMode    os.FileMode
	fileExists bool
	fileInfo   os.FileInfo
	fileMode   os.FileMode
	fileBytes  []byte
}

// OpenDeferred reads and validates the managed device store without creating,
// chmodding, or rewriting any path. The first persistent write is the explicit
// transactional invitation arm below.
func OpenDeferred(dir string, options ...Option) (*Store, error) {
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
	if store.now == nil || store.random == nil {
		return nil, errors.New("device store requires clock and random source")
	}
	before, err := readManagedStoreSnapshot(store.dir, store.path)
	if err != nil {
		return nil, err
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	after, err := readManagedStoreSnapshot(store.dir, store.path)
	if err != nil {
		return nil, err
	}
	if !sameManagedStoreSnapshot(before, after) {
		return nil, errors.New("device store changed while being opened read-only")
	}
	store.managedArmBaseline = &after
	return store, nil
}

// ArmBootstrapInvitationTransactional writes the bootstrap invitation while
// retaining exact rollback state until the durable write succeeds. It never
// resets credentials. Callers serialize this operation with the pairing gate
// and may supply a final admission check that runs after durable installation;
// a refusal there restores the exact prior snapshot before returning.
func (s *Store) ArmBootstrapInvitationTransactional(secret []byte, name, locale string, beforeCommit ...func() error) error {
	if len(beforeCommit) > 1 {
		return errors.New("managed invitation arm accepts at most one final admission check")
	}
	name, locale, err := validateMetadata(name, RoleController, locale)
	if err != nil {
		return err
	}
	if len(secret) != secretBytes {
		return errors.New("bootstrap device secret must be 32 bytes")
	}
	if s == nil {
		return errors.New("managed device store is unavailable")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	writer, err := acquireStoreWriterLock(s.dir)
	if err != nil {
		return err
	}
	defer writer.Close()
	if s.managedArmBaseline == nil {
		return errors.New("managed device store was not opened read-only")
	}
	baseline := *s.managedArmBaseline
	current, err := readManagedStoreSnapshot(s.dir, s.path)
	if err != nil {
		return err
	}
	if !sameManagedStoreSnapshot(baseline, current) {
		return errors.New("device store changed before managed invitation arm")
	}

	var createdDir os.FileInfo
	if !baseline.dirExists {
		if err := os.Mkdir(s.dir, 0o700); err != nil {
			return fmt.Errorf("create managed device store directory: %w", err)
		}
		createdDir, err = os.Lstat(s.dir)
		if err != nil {
			return errors.Join(fmt.Errorf("inspect created managed device store directory: %w", err), fmt.Errorf("%w: directory identity is unavailable", ErrManagedArmRecovery))
		}
		if !createdDir.IsDir() || createdDir.Mode()&os.ModeSymlink != 0 {
			cleanupErr := removeCreatedStoreDirectory(s.dir, createdDir)
			return errors.Join(errors.New("created managed device store directory identity is invalid"), managedArmRecoveryError(cleanupErr))
		}
		if err := syncDirectory(filepath.Dir(s.dir)); err != nil {
			cleanupErr := removeCreatedStoreDirectory(s.dir, createdDir)
			return errors.Join(fmt.Errorf("sync managed device store parent: %w", err), managedArmRecoveryError(cleanupErr))
		}
	}
	if err := validateManagedStoreDirectory(s.dir); err != nil {
		cleanupErr := removeCreatedStoreDirectory(s.dir, createdDir)
		return errors.Join(err, managedArmRecoveryError(cleanupErr))
	}

	previousInvitation := s.state.Invitation
	now := s.now().UTC()
	s.state.Invitation = &invitationRecord{
		InvitationID: bootstrapInvitationID,
		Version:      1,
		Secret:       encodeSecret(secret),
		ExpiresAt:    now.Add(invitationLifetime),
		Name:         name,
		Role:         RoleController,
		Locale:       locale,
	}
	data, err := json.Marshal(s.state)
	if err == nil {
		data = append(data, '\n')
	}
	var committed os.FileInfo
	if err == nil {
		writeBaseline := baseline
		if createdDir != nil {
			writeBaseline.dirExists = true
			writeBaseline.dirInfo = createdDir
			writeBaseline.dirMode = 0o700
		}
		committed, err = s.persistManagedArm(data, writeBaseline)
	}
	if err != nil {
		s.state.Invitation = previousInvitation
		rollbackErr := s.rollbackManagedArm(baseline, createdDir, committed)
		if rollbackErr != nil {
			return errors.Join(fmt.Errorf("persist bootstrap invitation: %w", err), fmt.Errorf("%w: %v", ErrManagedArmRecovery, rollbackErr))
		}
		if restored, snapshotErr := readManagedStoreSnapshot(s.dir, s.path); snapshotErr == nil {
			s.managedArmBaseline = &restored
		} else {
			return errors.Join(fmt.Errorf("persist bootstrap invitation: %w", err), fmt.Errorf("%w: verify restored device store: %v", ErrManagedArmRecovery, snapshotErr))
		}
		return fmt.Errorf("persist bootstrap invitation: %w", err)
	}
	if len(beforeCommit) > 0 && beforeCommit[0] != nil {
		if err := beforeCommit[0](); err != nil {
			s.state.Invitation = previousInvitation
			rollbackErr := s.rollbackManagedArm(baseline, createdDir, committed)
			if rollbackErr != nil {
				return errors.Join(fmt.Errorf("managed invitation admission changed before commit: %w", err), fmt.Errorf("%w: %v", ErrManagedArmRecovery, rollbackErr))
			}
			if restored, snapshotErr := readManagedStoreSnapshot(s.dir, s.path); snapshotErr == nil {
				s.managedArmBaseline = &restored
			} else {
				return errors.Join(fmt.Errorf("managed invitation admission changed before commit: %w", err), fmt.Errorf("%w: verify restored device store: %v", ErrManagedArmRecovery, snapshotErr))
			}
			return fmt.Errorf("managed invitation admission changed before commit: %w", err)
		}
	}

	committedSnapshot := managedStoreSnapshot{
		dirExists:  true,
		dirInfo:    createdDir,
		dirMode:    0o700,
		fileExists: true,
		fileInfo:   committed,
		fileMode:   0o600,
		fileBytes:  append([]byte(nil), data...),
	}
	if baseline.dirExists {
		committedSnapshot.dirInfo = baseline.dirInfo
		committedSnapshot.dirMode = baseline.dirMode
	}
	s.managedArmBaseline = &committedSnapshot
	return nil
}

func (s *Store) persistManagedArm(data []byte, baseline managedStoreSnapshot) (os.FileInfo, error) {
	temp, err := os.CreateTemp(s.dir, ".devices-arm-*.tmp")
	if err != nil {
		return nil, err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return nil, err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return nil, err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return nil, err
	}
	installedInfo, err := temp.Stat()
	if err != nil {
		_ = temp.Close()
		return nil, err
	}
	if err := temp.Close(); err != nil {
		return nil, err
	}
	current, err := readManagedStoreSnapshot(s.dir, s.path)
	if err != nil {
		return nil, err
	}
	if !sameManagedStoreSnapshot(baseline, current) {
		return nil, errors.New("device store changed during managed invitation arm")
	}
	if s.managedArmFault != nil {
		if err := s.managedArmFault("before-rename"); err != nil {
			return nil, err
		}
	}
	// Recheck after the deterministic seam while still holding the shared
	// cooperating-writer lock. A raw same-UID writer can still race this
	// final check and rename; that unsupported residual is documented.
	current, err = readManagedStoreSnapshot(s.dir, s.path)
	if err != nil {
		return nil, err
	}
	if !sameManagedStoreSnapshot(baseline, current) {
		return nil, errors.New("device store changed at the managed write boundary")
	}
	if err := os.Rename(tempPath, s.path); err != nil {
		return nil, err
	}
	if s.managedArmFault != nil {
		if err := s.managedArmFault("after-rename"); err != nil {
			return installedInfo, err
		}
	}
	if err := os.Chmod(s.path, 0o600); err != nil {
		return installedInfo, err
	}
	if err := syncDirectory(s.dir); err != nil {
		return installedInfo, err
	}
	return installedInfo, nil
}

func (s *Store) rollbackManagedArm(baseline managedStoreSnapshot, createdDir, committed os.FileInfo) error {
	current, err := readManagedStoreSnapshot(s.dir, s.path)
	if err != nil {
		return err
	}
	if committed == nil {
		expected := baseline
		if createdDir != nil {
			expected.dirExists = true
			expected.dirInfo = createdDir
			expected.dirMode = 0o700
		}
		if !sameManagedStoreSnapshot(expected, current) {
			return errors.New("device store changed before a managed write was committed")
		}
		return removeCreatedStoreDirectory(s.dir, createdDir)
	}
	if !current.fileExists || !os.SameFile(committed, current.fileInfo) {
		return errors.New("device store target was replaced after managed write; replacement was preserved")
	}
	if createdDir != nil {
		if !current.dirExists || !os.SameFile(createdDir, current.dirInfo) {
			return errors.New("managed device store directory was replaced; replacement was preserved")
		}
	} else if !current.dirExists || !os.SameFile(baseline.dirInfo, current.dirInfo) {
		return errors.New("managed device store directory changed during rollback")
	}
	if baseline.fileExists {
		if err := restoreManagedStoreFile(s.dir, s.path, committed, baseline.fileBytes, baseline.fileMode); err != nil {
			return err
		}
	} else {
		if err := os.Remove(s.path); err != nil {
			return fmt.Errorf("remove own uncommitted device store: %w", err)
		}
		if err := syncDirectory(s.dir); err != nil {
			return fmt.Errorf("sync removal of uncommitted device store: %w", err)
		}
	}
	return removeCreatedStoreDirectory(s.dir, createdDir)
}

func restoreManagedStoreFile(dir, path string, expected os.FileInfo, data []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(dir, ".devices-rollback-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(expected, current) {
		return errors.New("device store target changed during rollback; replacement was preserved")
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	if err := syncDirectory(dir); err != nil {
		return err
	}
	return nil
}

func readManagedStoreSnapshot(dir, path string) (managedStoreSnapshot, error) {
	snapshot := managedStoreSnapshot{}
	dirInfo, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil {
		return snapshot, fmt.Errorf("inspect managed device store directory: %w", err)
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return snapshot, errors.New("managed device store directory is not a real directory")
	}
	if dirInfo.Mode().Perm() != 0o700 {
		return snapshot, fmt.Errorf("managed device store directory mode %04o is not 0700", dirInfo.Mode().Perm())
	}
	if uid, ok := fileOwner(dirInfo); !ok || uid != uint32(os.Getuid()) {
		return snapshot, errors.New("managed device store directory is not owned by the current user")
	}
	snapshot.dirExists = true
	snapshot.dirInfo = dirInfo
	snapshot.dirMode = dirInfo.Mode().Perm()

	fileInfo, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil {
		return snapshot, fmt.Errorf("inspect managed device store file: %w", err)
	}
	if fileInfo.Mode()&os.ModeSymlink != 0 || !fileInfo.Mode().IsRegular() || fileNlink(fileInfo) > 1 {
		return snapshot, errors.New("managed device store file is not a private unlinked regular file")
	}
	if fileInfo.Mode().Perm() != 0o600 {
		return snapshot, fmt.Errorf("managed device store file mode %04o is not 0600", fileInfo.Mode().Perm())
	}
	if uid, ok := fileOwner(fileInfo); !ok || uid != uint32(os.Getuid()) {
		return snapshot, errors.New("managed device store file is not owned by the current user")
	}
	if fileInfo.Size() < 0 || fileInfo.Size() > 4<<20 {
		return snapshot, errors.New("managed device store file exceeds its 4 MiB limit")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return snapshot, fmt.Errorf("read managed device store file: %w", err)
	}
	snapshot.fileExists = true
	snapshot.fileInfo = fileInfo
	snapshot.fileMode = fileInfo.Mode().Perm()
	snapshot.fileBytes = data
	return snapshot, nil
}

func sameManagedStoreSnapshot(a, b managedStoreSnapshot) bool {
	if a.dirExists != b.dirExists || a.fileExists != b.fileExists {
		return false
	}
	if a.dirExists && (!os.SameFile(a.dirInfo, b.dirInfo) || a.dirMode != b.dirMode) {
		return false
	}
	if !a.fileExists {
		return true
	}
	return os.SameFile(a.fileInfo, b.fileInfo) && a.fileMode == b.fileMode && bytes.Equal(a.fileBytes, b.fileBytes)
}

func validateManagedStoreDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("managed device store directory changed or is not private")
	}
	if uid, ok := fileOwner(info); !ok || uid != uint32(os.Getuid()) {
		return errors.New("managed device store directory is not owned by the current user")
	}
	return nil
}

func managedArmRecoveryError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %v", ErrManagedArmRecovery, err)
}

func removeCreatedStoreDirectory(dir string, created os.FileInfo) error {
	if created == nil {
		return nil
	}
	current, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(created, current) {
		return errors.New("managed device store directory was replaced; replacement was preserved")
	}
	if err := os.Remove(dir); err != nil {
		return fmt.Errorf("remove newly created managed device store directory: %w", err)
	}
	if err := syncDirectory(filepath.Dir(dir)); err != nil {
		return fmt.Errorf("sync removal of managed device store directory: %w", err)
	}
	return nil
}

func encodeSecret(secret []byte) string {
	return base64.RawURLEncoding.EncodeToString(secret)
}
