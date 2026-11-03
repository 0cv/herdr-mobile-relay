package backend_test

import (
	"github.com/0cv/herdr-mobile-relay/internal/backend"
	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

var _ backend.Backend = (*herdr.Client)(nil)
