package tailscale

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/safesocket"
)

const appStoreLocalAPIProfileEnabled = false
const appStoreLocalAPIDiscoveryTimeout = 2 * time.Second

// The App Store source cell stays empty until a separately reviewed exact
// installed-version profile is approved.
var appStoreLocalAPIProfiles []appStoreLocalAPIProfile

var (
	appStoreDiscover = safesocket.LocalTCPPortAndToken
	dialLoopbackTCP  = (&net.Dialer{}).DialContext
)

type appStoreLocalAPIProfile struct {
	osVariant      string
	gitCommit      string
	extraGitCommit string
	long           string
	daemonLong     string
	cap            int
}

func appStoreLocalAPIMetadataCandidate(data []byte) bool {
	fields, err := object(data)
	if err != nil {
		return false
	}
	for _, key := range []string{"osVariant", "extraGitCommit"} {
		var value string
		if scalar(fields, key, &value, false) == nil && value != "" {
			return true
		}
	}
	return false
}

func newAppStoreLocalAPI(expectedVersion string, versionMetadata []byte) (*localAPI, error) {
	return newAppStoreLocalAPIWith(runtime.GOOS, appStoreLocalAPIProfileEnabled, appStoreLocalAPIProfiles,
		expectedVersion, versionMetadata, appStoreDiscover, dialLoopbackTCP)
}

// newAppStoreLocalAPIWith keeps test-only synthetic profiles and transports
// injectable without exposing an enablement or endpoint control to callers.
func newAppStoreLocalAPIWith(
	goos string,
	enabled bool,
	profiles []appStoreLocalAPIProfile,
	expectedVersion string,
	versionMetadata []byte,
	discover func() (int, string, error),
	loopbackDial func(context.Context, string, string) (net.Conn, error),
) (*localAPI, error) {
	if err := checkLocalAPIRuntime(goos); err != nil {
		return nil, err
	}
	if goos != "darwin" {
		return nil, unsupportedPlatformError()
	}
	if !enabled {
		return nil, errLocalAPIUnsupportedVersion
	}
	profile, ok := matchAppStoreLocalAPIProfile(versionMetadata, expectedVersion, profiles)
	if !ok {
		return nil, errLocalAPIUnsupportedVersion
	}
	port, token, err := boundedAppStoreDiscovery(discover)
	if err != nil {
		return nil, err
	}
	dial := appStoreLocalAPIDialer(port, loopbackDial)
	transport := &http.Transport{
		DialContext:            dial,
		DisableCompression:     true,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: localAPIDiagnosticLimit,
	}
	return &localAPI{
		client: &local.Client{
			Dial:      dial,
			Transport: noRedirectLocalAPITransport{next: transport, basicAuthToken: localAPIAuthToken{value: token}},
			OmitAuth:  true,
		},
		expectedVersion: profile.long,
	}, nil
}

func matchAppStoreLocalAPIProfile(data []byte, expectedVersion string, profiles []appStoreLocalAPIProfile) (appStoreLocalAPIProfile, bool) {
	fields, actual, ok := parseAppStoreLocalAPIVersion(data, expectedVersion)
	if !ok {
		return appStoreLocalAPIProfile{}, false
	}
	for _, field := range []struct {
		name   string
		target *string
		req    bool
	}{
		{name: "osVariant", target: &actual.osVariant, req: true},
		{name: "gitCommit", target: &actual.gitCommit, req: true},
		{name: "extraGitCommit", target: &actual.extraGitCommit},
		{name: "long", target: &actual.long, req: true},
		{name: "daemonLong", target: &actual.daemonLong, req: true},
	} {
		if scalar(fields, field.name, field.target, field.req) != nil {
			return appStoreLocalAPIProfile{}, false
		}
	}
	for _, profile := range profiles {
		if profile.osVariant != "" && profile.gitCommit != "" && profile.long == expectedVersion &&
			profile.daemonLong == expectedVersion && actual == profile {
			return profile, true
		}
	}
	return appStoreLocalAPIProfile{}, false
}

func parseAppStoreLocalAPIVersion(data []byte, expectedVersion string) (map[string]json.RawMessage, appStoreLocalAPIProfile, bool) {
	fields, err := object(data)
	if err != nil || keys(fields, "majorMinorPatch", "short", "long", "gitCommit", "daemonLong", "isDev", "gitDirty", "unstableBranch", "extraGitCommit", "osVariant", "gitCommitTime", "tailscaleGoGitHash", "cap") != nil {
		return nil, appStoreLocalAPIProfile{}, false
	}
	values := map[string]string{}
	for _, key := range []string{"majorMinorPatch", "short", "long", "gitCommit", "daemonLong", "extraGitCommit", "osVariant", "gitCommitTime", "tailscaleGoGitHash"} {
		var value string
		required := key == "majorMinorPatch" || key == "short" || key == "long" || key == "gitCommit" || key == "daemonLong" || key == "osVariant"
		if scalar(fields, key, &value, required) != nil {
			return nil, appStoreLocalAPIProfile{}, false
		}
		values[key] = value
	}
	for _, key := range []string{"isDev", "gitDirty", "unstableBranch"} {
		var value bool
		if scalar(fields, key, &value, false) != nil || value {
			return nil, appStoreLocalAPIProfile{}, false
		}
	}
	var cap int
	if scalar(fields, "cap", &cap, true) != nil || cap < 0 ||
		values["majorMinorPatch"] != SourceRelease || values["short"] != SourceRelease ||
		values["osVariant"] == "" || values["long"] != expectedVersion ||
		values["daemonLong"] != expectedVersion || !validAppStoreGitCommit(values["gitCommit"]) {
		return nil, appStoreLocalAPIProfile{}, false
	}
	parts := strings.Split(values["long"], "-")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != SourceRelease {
		return nil, appStoreLocalAPIProfile{}, false
	}
	commitPrefix := strings.TrimPrefix(parts[1], "t")
	if len(commitPrefix) < 7 || !strings.HasPrefix(values["gitCommit"], commitPrefix) {
		return nil, appStoreLocalAPIProfile{}, false
	}
	extra := values["extraGitCommit"]
	if len(parts) == 3 {
		extraPrefix := strings.TrimPrefix(parts[2], "g")
		if len(extra) != 40 || len(extraPrefix) < 7 || !strings.HasPrefix(extra, extraPrefix) || !validAppStoreGitCommit(extra) {
			return nil, appStoreLocalAPIProfile{}, false
		}
	} else if extra != "" {
		return nil, appStoreLocalAPIProfile{}, false
	}
	return fields, appStoreLocalAPIProfile{
		osVariant:      values["osVariant"],
		gitCommit:      values["gitCommit"],
		extraGitCommit: extra,
		long:           values["long"],
		daemonLong:     values["daemonLong"],
		cap:            cap,
	}, true
}

func validAppStoreGitCommit(commit string) bool {
	if len(commit) != 40 {
		return false
	}
	for _, char := range commit {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return false
		}
	}
	return true
}

func boundedAppStoreDiscovery(discover func() (int, string, error)) (int, string, error) {
	return boundedAppStoreDiscoveryWithTimeout(discover, appStoreLocalAPIDiscoveryTimeout)
}

func boundedAppStoreDiscoveryWithTimeout(discover func() (int, string, error), timeout time.Duration) (int, string, error) {
	if discover == nil || timeout <= 0 {
		return 0, "", errLocalAPIDiscovery
	}
	type result struct {
		port  int
		token string
		err   error
	}
	resultCh := make(chan result, 1)
	deadline := time.Now().Add(timeout)
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	go func() {
		defer func() {
			if recover() != nil {
				resultCh <- result{err: errLocalAPIDiscovery}
			}
		}()
		port, token, err := discover()
		resultCh <- result{port: port, token: token, err: err}
	}()
	select {
	case <-timer.C:
		return 0, "", errLocalAPIDiscovery
	case found := <-resultCh:
		if !time.Now().Before(deadline) || found.err != nil || !validAppStorePort(found.port) || !validAppStoreToken(found.token) {
			return 0, "", errLocalAPIDiscovery
		}
		return found.port, found.token, nil
	}
}

func validAppStorePort(port int) bool { return port >= 1 && port <= 65535 }

func validAppStoreToken(token string) bool {
	if len(token) < 1 || len(token) > 256 {
		return false
	}
	for i := 0; i < len(token); i++ {
		if token[i] < 0x21 || token[i] > 0x7e || token[i] == ':' {
			return false
		}
	}
	return true
}

func appStoreLocalAPIDialer(port int, dialLoopback func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if ctx == nil || network != "tcp" || address != localAPIHost+":80" || !validAppStorePort(port) || dialLoopback == nil {
			return nil, errLocalAPIRequest
		}
		conn, err := dialLoopback(ctx, "tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil || conn == nil {
			return nil, errLocalAPIRequest
		}
		return conn, nil
	}
}
