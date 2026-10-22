package doctor

import (
	"errors"

	"github.com/0cv/herdr-mobile-relay/internal/darwinacl"
)

// Package admission is descriptor-bound: absence means nothing without the
// same descriptor's local, ownership-honouring, ACL-capable volume evidence.
// Only ancestors may have deny-only ACLs; every package object needs no ACL.
type packageACLProbe struct {
	inspect    func(int) (darwinacl.ACL, error)
	filesystem func(int) error
}

var productionPackageACLProbe = packageACLProbe{inspect: darwinacl.Inspect, filesystem: darwinacl.Filesystem}

var errPackageEvidence = errors.New("unavailable package writer evidence")

func (p packageACLProbe) objectSafe(fd int, ancestor bool) error {
	if p.filesystem == nil || p.inspect == nil {
		return errPackageEvidence
	}
	if err := p.filesystem(fd); err != nil {
		return errPackageEvidence
	}
	acl, err := p.inspect(fd)
	if err != nil || (ancestor && !acl.DirectoryOK()) || (!ancestor && !acl.FileOK()) {
		return errPackageEvidence
	}
	return nil
}
