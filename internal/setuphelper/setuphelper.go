package setuphelper

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

func SetupFragment(token, label, relay string) string {
	values := url.Values{}
	values.Set("setup", token)
	values.Set("label", label)
	if relay != "" {
		values.Set("relay", relay)
	}
	return values.Encode()
}

// NormalizeExternalHTTPSOrigin accepts only a canonical, user-supplied HTTPS
// origin. Unlike NormalizeOrigin, it never supplies a scheme or trims a path;
// callers can therefore distinguish an operator's exact Serve address from a
// URL that would otherwise be silently rewritten.
func NormalizeExternalHTTPSOrigin(value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value || !strings.HasPrefix(value, "https://") {
		return "", errors.New("external Serve origin must be a canonical HTTPS origin")
	}
	if strings.ContainsAny(strings.TrimPrefix(value, "https://"), "/?#\\\\") {
		return "", errors.New("external Serve origin must not contain a path, query, fragment, or backslash")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery {
		return "", errors.New("external Serve origin must be a canonical HTTPS origin")
	}
	host := parsed.Hostname()
	if host == "" || strings.ContainsAny(parsed.Host, "%@") {
		return "", errors.New("external Serve origin has an invalid host")
	}
	canonicalHost := strings.ToLower(host)
	if ip := net.ParseIP(host); ip != nil {
		canonicalHost = ip.String()
	} else if !validOriginHostname(canonicalHost) {
		return "", errors.New("external Serve origin has an invalid host")
	}
	port := parsed.Port()
	if port != "" {
		number, portErr := strconv.Atoi(port)
		if portErr != nil || number < 1 || number > 65535 || strconv.Itoa(number) != port || number == 443 {
			return "", errors.New("external Serve origin has a non-canonical port")
		}
	}
	canonicalAuthority := canonicalHost
	if strings.Contains(canonicalHost, ":") {
		canonicalAuthority = "[" + canonicalHost + "]"
	}
	if port != "" {
		canonicalAuthority = net.JoinHostPort(canonicalHost, port)
	}
	canonical := "https://" + canonicalAuthority
	if value != canonical {
		return "", errors.New("external Serve origin is not canonical")
	}
	return canonical, nil
}

func validOriginHostname(host string) bool {
	if host == "" || len(host) > 253 || strings.HasSuffix(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
				return false
			}
		}
	}
	return true
}

func NormalizeOrigin(value string, allowLoopbackHTTP bool) (string, error) {
	value = strings.TrimSpace(value)
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", err
	}
	if parsed.User != nil || parsed.Host == "" || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("origin must not contain credentials, a path, query, or fragment")
	}
	hostname := parsed.Hostname()
	if hostname == "" || strings.IndexFunc(parsed.Host, func(r rune) bool { return r < 33 }) >= 0 {
		return "", errors.New("origin has an invalid host")
	}
	loopback := allowLoopbackHTTP && parsed.Scheme == "http" &&
		(hostname == "localhost" || net.ParseIP(hostname).IsLoopback())
	if parsed.Scheme != "https" && !loopback {
		return "", errors.New("origin must use HTTPS")
	}
	port := parsed.Port()
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", errors.New("origin has an invalid port")
		}
	}
	host := strings.ToLower(hostname)
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" && !(parsed.Scheme == "https" && port == "443") {
		host = net.JoinHostPort(hostname, port)
		if strings.Contains(hostname, ":") {
			host = "[" + hostname + "]:" + port
		}
	}
	return parsed.Scheme + "://" + host, nil
}

func TerminalQR(value string, maxColumns int) (string, error) {
	if value == "" {
		return "", errors.New("QR value is required")
	}
	code, err := qrcode.New(value, qrcode.Medium)
	if err != nil {
		return "", err
	}
	bitmap := code.Bitmap()
	if len(bitmap) == 0 {
		return "", errors.New("QR encoder returned an empty bitmap")
	}
	width := len(bitmap[0]) + 2
	if maxColumns > 0 && width > maxColumns {
		return "", fmt.Errorf("QR code needs %d columns, terminal has %d", width, maxColumns)
	}
	var output strings.Builder
	for row := 0; row < len(bitmap); row += 2 {
		output.WriteString("  ")
		for column := range bitmap[row] {
			top := bitmap[row][column]
			bottom := row+1 < len(bitmap) && bitmap[row+1][column]
			switch {
			case top && bottom:
				output.WriteRune('█')
			case top:
				output.WriteRune('▀')
			case bottom:
				output.WriteRune('▄')
			default:
				output.WriteRune(' ')
			}
		}
		if row+2 < len(bitmap) {
			output.WriteByte('\n')
		}
	}
	return output.String(), nil
}

// MaxQRBytes bounds what the phone may ask this computer to encode. A pairing
// link is a couple of hundred characters; anything larger is not a link.
const MaxQRBytes = 512

// PackedQR renders value as QR modules without a quiet zone, packed row-major
// into bits so the phone can draw them without its own encoder.
func PackedQR(value string) (int, []byte, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil, errors.New("QR value is required")
	}
	if len(value) > MaxQRBytes {
		return 0, nil, fmt.Errorf("QR value exceeds %d bytes", MaxQRBytes)
	}
	code, err := qrcode.New(value, qrcode.Medium)
	if err != nil {
		return 0, nil, err
	}
	code.DisableBorder = true
	bitmap := code.Bitmap()
	size := len(bitmap)
	if size == 0 {
		return 0, nil, errors.New("QR encoder returned an empty bitmap")
	}
	packed := make([]byte, (size*size+7)/8)
	for row := range bitmap {
		for column, dark := range bitmap[row] {
			if !dark {
				continue
			}
			index := row*size + column
			packed[index/8] |= 1 << (7 - index%8)
		}
	}
	return size, packed, nil
}
