package pibridge

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

var activityFixtureIndex atomic.Uint64

func testActivityInstance(t *testing.T) string {
	t.Helper()
	if selected := os.Getenv("HERDR_TEST_PI_INSTANCES"); selected != "" {
		instances := strings.Split(selected, ",")
		instance := instances[(activityFixtureIndex.Add(1)-1)%uint64(len(instances))]
		decoded, err := hex.DecodeString(instance)
		if err != nil || len(decoded) != 32 || len(instances) < 8 {
			t.Fatal("Pi activity fixtures require at least eight preallocated 32-byte instance identities")
		}
		return instance
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(nonce[:])
}
