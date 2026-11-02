package tailscaleserve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const usage = "usage: herdr-mobile-relay tailscale {check|address|serve|off} [--port N] [--relay-port N] [--bg]"

const (
	Usage      = 2
	Missing    = 3
	Old        = 4
	Stopped    = 5
	NoDNS      = 6
	NoHTTPS    = 7
	Permission = 8
	Conflict   = 9
)

var versionNumber = regexp.MustCompile(`^(\d+)\.(\d+)(?:\.|\s|$)`)
var systemBundle = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
var outputTimeout = 30 * time.Second
var foregroundGrace = 2 * time.Second

type failure struct {
	code int
	err  error
}

func fail(code int, format string, args ...any) error {
	return failure{code, fmt.Errorf(format, args...)}
}

func (f failure) Error() string { return f.err.Error() }

type client struct {
	binary string
	env    []string
}

func resolve() (client, error) {
	if path := os.Getenv("HERDR_TAILSCALE_BIN"); path != "" {
		if !filepath.IsAbs(path) || !executable(path) {
			return client{}, fail(Missing, "HERDR_TAILSCALE_BIN must be an absolute executable: %q", path)
		}
		return client{path, append(os.Environ(), "TAILSCALE_BE_CLI=1")}, nil
	}
	if path, err := exec.LookPath("tailscale"); err == nil {
		return client{path, append(os.Environ(), "TAILSCALE_BE_CLI=1")}, nil
	}
	var paths []string
	if home := os.Getenv("HOME"); home != "" {
		paths = append(paths, filepath.Join(home, "Applications/Tailscale.app/Contents/MacOS/Tailscale"))
	}
	paths = append(paths, systemBundle)
	for _, path := range paths {
		if executable(path) {
			return client{path, append(os.Environ(), "TAILSCALE_BE_CLI=1")}, nil
		}
	}
	return client{}, fail(Missing, "Tailscale CLI not installed; install Tailscale or set HERDR_TAILSCALE_BIN")
}

func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0
}

func (c client) command(args ...string) *exec.Cmd {
	cmd := exec.Command(c.binary, args...)
	cmd.Env = c.env
	return cmd
}

func (c client) output(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), outputTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.binary, args...)
	cmd.Env = c.env
	cmd.WaitDelay = time.Second
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("tailscale %s timed out: %w", strings.Join(args, " "), ctx.Err())
	}
	if err != nil {
		return nil, cliError(strings.Join(args, " "), output, err)
	}
	return output, nil
}

func cliError(action string, output []byte, err error) error {
	message := strings.TrimSpace(string(output))
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "access denied") || strings.Contains(lower, "permission denied"):
		return fail(Permission, "Tailscale denied permission for %s; on Linux, run sudo tailscale set --operator=$USER once: %s", action, message)
	case strings.Contains(lower, "error enabling https feature"), strings.Contains(lower, "https cert support is not enabled/configured"):
		return fail(NoHTTPS, "enable HTTPS certificates in your Tailscale tailnet DNS settings: %s", message)
	}
	return fmt.Errorf("tailscale %s failed: %s: %w", action, message, err)
}

type status struct {
	BackendState string `json:"BackendState"`
	Self         struct {
		DNSName string `json:"DNSName"`
	} `json:"Self"`
	CertDomains []string `json:"CertDomains"`
}

func (c client) check(requireHTTPS bool) (string, error) {
	output, err := c.output("version")
	if err != nil {
		return "", err
	}
	matches := versionNumber.FindStringSubmatch(strings.TrimSpace(string(output)))
	if matches == nil {
		return "", fmt.Errorf("cannot read Tailscale version: %q", strings.TrimSpace(string(output)))
	}
	major, _ := strconv.Atoi(matches[1])
	minor, _ := strconv.Atoi(matches[2])
	if major < 1 || major == 1 && minor < 52 {
		return "", fail(Old, "Tailscale %s is too old; install Tailscale 1.52 or newer", strings.TrimSuffix(matches[0], "."))
	}
	output, err = c.output("status", "--json")
	if err != nil {
		var failure failure
		if errors.As(err, &failure) && failure.code == Permission {
			return "", err
		}
		return "", fail(Stopped, "Tailscale is not logged in or running: %v", err)
	}
	var st status
	if err := json.Unmarshal(output, &st); err != nil {
		return "", fmt.Errorf("invalid tailscale status --json: %w", err)
	}
	if st.BackendState != "Running" {
		return "", fail(Stopped, "Tailscale is not logged in or running (BackendState: %q)", st.BackendState)
	}
	name := strings.TrimSuffix(st.Self.DNSName, ".")
	if !strings.HasSuffix(name, ".ts.net") || strings.TrimSuffix(name, ".ts.net") == "" {
		return "", fail(NoDNS, "Tailscale has no *.ts.net name; enable MagicDNS in your tailnet")
	}
	if requireHTTPS && len(st.CertDomains) == 0 {
		return "", fail(NoHTTPS, "enable HTTPS certificates in your Tailscale tailnet DNS settings")
	}
	return name, nil
}

type serveConfig struct {
	TCP map[string]struct{ HTTPS bool } `json:"TCP"`
	Web map[string]struct {
		Handlers map[string]struct{ Proxy string }
	} `json:"Web"`
	AllowFunnel map[string]bool        `json:"AllowFunnel"`
	Foreground  map[string]serveConfig `json:"Foreground"`
}

func (c client) mapping(port int, target string) (bool, bool, error) {
	output, err := c.output("serve", "status", "--json")
	if err != nil {
		return false, false, err
	}
	var config serveConfig
	if err := json.Unmarshal(output, &config); err != nil {
		return false, false, fmt.Errorf("invalid tailscale serve status --json: %w", err)
	}
	used, owned := config.portMapping(port, target)
	return used, owned, nil
}

func (config serveConfig) portMapping(port int, target string) (bool, bool) {
	key := strconv.Itoa(port)
	tcp, hasTCP := config.TCP[key]
	used := hasTCP
	owned := hasTCP && tcp.HTTPS
	webCount := 0
	for host, web := range config.Web {
		if !strings.HasSuffix(host, ":"+key) {
			continue
		}
		used = true
		webCount++
		root, ok := web.Handlers["/"]
		owned = owned && ok && len(web.Handlers) == 1 && root.Proxy == target
	}
	for host, enabled := range config.AllowFunnel {
		if enabled && strings.HasSuffix(host, ":"+key) {
			used, owned = true, false
		}
	}
	for _, foreground := range config.Foreground {
		if active, _ := foreground.portMapping(port, target); active {
			return true, false
		}
	}
	return used, owned && webCount == 1
}

func (c client) foreground(args []string, stderr io.Writer) error {
	cmd := c.command(args...)
	cmd.WaitDelay = time.Second
	var output bytes.Buffer
	diagnostics := io.MultiWriter(stderr, &output)
	cmd.Stdout = diagnostics
	cmd.Stderr = diagnostics
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(interrupt)
	if err := cmd.Start(); err != nil {
		return cliError(strings.Join(args, " "), nil, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return cliError(strings.Join(args, " "), output.Bytes(), err)
		}
		return nil
	case sig := <-interrupt:
		_ = cmd.Process.Signal(sig)
		select {
		case <-done:
		case <-time.After(foregroundGrace):
			_ = cmd.Process.Kill()
			<-done
		}
		return nil
	}
}

func Run(args []string, stdout, stderr io.Writer) (int, error) {
	if len(args) == 0 {
		return Usage, errors.New(usage)
	}
	op, args := args[0], args[1:]
	if op != "check" && op != "address" && op != "serve" && op != "off" {
		return Usage, errors.New(usage)
	}
	flags := flag.NewFlagSet("tailscale "+op, flag.ContinueOnError)
	flags.SetOutput(stderr)
	port := flags.Int("port", 8443, "Tailscale HTTPS port")
	relayPort := flags.Int("relay-port", 8375, "local relay port")
	bg := flags.Bool("bg", false, "keep Serve running after this command exits")
	if err := flags.Parse(args); err != nil {
		return Usage, err
	}
	if flags.NArg() != 0 || *port < 1 || *port > 65535 || *relayPort < 1 || *relayPort > 65535 || *bg && op != "serve" {
		return Usage, errors.New(usage)
	}
	c, err := resolve()
	if err != nil {
		return result(err)
	}
	name, err := c.check(op != "off")
	if err != nil {
		return result(err)
	}
	if op == "check" {
		return 0, nil
	}
	if op == "address" {
		address := "https://" + name
		if *port != 443 {
			address = "https://" + net.JoinHostPort(name, strconv.Itoa(*port))
		}
		_, err = fmt.Fprintln(stdout, address)
		return result(err)
	}
	target := fmt.Sprintf("http://127.0.0.1:%d", *relayPort)
	used, owned, err := c.mapping(*port, target)
	if err != nil {
		return result(err)
	}
	if op == "off" {
		if !used {
			return 0, nil
		}
		if !owned {
			return result(fail(Conflict, "Tailscale HTTPS port %d is in use by another mapping; refusing to remove it", *port))
		}
		_, err = c.output("serve", "--https="+strconv.Itoa(*port), "off")
		return result(err)
	}
	if used {
		if owned && *bg {
			return 0, nil
		}
		return result(fail(Conflict, "Tailscale HTTPS port %d is already in use; refusing to replace its mapping", *port))
	}
	serveArgs := []string{"serve", "--yes", "--https=" + strconv.Itoa(*port), target}
	if *bg {
		serveArgs = append([]string{"serve", "--bg"}, serveArgs[1:]...)
		_, err = c.output(serveArgs...)
	} else {
		err = c.foreground(serveArgs, stderr)
	}
	if err != nil {
		if _, owned, statusErr := c.mapping(*port, target); statusErr == nil && owned {
			if _, offErr := c.output("serve", "--https="+strconv.Itoa(*port), "off"); offErr != nil {
				err = fmt.Errorf("%w; cleanup failed, mapping may remain: %v", err, offErr)
			}
		} else if statusErr != nil {
			err = fmt.Errorf("%w; could not check published mapping; mapping may remain: %v", err, statusErr)
		}
	}
	return result(err)
}

func result(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var failure failure
	if errors.As(err, &failure) {
		return failure.code, err
	}
	return 1, err
}
