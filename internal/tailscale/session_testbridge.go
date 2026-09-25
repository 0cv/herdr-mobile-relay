//go:build herdr_tailscale_test

package tailscale

import (
	"net/http"

	"tailscale.com/client/local"
)

// NewSessionAuthorityWithRawLocalAPIForTest constructs the real authority over
// a raw protocol RoundTripper. This bridge is present only in explicit hosted
// integration-test builds; the endpoint allowlist and response validation are
// unchanged, and it exposes no ownership or readiness injection.
func NewSessionAuthorityWithRawLocalAPIForTest(httpsTransport http.RoundTripper, httpsPort, backendPort int) (*SessionAuthority, error) {
	if httpsTransport == nil {
		return nil, errLocalAPIRequest
	}
	const expectedVersion = "1.102.4-tbbcd7d1fc"
	metadata := []byte(`{"majorMinorPatch":"1.102.4","short":"1.102.4","long":"1.102.4-tbbcd7d1fc","gitCommit":"bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8","daemonLong":"1.102.4-tbbcd7d1fc","cap":141}`)
	if !localAPIVersionMetadataAllowed(metadata, expectedVersion) {
		return nil, errLocalAPIUnsupportedVersion
	}
	api := &localAPI{
		client:          &local.Client{Transport: noRedirectLocalAPITransport{next: httpsTransport}, OmitAuth: true},
		expectedVersion: expectedVersion,
	}
	return newSessionAuthority(api, httpsPort, backendPort)
}
