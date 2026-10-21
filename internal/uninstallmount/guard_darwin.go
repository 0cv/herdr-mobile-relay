package uninstallmount

import "errors"

// No Darwin mount-namespace/descriptor correspondence proof is implemented.
// In particular this is never a successful fallback for the Linux guard.
func Check(home, release, config, cache string) error {
	return errors.New("uninstall mount guard unsupported")
}

func CheckRoots(home string, roots ...string) error {
	return errors.New("uninstall mount guard unsupported")
}
