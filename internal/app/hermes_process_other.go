//go:build !darwin && !linux

package app

import (
	"context"
	"errors"
)

func hermesProcessStart(context.Context, int) (float64, error) {
	return 0, errors.New("process start unavailable")
}
