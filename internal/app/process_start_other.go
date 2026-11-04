//go:build !darwin && !linux

package app

import (
	"context"
	"errors"
	"time"
)

func processStartTime(context.Context, int) (time.Time, error) {
	return time.Time{}, errors.New("process start unavailable")
}
