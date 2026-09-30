package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

// tailscale-cli retains read-only diagnostics and the local-only transport
// switch check. All real CLI Serve operations require the process-local
// dev-tailscale-cli workflow handle and cannot be reconstructed from flags.
func runTailscaleCLI(args []string, stdout, stderr io.Writer) (int, error) {
	return runTailscaleCLIWithInput(args, os.Stdin, stdout, stderr)
}

func runTailscaleCLIWithInput(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(args) > 0 && args[0] == "check-transport-switch" {
		return runTailscaleCLITransportSwitchCheck(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "activation-check" {
		flags := flag.NewFlagSet("tailscale-cli activation-check", flag.ContinueOnError)
		flags.SetOutput(stderr)
		scope := flags.String("scope", "production", "production or development activation scope")
		if err := flags.Parse(args[1:]); err != nil {
			return 2, err
		}
		if flags.NArg() != 0 {
			return 2, errors.New("activation-check accepts no positional arguments")
		}
		if *scope == "development" && config.TailscaleCLIDevelopmentQualificationEnabled() {
			_, err := fmt.Fprintln(stdout, "development-qualification-enabled; runtime qualification remains pending; operations require dev-tailscale-cli")
			return status(err)
		}
		return 2, errors.New("production activation remains disabled pending physical-phone qualification and separate production enablement; no CLI or service was contacted")
	}
	if len(args) == 0 {
		return 2, tailscaleCLIUsageError()
	}
	operation, operationArgs := args[0], args[1:]
	if operation == "resolve-binary" {
		flags := flag.NewFlagSet("tailscale-cli resolve-binary", flag.ContinueOnError)
		flags.SetOutput(stderr)
		binary := flags.String("binary", os.Getenv("HERDR_TAILSCALE_CLI_BIN"), "explicit absolute Tailscale CLI override")
		if err := flags.Parse(operationArgs); err != nil {
			return 2, err
		}
		if flags.NArg() != 0 {
			return 2, errors.New("resolve-binary accepts no positional arguments")
		}
		selected, err := tailscalecli.ResolveBinary(*binary, os.Getenv("PATH"), runtime.GOOS)
		if err != nil {
			return 1, err
		}
		_, err = fmt.Fprintln(stdout, selected)
		return status(err)
	}
	if operation == "preflight" {
		flags := flag.NewFlagSet("tailscale-cli preflight", flag.ContinueOnError)
		flags.SetOutput(stderr)
		binary := flags.String("binary", os.Getenv("HERDR_TAILSCALE_CLI_BIN"), "selected absolute Tailscale CLI path")
		scope := flags.String("scope", "", "development only")
		httpsPort := flags.Int("https-port", tailscalecli.DevelopmentHTTPSPort, "Tailscale HTTPS Serve listener port")
		if err := flags.Parse(operationArgs); err != nil {
			return 2, err
		}
		if flags.NArg() != 0 {
			return 2, errors.New("preflight accepts no positional arguments")
		}
		if *scope != "development" || *httpsPort != tailscalecli.DevelopmentHTTPSPort ||
			!config.TailscaleCLIDevelopmentQualificationEnabled() {
			return 2, errors.New("Tailscale CLI preflight is limited to the fixed development profile")
		}
		client, err := selectedCLIClient(*binary)
		if err != nil {
			return 1, err
		}
		report, err := client.Preflight(context.Background(), tailscalecli.DevelopmentHTTPSPort)
		if err != nil {
			return tailscalePreflightExitCode(err), err
		}
		if report.Profile != tailscalecli.ProfileAppStoreSupplied || !report.DevelopmentQualificationEnabled {
			return 78, tailscalecli.ErrUnsupported
		}
		if err := json.NewEncoder(stdout).Encode(report); err != nil {
			return 1, err
		}
		return 0, nil
	}
	return 2, tailscalecli.ErrWorkflowRequired
}

func runTailscaleCLITransportSwitchCheck(args []string, stdout, stderr io.Writer) (int, error) {
	flags := flag.NewFlagSet("tailscale-cli check-transport-switch", flag.ContinueOnError)
	flags.SetOutput(stderr)
	stateRoot := flags.String("state-root", os.Getenv("HERDR_TAILSCALE_CLI_STATE_ROOT"), "private registration root")
	coordinationRoot := flags.String("coordination-root", os.Getenv("HERDR_TAILSCALE_CLI_COORDINATION_ROOT"), "shared private node-lock root")
	installationID := flags.String("installation-id", os.Getenv("HERDR_RELAY_INSTANCE_ID"), "stable relay installation identifier")
	backendPort := flags.Int("backend-port", 0, "loopback relay backend port")
	if err := flags.Parse(args); err != nil {
		return 2, err
	}
	if flags.NArg() != 0 {
		return 2, errors.New("check-transport-switch accepts no positional arguments")
	}
	if err := tailscalecli.CheckTransportSwitch(*stateRoot, *coordinationRoot, *installationID, *backendPort); err != nil {
		return 1, err
	}
	return 0, nil
}

func tailscalePreflightExitCode(err error) int {
	if errors.Is(err, tailscalecli.ErrTransientUnavailable) {
		return 75
	}
	return 78
}

func selectedCLIClient(binary string) (*tailscalecli.Client, error) {
	selected, err := tailscalecli.ResolveBinary(binary, os.Getenv("PATH"), runtime.GOOS)
	if err != nil {
		return nil, err
	}
	return tailscalecli.NewClient(selected)
}

func readRouteConfirmation(stdin io.Reader, expected string) (string, error) {
	if stdin == nil {
		return "", errors.New("route-bound confirmation must be supplied on stdin")
	}
	reader := bufio.NewReaderSize(stdin, 512)
	line, err := reader.ReadSlice('\n')
	if err != nil || len(line) > 512 {
		return "", errors.New("route-bound confirmation line is missing or oversized")
	}
	typed := strings.TrimSuffix(string(line), "\n")
	if typed != expected {
		return "", errors.New("route-bound confirmation did not exactly match the selected node, listener and backend")
	}
	return typed, nil
}

func tailscaleCLIUsageError() error {
	return errors.New("usage: herdr-mobile-relay tailscale-cli {activation-check|resolve-binary|preflight --scope development|check-transport-switch}; real operations require dev-tailscale-cli")
}
