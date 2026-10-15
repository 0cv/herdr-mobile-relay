//go:build !darwin && !linux

package p1fixture

func childBirth(int) (string, error) { return "", ErrContract }
