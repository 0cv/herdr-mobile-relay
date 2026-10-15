//go:build !darwin && !linux

package ownedproc

import (
	"context"
	"errors"
)

type Native struct{}

func (Native) Lookup(context.Context, int) (Process, error) { return Process{}, errors.ErrUnsupported }
func (Native) Table(context.Context) ([]Process, error)     { return nil, errors.ErrUnsupported }
func (Native) Signal(int, bool) error                       { return errors.ErrUnsupported }
