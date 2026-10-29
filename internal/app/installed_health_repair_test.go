//go:build !herdr_tailscale_test

package app

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/deviceauth"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

type installedSyntheticTransport func(*http.Request) (*http.Response, error)

func (f installedSyntheticTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type installedSyntheticBody struct{ err error }

func (b installedSyntheticBody) Read([]byte) (int, error) { return 0, b.err }
func (installedSyntheticBody) Close() error               { return nil }

func TestInstalledLocalHealthEvidence(t *testing.T) {
	certificate := &tls.CertificateVerificationError{Err: errors.New("private certificate diagnostic")}
	for _, tc := range []struct {
		name        string
		err         error
		nonterminal bool
	}{
		{"cancelled", context.Canceled, true},
		{"deadline", context.DeadlineExceeded, true},
		{"availability", tailscalecli.ErrTransientUnavailable, true},
		{"connection", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, true},
		{"unknown", errors.New("private credential diagnostic"), false},
		{"conflict_cancelled", errors.Join(tailscalecli.ErrConflict, context.Canceled), false},
		{"uncertain_cancelled", errors.Join(tailscalecli.ErrUncertain, context.Canceled), false},
		{"malformed_cancelled", errors.Join(&json.SyntaxError{Offset: 1}, context.Canceled), false},
		{"certificate_cancelled", errors.Join(certificate, context.Canceled), false},
		{"unknown_availability", errors.Join(errors.New("private body"), tailscalecli.ErrTransientUnavailable), false},
	} {
		for _, phase := range []string{"request", "body"} {
			t.Run(tc.name+"/"+phase, func(t *testing.T) {
				s := &Server{cfg: &config.Config{Host: "127.0.0.1", Port: 18377, InstanceID: "synthetic"}, installedRun: "generation"}
				s.developmentCLIHealthClient = func(time.Duration) *http.Client {
					return &http.Client{Transport: installedSyntheticTransport(func(*http.Request) (*http.Response, error) {
						if phase == "request" {
							return nil, tc.err
						}
						return &http.Response{StatusCode: 200, Header: http.Header{"X-Herdr-Relay-Instance": []string{"synthetic"}}, Body: installedSyntheticBody{tc.err}}, nil
					})}
				}
				err := s.checkInstalledLocalHealth(context.Background())
				if err == nil || !errors.Is(err, tc.err) || installedFailureBranch(err, false, true) != tc.nonterminal {
					t.Fatalf("lost evidence/nonterminal=%v", tc.nonterminal)
				}
				if strings.Contains(fmt.Sprintf("%v %#v", err, err), "private") {
					t.Fatal("diagnostic disclosure")
				}
			})
		}
	}
}

func TestInstalledLocalHealthIdentityDecode(t *testing.T) {
	// Inactive zero owner marker selects data-only installed identity correlation.
	// It is NOT ownership, admission or positive concrete qualification.
	s := &Server{cfg: &config.Config{Transport: config.TransportTailscaleCLI, Host: "127.0.0.1", Port: 18377, InstanceID: "synthetic", TailscaleCLIOrigin: "https://synthetic.invalid"}, installedCLI: &tailscalecli.InstalledSession{}, installedRun: "generation", version: "v", revision: "r"}
	if s.controlRunID() != "generation" {
		t.Fatal("synthetic fixture did not resolve installed generation")
	}
	h := installedLocalHealth{Status: "ok", Readiness: "ready", Transport: config.TransportTailscaleCLI, Instance: "synthetic", Origin: s.cfg.TailscaleCLIOrigin, Run: "generation", Version: "v", Revision: "r"}
	for _, name := range []string{"matching", "unavailable", "wrong_generation", "malformed", "empty_generation"} {
		t.Run(name, func(t *testing.T) {
			value := h
			switch name {
			case "unavailable":
				value.Readiness = "waiting"
			case "wrong_generation":
				value.Run = "other"
			case "empty_generation":
				value.Run = ""
			}
			body, _ := json.Marshal(value)
			if name == "malformed" {
				body = []byte("{private invalid json")
			}
			s.developmentCLIHealthClient = func(time.Duration) *http.Client {
				return &http.Client{Transport: installedSyntheticTransport(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Header: http.Header{"X-Herdr-Relay-Instance": []string{"synthetic"}}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
				})}
			}
			err := s.checkInstalledLocalHealth(context.Background())
			if name == "matching" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || installedFailureBranch(err, false, true) != (name == "unavailable") {
				t.Fatal("incorrect identity classification")
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatal("body disclosure")
			}
		})
	}
}

type installedCancellingReader struct {
	reader io.Reader
	cancel context.CancelFunc
}

func (r installedCancellingReader) Read(p []byte) (int, error) {
	r.cancel()
	return r.reader.Read(p)
}

func TestInstalledHealthDecodeCancellationEvidence(t *testing.T) {
	for _, name := range []string{"malformed", "interrupted", "conflict", "certificate"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var reader io.Reader
			switch name {
			case "malformed":
				reader = strings.NewReader("{private malformed")
			case "interrupted":
				reader = installedSyntheticBody{context.Canceled}
			case "conflict":
				reader = installedSyntheticBody{tailscalecli.ErrConflict}
			case "certificate":
				reader = installedSyntheticBody{&tls.CertificateVerificationError{Err: errors.New("private certificate")}}
			}
			var value installedLocalHealth
			err := decodeInstalledHealth(ctx, installedCancellingReader{reader, cancel}, &value)
			if !errors.Is(err, context.Canceled) || installedFailureBranch(err, false, true) != (name == "interrupted") {
				t.Fatal("decode lost cancellation or terminal evidence")
			}
			if strings.Contains(fmt.Sprintf("%v %#v", err, err), "private") {
				t.Fatal("decode disclosed body")
			}
		})
	}
}

func TestInstalledInvitationObservationErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		refused bool
	}{
		{"missing", deviceauth.ErrBootstrapUnavailable, false},
		{"pending", deviceauth.ErrBootstrapUnavailable, false},
		{"consumed", deviceauth.ErrBootstrapUnavailable, false},
		{"expired", deviceauth.ErrInvitationExpired, false},
		{"burned", deviceauth.ErrInvitationBurned, false},
		{"pending_backoff", deviceauth.ErrRateLimited, false},
		{"uncertain", deviceauth.ErrManagedArmRecovery, true},
		{"cancelled", context.Canceled, true},
		{"unknown", errors.New("private snapshot diagnostic"), true},
		{"missing_uncertain", errors.Join(deviceauth.ErrBootstrapUnavailable, deviceauth.ErrManagedArmRecovery), true},
		{"missing_cancelled", errors.Join(deviceauth.ErrBootstrapUnavailable, context.Canceled), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			available, err := installedInvitationObservation(tc.err)
			if available || (err != nil) != tc.refused {
				t.Fatal("incorrect snapshot projection")
			}
			if err != nil && (!errors.Is(err, tc.err) || strings.Contains(err.Error(), "private")) {
				t.Fatal("lost typed/redacted evidence")
			}
		})
	}
}
