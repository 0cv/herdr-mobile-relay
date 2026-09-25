//go:build !linux && !darwin

package deviceauth

import (
	"errors"
	"os"
)

func storeWriterLockSupported() bool { return false }

func openStoreWriterLockFile(string) (*os.File, error) {
	return nil, errors.New("cooperating device-store writer locks are unsupported on this platform")
}

func lockStoreWriterFile(*os.File) error {
	return errors.New("cooperating device-store writer locks are unsupported on this platform")
}

func unlockStoreWriterFile(*os.File) error { return nil }
