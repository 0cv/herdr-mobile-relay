package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

func TestInstalledSetupMonitorTransition(t *testing.T) {
	published := make(chan struct{})
	ticks := make(chan time.Time, 2)
	ticks <- time.Unix(1, 0)
	ticks <- time.Unix(1000, 0)
	close(ticks)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls, quarantines := 0, 0
	check := func(context.Context) error { calls++; return tailscalecli.ErrUncertain }
	quarantine := func() { quarantines++ }
	installedRecoveryAfterPublication(ctx, published, ticks, check, quarantine)
	if calls != 0 || quarantines != 1 {
		t.Fatal("waiting polled or failed to close")
	}
	close(published)
	installedRecoveryAfterPublication(context.Background(), published, ticks, check, quarantine)
	if calls != 1 {
		t.Fatal("publication failed to transition to terminal read-only monitor")
	}
}

func TestInstalledRecoveryJoinedPrecedence(t *testing.T) {
	// All errors and times are synthetic: no resolver, transport or socket.
	wrappers := []func(error) error{
		func(e error) error { return e },
		func(e error) error { return fmt.Errorf("wrapped: %w", e) },
		func(e error) error { return &url.Error{Op: "Get", URL: "redacted", Err: e} },
		func(e error) error { return &net.OpError{Op: "read", Net: "tcp", Err: e} },
		func(e error) error {
			return fmt.Errorf("outer: %w", &url.Error{Err: &net.OpError{Err: fmt.Errorf("inner: %w", e)}})
		},
	}
	assertLoop := func(err error, retry bool) {
		t.Helper()
		if installedAvailabilityFailure(err) != retry {
			t.Fatal("incorrect error-tree classification")
		}
		ticks := make(chan time.Time, 6)
		for _, seconds := range []int64{1, 60, 61, 180, 181, 211} {
			ticks <- time.Unix(seconds, 0)
		}
		close(ticks)
		calls, quarantines := 0, 0
		installedRecovery(context.Background(), ticks, func(context.Context) error {
			calls++
			if calls <= 2 {
				return err
			}
			return nil
		}, func() { quarantines++ })
		// Two availability failures back off 60/120 seconds, then successful
		// recovery restores the 30-second interval. Terminal trees stop at one.
		wantCalls, wantQuarantines := 1, 2
		if retry {
			wantCalls, wantQuarantines = 4, 3
		}
		if calls != wantCalls || quarantines != wantQuarantines {
			t.Fatal("recovery/backoff or terminal latch violated")
		}
	}
	dns := &net.DNSError{IsTimeout: true}
	unknown := errors.New("unknown evidence")
	cert := &tls.CertificateVerificationError{Err: context.DeadlineExceeded}
	for _, wrap := range wrappers {
		for _, positive := range []error{
			dns, context.DeadlineExceeded,
			errors.Join(dns, tailscalecli.ErrTransientUnavailable),
			&url.Error{Err: io.EOF},
			&net.OpError{Err: io.ErrUnexpectedEOF},
			&net.OpError{Err: syscall.ECONNRESET},
			&url.Error{Err: errors.Join(io.EOF, dns)},
		} {
			assertLoop(wrap(positive), true)
		}
		for _, terminal := range []error{
			unknown, tailscalecli.ErrConflict, tailscalecli.ErrUncertain,
			tailscalecli.ErrUnsupported, context.Canceled,
			cert, x509.UnknownAuthorityError{},
		} {
			assertLoop(wrap(terminal), false)
			for _, join := range []error{
				errors.Join(dns, terminal), errors.Join(terminal, dns),
				errors.Join(&url.Error{Err: io.EOF}, terminal),
				errors.Join(dns, fmt.Errorf("nested: %w", errors.Join(context.DeadlineExceeded, terminal))),
			} {
				assertLoop(wrap(join), false)
			}
		}
	}
	connectionError := func(err error) error {
		return &url.Error{Op: "Get", URL: "https://synthetic.invalid", Err: &net.OpError{
			Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: err},
		}}
	}
	for _, errno := range []syscall.Errno{
		syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.ENETDOWN,
		syscall.EHOSTDOWN, syscall.ECONNABORTED,
	} {
		assertLoop(connectionError(errno), true)
		for _, terminal := range []error{
			unknown, tailscalecli.ErrConflict, tailscalecli.ErrUncertain,
			tailscalecli.ErrUnsupported, context.Canceled,
			cert, x509.UnknownAuthorityError{}, syscall.EACCES, syscall.EPERM,
		} {
			assertLoop(errors.Join(connectionError(errno), terminal), false)
			assertLoop(errors.Join(terminal, connectionError(errno)), false)
			assertLoop(connectionError(errors.Join(errno, terminal)), false)
			assertLoop(connectionError(errors.Join(terminal, errno)), false)
		}
	}
	assertLoop(connectionError(syscall.EACCES), false)
	assertLoop(connectionError(syscall.EPERM), false)
	assertLoop(io.EOF, false)
	assertLoop(fmt.Errorf("raw: %w", io.ErrUnexpectedEOF), false)
	assertLoop(&net.DNSError{}, false)

	for _, terminal := range []error{tailscalecli.ErrConflict, tailscalecli.ErrUncertain, tailscalecli.ErrUnsupported, errors.New("unknown evidence")} {
		for _, err := range []error{errors.Join(context.DeadlineExceeded, terminal), errors.Join(terminal, tailscalecli.ErrTransientUnavailable), fmt.Errorf("wrapped: %w", errors.Join(terminal, context.DeadlineExceeded))} {
			if installedAvailabilityFailure(err) {
				t.Fatal("terminal/unknown evidence became retryable")
			}
			ticks := make(chan time.Time, 2)
			ticks <- time.Unix(1, 0)
			ticks <- time.Unix(1000, 0)
			close(ticks)
			calls := 0
			installedRecovery(context.Background(), ticks, func(context.Context) error { calls++; return err }, func() {})
			if calls != 1 {
				t.Fatal("terminal evidence did not latch")
			}
		}
	}
	if !installedAvailabilityFailure(errors.Join(context.DeadlineExceeded, tailscalecli.ErrTransientUnavailable)) {
		t.Fatal("pure availability join rejected")
	}
}
