package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

// tailscale-cli retains filesystem-only binary selection and the local-only
// transport-switch check. All real CLI Serve operations use the process-local
// dev-tailscale-cli workflow after Go-owned profile/isolation validation; this
// does not claim same-user anti-fabrication or shell-launcher provenance.
func runTailscaleCLI(args []string, stdout, stderr io.Writer) (int, error) {
	return runTailscaleCLIWithInput(args, os.Stdin, stdout, stderr)
}

func runTailscaleCLIWithInput(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(args) > 0 && args[0] == "with-transport-switch-lock" {
		return runTailscaleCLIWithTransportSwitchLock(args[1:], stdin, stdout, stderr)
	}
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
		if *scope == "development" {
			_, err := fmt.Fprintln(stdout, "development CLI access is limited to the profile-checked isolated dev-tailscale-cli workflow; runtime qualification remains pending")
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
	return 2, tailscalecli.ErrWorkflowRequired
}

func runTailscaleCLIWithTransportSwitchLock(args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	flags := flag.NewFlagSet("tailscale-cli with-transport-switch-lock", flag.ContinueOnError)
	flags.SetOutput(stderr)
	stateRoot := flags.String("state-root", os.Getenv("HERDR_TAILSCALE_CLI_STATE_ROOT"), "private registration root")
	coordinationRoot := flags.String("coordination-root", os.Getenv("HERDR_TAILSCALE_CLI_COORDINATION_ROOT"), "shared private node-lock root")
	installationID := flags.String("installation-id", os.Getenv("HERDR_RELAY_INSTANCE_ID"), "stable relay installation identifier")
	backendPort := flags.Int("backend-port", 0, "loopback relay backend port")
	if err := flags.Parse(args); err != nil {
		return 2, err
	}
	command := flags.Args()
	if len(command) == 0 {
		return 2, errors.New("with-transport-switch-lock requires a local commit command after --")
	}
	err := tailscalecli.WithTransportSwitchLock(*stateRoot, *coordinationRoot, *installationID, *backendPort, func() error {
		child := exec.Command(command[0], command[1:]...)
		child.Stdin = stdin
		child.Stdout = stdout
		child.Stderr = stderr
		return child.Run()
	})
	if err != nil {
		return 1, err
	}
	return 0, nil
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
	return errors.New("usage: herdr-mobile-relay tailscale-cli {activation-check|resolve-binary|check-transport-switch|with-transport-switch-lock}; real CLI operations require dev-tailscale-cli")
}
