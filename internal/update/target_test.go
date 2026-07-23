package update

import relayrelease "github.com/0cv/herdr-mobile-relay/internal/release"

func currentTargetForTest() string {
	return relayrelease.CurrentTarget()
}
