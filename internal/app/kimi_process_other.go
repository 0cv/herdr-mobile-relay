//go:build !darwin

package app

func kimiProcessStart(int) (string, error) {
	return "", nil
}
