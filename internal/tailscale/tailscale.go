// Package tailscale implements a bounded, read-only, source-versioned CLI adapter.
package tailscale

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultHTTPSPort = 443
	MaxOutputBytes   = 1 << 20
	CommandTimeout   = 5 * time.Second
)

// waitDelay bounds how long run waits for inherited stdout/stderr pipes to
// close after the direct child exits or the inspection deadline passes.
var waitDelay = 2 * time.Second

var (
	ErrNotJSON       = errors.New("tailscale returned invalid JSON")
	ErrOutputTooLong = errors.New("tailscale output exceeded the safety limit")
)

type CommandError struct {
	Binary string
	Args   []string
	Err    error
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("tailscale command %s failed: %v", e.Binary, e.Err)
}
func (e *CommandError) Unwrap() error { return e.Err }

type Status struct {
	BackendState    string
	Version         string
	NodeID          string
	DNSName         string
	UserID          int64
	TailnetName     string
	MagicDNSSuffix  string
	MagicDNSEnabled bool
	CertDomains     []string
	Account         map[string]json.RawMessage
	SelfPresent     bool
	LoggedIn        bool
}
type ServeStatus struct {
	Complete         bool
	Configured       bool
	FunnelConfigured bool
	RetainedFunnel   bool
	Routes           []string
	ObservedRoutes   []Route
}
type Inspection struct {
	VersionMetadata      json.RawMessage `json:"version_metadata,omitempty"`
	AccountLoginName     string          `json:"account_login_name,omitempty"`
	AccountDisplayName   string          `json:"account_display_name,omitempty"`
	AccountProfilePicURL string          `json:"account_profile_pic_url,omitempty"`
	BackendState         string          `json:"backend_state"`
	LoggedIn             bool            `json:"logged_in"`
	NodeID               string          `json:"node_id,omitempty"`
	DNSName              string          `json:"dns_name,omitempty"`
	Origin               string          `json:"origin,omitempty"`
	ServeConfigured      bool            `json:"serve_configured"`
	FunnelConfigured     bool            `json:"funnel_configured"`
	ServeRouteCount      int             `json:"serve_route_count"`
	ServeRouteOwned      bool            `json:"serve_route_owned"`
	ServeRoutes          []string        `json:"serve_routes,omitempty"`
	ServeInspected       bool            `json:"serve_inspected"`
	ExposureComplete     bool            `json:"exposure_complete"`
	ObservedRoutes       []Route         `json:"observed_routes,omitempty"`
	StatusVersion        string          `json:"status_version,omitempty"`
	UserID               int64           `json:"user_id,omitempty"`
	TailnetName          string          `json:"tailnet_name,omitempty"`
	MagicDNSSuffix       string          `json:"magic_dns_suffix,omitempty"`
	MagicDNSEnabled      bool            `json:"magic_dns_enabled"`
	CertDomains          []string        `json:"cert_domains,omitempty"`
}

func ParseStatus(data []byte) (Status, error)           { return parseIdentity(data) }
func ParseServeStatus(data []byte) (ServeStatus, error) { return parseServe(data) }

// Inspect does not start Serve, change authentication, or infer ownership.
func Inspect(ctx context.Context, binary string, httpsPort int) (Inspection, error) {
	return inspectWithRunner(ctx, binary, httpsPort, run)
}
func inspectWithRunner(ctx context.Context, binary string, httpsPort int, runner func(context.Context, string, ...string) ([]byte, error)) (Inspection, error) {
	if strings.TrimSpace(binary) == "" {
		binary = "tailscale"
	}
	data, err := runner(ctx, binary, "status", "--json")
	if err != nil {
		return Inspection{}, err
	}
	status, err := ParseStatus(data)
	if err != nil {
		return Inspection{}, err
	}
	inspection := Inspection{BackendState: status.BackendState, LoggedIn: status.LoggedIn, NodeID: status.NodeID, DNSName: status.DNSName, StatusVersion: status.Version, UserID: status.UserID, TailnetName: status.TailnetName, MagicDNSSuffix: status.MagicDNSSuffix, MagicDNSEnabled: status.MagicDNSEnabled, CertDomains: status.CertDomains}
	// These optional account strings were type-checked by ParseStatus.
	inspection.AccountLoginName, _ = stringField(status.Account, "LoginName")
	inspection.AccountDisplayName, _ = stringField(status.Account, "DisplayName")
	inspection.AccountProfilePicURL, _ = stringField(status.Account, "ProfilePicURL")
	if !status.LoggedIn {
		return inspection, fmt.Errorf("authenticated identity unavailable")
	}
	inspection.Origin, err = Origin(status.DNSName, httpsPort)
	if err != nil {
		return inspection, err
	}
	data, err = runner(ctx, binary, "version", "--json", "--daemon")
	if err != nil {
		return inspection, err
	}
	if err = validateVersion(data, status.Version); err != nil {
		return inspection, err
	}
	inspection.VersionMetadata = append(json.RawMessage(nil), data...)
	data, err = runner(ctx, binary, "serve", "status", "--json")
	if err != nil {
		return inspection, err
	}
	serve, err := ParseServeStatus(data)
	if err != nil {
		return inspection, err
	}
	inspection.ServeConfigured = serve.Configured
	inspection.FunnelConfigured = serve.FunnelConfigured
	inspection.ServeRouteCount = len(serve.ObservedRoutes)
	inspection.ServeRoutes = serve.Routes
	inspection.ObservedRoutes = serve.ObservedRoutes
	inspection.ServeInspected = true
	inspection.ExposureComplete = serve.Complete
	// No independently owned child session/backend is supplied to this API.
	inspection.ServeRouteOwned = false
	return inspection, nil
}

func Origin(dnsName string, port int) (string, error) {
	host := strings.TrimSuffix(strings.TrimSpace(dnsName), ".")
	if !validHostname(host) {
		return "", fmt.Errorf("Tailscale node has no usable DNS name")
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid HTTPS port %d", port)
	}
	if port == DefaultHTTPSPort {
		return "https://" + strings.ToLower(host), nil
	}
	return "https://" + net.JoinHostPort(strings.ToLower(host), strconv.Itoa(port)), nil
}

func run(parent context.Context, binary string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, CommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	// A descendant that inherits stdout/stderr must not extend the inspection
	// deadline: WaitDelay bounds the pipe wait after the direct child exits and
	// kills it if it is still running.
	command.WaitDelay = waitDelay
	var output limitedBuffer
	var stderr limitedBuffer
	command.Stdout = &output
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if output.exceeded || stderr.exceeded {
			return nil, ErrOutputTooLong
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("tailscale command timed out")
		}
		// WaitDelay closes inherited pipes once the bound elapses. When the
		// direct child itself succeeded, its captured output is complete and the
		// bounded pipe close is not a command failure.
		if errors.Is(err, exec.ErrWaitDelay) && command.ProcessState != nil && command.ProcessState.Success() {
			return output.Bytes(), nil
		}
		return nil, &CommandError{Binary: binary, Args: append([]string(nil), args...), Err: commandError(err)}
	}
	if output.exceeded || stderr.exceeded {
		return nil, ErrOutputTooLong
	}
	return output.Bytes(), nil
}
func commandError(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Errorf("exit status %d", exitErr.ExitCode())
	}
	return err
}

type limitedBuffer struct {
	bytes.Buffer
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > MaxOutputBytes {
		b.exceeded = true
		return 0, ErrOutputTooLong
	}
	return b.Buffer.Write(p)
}
func validHostname(host string) bool {
	if host == "" || len(host) > 253 || strings.ContainsAny(host, " /\\\t\r\n") || net.ParseIP(host) != nil {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '-' {
				return false
			}
		}
	}
	return true
}
