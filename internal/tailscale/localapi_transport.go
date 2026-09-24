package tailscale

import (
	"errors"
	"net/http"
	"strings"
)

// newPinnedLocalAPIRoundTripper keeps the upstream client's raw request and
// authentication implementation while adding endpoint, redirect, and response
// header bounds that DoLocalRequest's public seam does not expose.
func newPinnedLocalAPIRoundTripper() (http.RoundTripper, bool, error) {
	dial, omitAuth, err := platformLocalAPIDialer()
	if err != nil {
		return nil, true, err
	}
	transport := &http.Transport{
		DialContext:            dial,
		DisableCompression:     true,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: localAPIDiagnosticLimit,
	}
	return noRedirectLocalAPITransport{next: transport}, omitAuth, nil
}

type noRedirectLocalAPITransport struct {
	next http.RoundTripper
}

func (t noRedirectLocalAPITransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t.next == nil || !validLocalAPIRequest(request) {
		return nil, errors.New("invalid local Tailscale API endpoint")
	}
	response, err := t.next.RoundTrip(request)
	if err != nil || response == nil {
		return response, err
	}
	if isRedirectStatus(response.StatusCode) {
		header := response.Header.Clone()
		for name := range header {
			if strings.EqualFold(name, "Location") {
				delete(header, name)
			}
		}
		response.Header = header
	}
	return response, nil
}

func validLocalAPIRequest(request *http.Request) bool {
	if request == nil || request.URL == nil || request.URL.Scheme != "http" || request.URL.Host != localAPIHost || request.URL.User != nil || request.URL.Fragment != "" || request.URL.Opaque != "" || request.URL.RawPath != "" || request.URL.ForceQuery {
		return false
	}
	if request.Host != "" && request.Host != localAPIHost {
		return false
	}
	if localAPIHeaderPresent(request.Header, "Proxy-Authorization") {
		return false
	}
	switch request.URL.Path {
	case localAPIStatusPath:
		return request.Method == http.MethodGet && request.URL.RawQuery == ""
	case localAPIServeConfigPath:
		if request.URL.RawQuery != "" {
			return false
		}
		if request.Method == http.MethodGet {
			return !localAPIHeaderPresent(request.Header, "If-Match")
		}
		if request.Method != http.MethodPost {
			return false
		}
		etag, etagPresent := localAPIHeaderValues(request.Header, "If-Match")
		contentType, contentTypePresent := localAPIHeaderValues(request.Header, "Content-Type")
		if !etagPresent || len(etag) != 1 || !validServeETag(etag[0]) {
			return false
		}
		if !contentTypePresent || len(contentType) != 1 || contentType[0] != "application/json" {
			return false
		}
		return !localAPIHeaderPresent(request.Header, "Idempotency-Key") && !localAPIHeaderPresent(request.Header, "X-Idempotency-Key")
	case localAPIWatchPath:
		return request.Method == http.MethodGet && request.URL.RawQuery == localAPIWatchQuery
	default:
		return false
	}
}

func localAPIHeaderPresent(header http.Header, name string) bool {
	_, present := localAPIHeaderValues(header, name)
	return present
}

func localAPIHeaderValues(header http.Header, name string) ([]string, bool) {
	var values []string
	present := false
	for key, headerValues := range header {
		if strings.EqualFold(key, name) {
			present = true
			values = append(values, headerValues...)
		}
	}
	return values, present
}

func isRedirectStatus(status int) bool { return status >= 300 && status < 400 }

func unsupportedPlatformError() error {
	return errors.New("Tailscale LocalAPI transport is unsupported on this platform")
}
