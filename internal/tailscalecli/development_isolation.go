package tailscalecli

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/setuphelper"
)

const developmentEnvironmentMaxBytes = 64 * 1024

type developmentIsolation struct {
	home, root, state, coordination     string
	relayEnv, configHome, cacheHome     string
	dataHome, releaseRoot, runtimeRoot  string
	webRoot, relayBinary, pairingSocket string
	herdrBinary, herdrSocket            string
}

// ValidateDevelopmentOperationEnvironment rechecks the complete Go-owned
// development layout before action-specific operations. It does not establish
// launcher provenance or defend against a deliberately fabricating same-UID
// process; it prevents ordinary entrypoint/root/configuration bypasses.
func ValidateDevelopmentOperationEnvironment(root, stateRoot, coordinationRoot string, requireStopped, requireHerdrSocket bool) error {
	layout, err := developmentIsolationFromEnvironment(root, stateRoot, coordinationRoot)
	if err != nil {
		return err
	}
	return layout.validate(requireStopped, requireHerdrSocket)
}

func developmentIsolationFromEnvironment(root, stateRoot, coordinationRoot string) (*developmentIsolation, error) {
	home := os.Getenv("HOME")
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, ErrPermissionDenied
		}
	}
	layout := &developmentIsolation{
		home:          home,
		root:          root,
		state:         stateRoot,
		coordination:  coordinationRoot,
		relayEnv:      filepath.Join(root, "relay.env"),
		configHome:    filepath.Join(root, "config"),
		cacheHome:     filepath.Join(root, "cache"),
		dataHome:      filepath.Join(root, "data"),
		releaseRoot:   filepath.Join(root, "data", "herdr-mobile-relay"),
		runtimeRoot:   filepath.Join(root, "runtime"),
		webRoot:       filepath.Join(root, "current", "web"),
		relayBinary:   filepath.Join(root, "current", "bin", "herdr-mobile-relay"),
		pairingSocket: filepath.Join(root, "config", "pairing-control.sock"),
		herdrBinary:   os.Getenv("HERDR_BIN"),
		herdrSocket:   os.Getenv("HERDR_SOCKET_PATH"),
	}
	if err := layout.validate(false, false); err != nil {
		return nil, err
	}
	return layout, nil
}

func (d *developmentIsolation) validate(requireStopped, requireHerdrSocket bool) error {
	if d == nil || runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return ErrUnsupported
	}
	if os.Getenv("HERDR_DEV_TAILSCALE_CLI_ENABLE") != "1" ||
		os.Getenv("HERDR_TAILSCALE_CLI_SCOPE") != "development" ||
		os.Getenv("HERDR_RELAY_TRANSPORT") != "tailscale-cli" {
		return fmt.Errorf("%w: explicit development opt-in and development transport are required", ErrWorkflowRequired)
	}
	for _, name := range []string{
		"HERDR_DEV_TAILSCALE_CLI_PORT", "HERDR_DEV_TAILSCALE_CLI_PLUGIN_PORT", "HERDR_DEV_TAILSCALE_CLI_HTTPS_PORT",
	} {
		if os.Getenv(name) != "" {
			return fmt.Errorf("%w: %s is not an allowed development override", ErrWorkflowRequired, name)
		}
	}
	for _, name := range []string{
		"HERDR_PLUGIN_CONFIG_DIR", "HERDR_TAILSCALE_ORIGIN", "HERDR_EXTERNAL_HTTPS_ORIGIN",
		"HERDR_GATEWAY_URL", "HERDR_GATEWAY_SELECTION", "HERDR_RELAY_RUN_ID",
	} {
		if os.Getenv(name) != "" {
			return fmt.Errorf("%w: conflicting production or transport setting %s", ErrWorkflowRequired, name)
		}
	}
	if !sameAbsolutePath(d.home, os.Getenv("HOME")) || !filepath.IsAbs(d.home) ||
		!sameAbsolutePath(d.root, os.Getenv("HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT")) ||
		!sameAbsolutePath(d.state, os.Getenv("HERDR_TAILSCALE_CLI_STATE_ROOT")) ||
		!sameAbsolutePath(d.coordination, os.Getenv("HERDR_TAILSCALE_CLI_COORDINATION_ROOT")) {
		return fmt.Errorf("%w: development roots must match the Go-validated process environment", ErrPermissionDenied)
	}
	if filepath.Clean(d.root) != d.root || filepath.Clean(d.state) != filepath.Join(d.root, "registration") ||
		filepath.Clean(d.coordination) != d.coordination || d.root == "/" || d.root == d.home ||
		!filepath.IsAbs(d.root) || !filepath.IsAbs(d.coordination) {
		return ErrPermissionDenied
	}
	if os.Getenv("HERDR_RELAY_ENV") != d.relayEnv ||
		os.Getenv("XDG_CONFIG_HOME") != d.configHome ||
		os.Getenv("XDG_CACHE_HOME") != d.cacheHome ||
		os.Getenv("XDG_DATA_HOME") != d.dataHome ||
		os.Getenv("HERDR_RELEASE_ROOT") != d.releaseRoot ||
		os.Getenv("HERDR_WEB_ROOT") != d.webRoot ||
		os.Getenv("HERDR_RELAY_BIN") != d.relayBinary ||
		os.Getenv("HERDR_RELAY_PAIRING_SOCKET") != d.pairingSocket {
		return fmt.Errorf("%w: relay environment, XDG roots, release, web bundle, or control socket is outside the isolated layout", ErrPermissionDenied)
	}
	if os.Getenv("HERDR_RELAY_HOST") != "127.0.0.1" ||
		os.Getenv("HERDR_RELAY_PORT") != strconv.Itoa(DevelopmentBackendPort) ||
		os.Getenv("HERDR_RELAY_PLUGIN_PORT") != strconv.Itoa(DevelopmentPluginPort) ||
		os.Getenv("HERDR_TAILSCALE_CLI_HTTPS_PORT") != strconv.Itoa(DevelopmentHTTPSPort) ||
		os.Getenv("HERDR_REACHABILITY_PORT_MAPPING") != "0" ||
		os.Getenv("HERDR_RELAY_REARM_BOOTSTRAP") != "0" {
		return fmt.Errorf("%w: development ports or side-effect settings differ from the fixed profile", ErrWorkflowRequired)
	}

	for _, path := range []string{d.root, d.state, d.coordination, d.configHome, d.cacheHome, d.dataHome} {
		resolved, err := validatePrivateDirectory(path)
		if err != nil || resolved != path {
			return fmt.Errorf("%w: isolated development directory is not private", ErrPermissionDenied)
		}
	}
	for _, optional := range []string{d.releaseRoot, d.runtimeRoot} {
		if err := validateOptionalPrivateDirectory(optional); err != nil {
			return err
		}
	}
	if pathsOverlap(d.root, d.coordination) || pathsOverlap(d.state, d.coordination) {
		return ErrPermissionDenied
	}
	if err := d.validateCurrentRelease(); err != nil {
		return err
	}
	if err := requirePrivateFile(d.relayEnv, 0o600); err != nil {
		return err
	}
	if err := d.validateMarker(); err != nil {
		return err
	}
	values, err := readShellEnvironment(d.relayEnv)
	if err != nil {
		return err
	}
	if err := d.validateRelayEnvironment(values); err != nil {
		return err
	}
	if err := d.validateProductionSeparation(); err != nil {
		return err
	}
	if err := validateOptionalSocket(d.pairingSocket); err != nil {
		return err
	}
	if requireStopped {
		if _, err := os.Lstat(d.pairingSocket); err == nil {
			return errors.New("stop the foreground development relay before this action; state and route were retained")
		} else if !errors.Is(err, os.ErrNotExist) {
			return ErrPermissionDenied
		}
	}
	if !filepath.IsAbs(d.herdrSocket) || pathsOverlap(d.root, d.herdrSocket) {
		return ErrPermissionDenied
	}
	if err := validateOptionalSocket(d.herdrSocket); err != nil {
		return err
	}
	if requireHerdrSocket {
		if err := requireOwnedSocket(d.herdrSocket); err != nil {
			return errors.New("the configured local Herdr Unix socket is unavailable or unsafe")
		}
		if err := requireExecutable(d.herdrBinary); err != nil {
			return errors.New("the configured Herdr executable is unavailable or unsafe")
		}
	}
	return nil
}

func (d *developmentIsolation) validateRelayEnvironment(values map[string]string) error {
	origin := values["HERDR_TAILSCALE_CLI_ORIGIN"]
	originURL, err := url.Parse(origin)
	if err != nil || !validCanonicalOrigin(origin) || originURL.Scheme != "https" || originURL.Port() != strconv.Itoa(DevelopmentHTTPSPort) {
		return fmt.Errorf("%w: the configured origin is not the exact development HTTPS endpoint", ErrPermissionDenied)
	}
	if len(values["HERDR_RELAY_TOKEN"]) != 32 || !validLabel(values["HERDR_RELAY_INSTANCE_ID"]) ||
		!validLabel(values["HERDR_RELAY_CONTROL_RUN_ID"]) || !validNodeID(values["HERDR_TAILSCALE_CLI_NODE_ID"]) {
		return fmt.Errorf("%w: development relay identity is incomplete", ErrPermissionDenied)
	}
	if values["HERDR_RELAY_TRANSPORT"] != "tailscale-cli" ||
		values["HERDR_RELAY_HOST"] != "127.0.0.1" ||
		values["HERDR_RELAY_PORT"] != strconv.Itoa(DevelopmentBackendPort) ||
		values["HERDR_RELAY_PLUGIN_PORT"] != strconv.Itoa(DevelopmentPluginPort) ||
		values["HERDR_TAILSCALE_CLI_SCOPE"] != "development" ||
		values["HERDR_TAILSCALE_CLI_HTTPS_PORT"] != strconv.Itoa(DevelopmentHTTPSPort) ||
		values["HERDR_TAILSCALE_CLI_STATE_ROOT"] != d.state ||
		values["HERDR_TAILSCALE_CLI_COORDINATION_ROOT"] != d.coordination ||
		values["HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT"] != d.root ||
		values["HERDR_RELAY_PAIRING_SOCKET"] != d.pairingSocket ||
		values["HERDR_REACHABILITY_PORT_MAPPING"] != "0" ||
		values["HERDR_RELAY_REARM_BOOTSTRAP"] != "0" {
		return fmt.Errorf("%w: persisted relay settings escape the isolated development tuple", ErrPermissionDenied)
	}
	phoneApp := values["HERDR_PHONE_APP_URL"]
	if normalized, normalizeErr := normalizePhoneAppOrigin(phoneApp); normalizeErr != nil || normalized != phoneApp {
		return fmt.Errorf("%w: phone-app origin is not canonical HTTPS", ErrPermissionDenied)
	}
	cli := values["HERDR_TAILSCALE_CLI_BIN"]
	if !filepath.IsAbs(cli) || os.Getenv("HERDR_TAILSCALE_CLI_BIN") != cli {
		return fmt.Errorf("%w: selected Tailscale executable is not bound to private development configuration", ErrPermissionDenied)
	}
	if err := requireExecutable(cli); err != nil {
		return err
	}
	for key, expected := range map[string]string{
		"HERDR_RELAY_TOKEN":                     values["HERDR_RELAY_TOKEN"],
		"HERDR_RELAY_INSTANCE_ID":               values["HERDR_RELAY_INSTANCE_ID"],
		"HERDR_RELAY_CONTROL_RUN_ID":            values["HERDR_RELAY_CONTROL_RUN_ID"],
		"HERDR_RELAY_HOST":                      "127.0.0.1",
		"HERDR_RELAY_PORT":                      strconv.Itoa(DevelopmentBackendPort),
		"HERDR_RELAY_PLUGIN_PORT":               strconv.Itoa(DevelopmentPluginPort),
		"HERDR_RELAY_PAIRING_SOCKET":            d.pairingSocket,
		"HERDR_TAILSCALE_CLI_ORIGIN":            origin,
		"HERDR_TAILSCALE_CLI_SCOPE":             "development",
		"HERDR_TAILSCALE_CLI_STATE_ROOT":        d.state,
		"HERDR_TAILSCALE_CLI_COORDINATION_ROOT": d.coordination,
		"HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT":  d.root,
		"HERDR_TAILSCALE_CLI_HTTPS_PORT":        strconv.Itoa(DevelopmentHTTPSPort),
		"HERDR_TAILSCALE_CLI_BIN":               cli,
		"HERDR_TAILSCALE_CLI_NODE_ID":           values["HERDR_TAILSCALE_CLI_NODE_ID"],
		"HERDR_PHONE_APP_URL":                   phoneApp,
		"HERDR_BIN":                             values["HERDR_BIN"],
		"HERDR_SOCKET_PATH":                     values["HERDR_SOCKET_PATH"],
		"HERDR_REACHABILITY_PORT_MAPPING":       "0",
		"HERDR_RELAY_REARM_BOOTSTRAP":           "0",
	} {
		if os.Getenv(key) != expected {
			return fmt.Errorf("%w: process setting %s differs from the private relay configuration", ErrPermissionDenied, key)
		}
	}
	if !filepath.IsAbs(d.herdrBinary) || !filepath.IsAbs(d.herdrSocket) ||
		!sameAbsolutePath(d.herdrBinary, values["HERDR_BIN"]) || !sameAbsolutePath(d.herdrSocket, values["HERDR_SOCKET_PATH"]) {
		return ErrPermissionDenied
	}
	return nil
}

func (d *developmentIsolation) validateCurrentRelease() error {
	if _, err := validatePrivateDirectory(filepath.Join(d.root, "releases")); err != nil {
		return ErrPermissionDenied
	}
	current := filepath.Join(d.root, "current")
	info, err := os.Lstat(current)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return ErrPermissionDenied
	}
	target, err := os.Readlink(current)
	if err != nil || !strings.HasPrefix(target, "releases/") || filepath.Clean(target) != target || strings.Contains(target, `\`) {
		return ErrPermissionDenied
	}
	release := filepath.Join(d.root, target)
	if _, err := validatePrivateDirectory(release); err != nil {
		return ErrPermissionDenied
	}
	resolvedCurrent, err := filepath.EvalSymlinks(current)
	if err != nil || resolvedCurrent != release {
		return ErrPermissionDenied
	}
	for _, directory := range []string{filepath.Join(release, "bin"), filepath.Join(release, "web")} {
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(info) || info.Mode().Perm()&0o022 != 0 {
			return ErrPermissionDenied
		}
	}
	for _, file := range []string{filepath.Join(release, "bin", "herdr-mobile-relay"), filepath.Join(release, "web", "version.json")} {
		info, err := os.Lstat(file)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(info) || info.Mode().Perm()&0o022 != 0 {
			return ErrPermissionDenied
		}
	}
	if err := requireExecutable(d.relayBinary); err != nil {
		return err
	}
	if !sameAbsolutePath(os.Getenv("HERDR_RELAY_BIN"), d.relayBinary) ||
		!sameAbsolutePath(os.Getenv("HERDR_WEB_ROOT"), d.webRoot) {
		return ErrPermissionDenied
	}
	return nil
}

func (d *developmentIsolation) validateMarker() error {
	path := filepath.Join(d.root, ".herdr-dev-tailscale-cli")
	if err := requirePrivateFile(path, 0o600); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ErrPermissionDenied
	}
	want := "HERDR_DEV_TAILSCALE_CLI_ROOT=1\n" +
		"HERDR_DEV_TAILSCALE_CLI_STATE_ROOT=" + d.state + "\n" +
		"HERDR_DEV_TAILSCALE_CLI_COORDINATION_ROOT=" + d.coordination + "\n"
	if string(data) != want {
		return ErrPermissionDenied
	}
	return nil
}

func (d *developmentIsolation) validateProductionSeparation() error {
	productionEnv, servicePath, err := productionEnvironmentPath(d.home)
	if err != nil {
		return err
	}
	hint := os.Getenv("HERDR_DEV_TAILSCALE_CLI_PRODUCTION_ENV_FILE")
	if hint != "" {
		if !filepath.IsAbs(hint) || filepath.Clean(hint) != hint {
			return ErrPermissionDenied
		}
		if productionEnv != "" && !sameAbsolutePath(productionEnv, hint) {
			return fmt.Errorf("%w: supplied production environment differs from the installed service", ErrPermissionDenied)
		}
		productionEnv = hint
	}
	if productionEnv == "" {
		working, _ := os.Getwd()
		for _, candidate := range []string{
			filepath.Join(working, "relay", ".env"),
			filepath.Join(d.home, ".config", "herdr-mobile-relay", "relay.env"),
			filepath.Join(d.home, ".config", "herdr-mobile-relay", ".env"),
			filepath.Join(d.home, ".local", "share", "herdr-mobile-relay", "relay.env"),
		} {
			if _, statErr := os.Lstat(candidate); statErr == nil {
				productionEnv = candidate
				break
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return ErrPermissionDenied
			}
		}
	}

	productionValues := map[string]string{}
	if productionEnv != "" {
		if sameAbsolutePath(productionEnv, d.relayEnv) || pathsOverlap(canonicalMaybeMissing(productionEnv), d.root) {
			return ErrPermissionDenied
		}
		if _, err := os.Lstat(productionEnv); err == nil {
			if err := requirePrivateFile(productionEnv, 0o600); err != nil {
				return err
			}
			productionValues, err = readShellEnvironment(productionEnv)
			if err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) || servicePath != "" {
			return ErrPermissionDenied
		}
	}
	coordinationExpected := filepath.Join(d.home, ".local", "state", "herdr-mobile-relay", "tailscale-cli-coordination")
	if configured := productionValues["HERDR_TAILSCALE_CLI_COORDINATION_ROOT"]; configured != "" {
		coordinationExpected = configured
	}
	if !filepath.IsAbs(coordinationExpected) || !sameAbsolutePath(canonicalMaybeMissing(coordinationExpected), d.coordination) {
		return fmt.Errorf("%w: development coordination must use the installed service's shared private lock root", ErrPermissionDenied)
	}

	configBase := productionValues["XDG_CONFIG_HOME"]
	if configBase == "" {
		configBase = filepath.Join(d.home, ".config")
	}
	cacheBase := productionValues["XDG_CACHE_HOME"]
	if cacheBase == "" {
		cacheBase = filepath.Join(d.home, ".cache")
	}
	dataBase := productionValues["XDG_DATA_HOME"]
	if dataBase == "" {
		dataBase = filepath.Join(d.home, ".local", "share")
	}
	protected := []string{
		filepath.Join(configBase, "herdr-mobile-relay"),
		filepath.Join(cacheBase, "herdr-mobile-relay"),
		filepath.Join(dataBase, "herdr-mobile-relay"),
		filepath.Join(d.home, ".local", "state", "herdr-mobile-relay"),
		filepath.Join(d.home, ".config", "systemd", "user"),
		filepath.Join(d.home, "Library", "LaunchAgents"),
	}
	if productionEnv != "" {
		protected = append(protected, productionEnv)
	}
	for _, key := range []string{
		"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "HERDR_PLUGIN_CONFIG_DIR",
		"HERDR_RELEASE_ROOT", "HERDR_TAILSCALE_CLI_STATE_ROOT", "HERDR_TAILSCALE_CLI_DEVELOPMENT_ROOT",
	} {
		if path := productionValues[key]; path != "" {
			if !filepath.IsAbs(path) {
				return ErrPermissionDenied
			}
			protected = append(protected, path)
		}
	}
	if servicePath != "" {
		protected = append(protected, servicePath, filepath.Dir(servicePath))
	}
	working, _ := os.Getwd()
	if working != "" {
		protected = append(protected,
			filepath.Join(working, "relay", ".dev-tailscale"),
			filepath.Join(working, "relay", ".dev"))
	}
	for _, path := range protected {
		if path == "" || !filepath.IsAbs(path) {
			continue
		}
		protectedPath := canonicalMaybeMissing(path)
		if pathsOverlap(d.root, protectedPath) || pathsOverlap(d.herdrSocket, protectedPath) {
			return fmt.Errorf("%w: development root or Herdr socket overlaps production or unrelated development state", ErrPermissionDenied)
		}
	}
	if pathsOverlap(d.coordination, d.root) || pathsOverlap(d.coordination, d.configHome) ||
		pathsOverlap(d.coordination, d.cacheHome) || pathsOverlap(d.coordination, d.dataHome) {
		return ErrPermissionDenied
	}
	return nil
}

func productionEnvironmentPath(home string) (envPath, servicePath string, err error) {
	var candidate string
	switch runtime.GOOS {
	case "linux":
		servicePath = filepath.Join(home, ".config", "systemd", "user", "herdr-mobile-relay.service")
	case "darwin":
		servicePath = filepath.Join(home, "Library", "LaunchAgents", "com.herdr-mobile-relay.service.plist")
	default:
		return "", "", ErrUnsupported
	}
	info, statErr := os.Lstat(servicePath)
	if errors.Is(statErr, os.ErrNotExist) {
		return "", "", nil
	}
	if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(info) || info.Mode().Perm()&0o022 != 0 {
		return "", "", ErrPermissionDenied
	}
	if info.Size() < 0 || info.Size() > developmentEnvironmentMaxBytes {
		return "", "", ErrPermissionDenied
	}
	data, readErr := os.ReadFile(servicePath)
	if readErr != nil {
		return "", "", ErrPermissionDenied
	}
	if runtime.GOOS == "linux" {
		configured, parseErr := parseSystemdRelayEnvironment(data)
		if parseErr != nil {
			return "", "", parseErr
		}
		candidate = configured
	} else {
		configured, parseErr := parseLaunchdRelayEnvironment(data)
		if parseErr != nil {
			return "", "", parseErr
		}
		candidate = configured
	}
	if candidate == "" || !filepath.IsAbs(candidate) || filepath.Clean(candidate) != candidate {
		return "", "", ErrPermissionDenied
	}
	return candidate, servicePath, nil
}

func parseSystemdRelayEnvironment(data []byte) (string, error) {
	var result string
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Environment=") {
			continue
		}
		value := strings.TrimPrefix(line, "Environment=")
		if !strings.HasPrefix(value, "HERDR_RELAY_ENV=") {
			if strings.Contains(value, "HERDR_RELAY_ENV=") {
				return "", ErrPermissionDenied
			}
			continue
		}
		if found {
			return "", ErrPermissionDenied
		}
		found = true
		value = strings.TrimPrefix(value, "HERDR_RELAY_ENV=")
		decoded, err := decodeSystemdValue(value)
		if err != nil {
			return "", err
		}
		result = decoded
	}
	if result == "" || !filepath.IsAbs(result) || filepath.Clean(result) != result {
		return "", ErrPermissionDenied
	}
	return result, nil
}

func decodeSystemdValue(value string) (string, error) {
	quoted := strings.HasPrefix(value, "\"")
	if quoted {
		if len(value) < 2 || !strings.HasSuffix(value, "\"") {
			return "", ErrPermissionDenied
		}
		value = value[1 : len(value)-1]
	} else if strings.ContainsAny(value, " \t\r\n") {
		return "", ErrPermissionDenied
	}
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' {
			i++
			if i >= len(value) {
				return "", ErrPermissionDenied
			}
			out.WriteByte(value[i])
			continue
		}
		if value[i] == '%' && i+1 < len(value) && value[i+1] == '%' {
			i++
		}
		out.WriteByte(value[i])
	}
	return out.String(), nil
}

type plistElement struct {
	name     xml.Name
	text     string
	children []*plistElement
}

func parseLaunchdRelayEnvironment(data []byte) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", ErrPermissionDenied
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			if text, isText := token.(xml.CharData); isText && strings.TrimSpace(string(text)) != "" {
				return "", ErrPermissionDenied
			}
			continue
		}
		root, err := readPlistElement(decoder, start)
		if err != nil || root.name.Local != "plist" || len(root.children) != 1 || validatePlistDictionaries(root) != nil || consumePlistRemainder(decoder) != nil {
			return "", ErrPermissionDenied
		}
		dict := plistValue(root.children[0], "EnvironmentVariables")
		if dict == nil || dict.name.Local != "dict" {
			return "", ErrPermissionDenied
		}
		value := plistValue(dict, "HERDR_RELAY_ENV")
		if value == nil || value.name.Local != "string" || value.text == "" ||
			!filepath.IsAbs(value.text) || filepath.Clean(value.text) != value.text {
			return "", ErrPermissionDenied
		}
		return value.text, nil
	}
}

func readPlistElement(decoder *xml.Decoder, start xml.StartElement) (*plistElement, error) {
	element := &plistElement{name: start.Name}
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		switch token := token.(type) {
		case xml.StartElement:
			child, err := readPlistElement(decoder, token)
			if err != nil {
				return nil, err
			}
			element.children = append(element.children, child)
		case xml.CharData:
			element.text += string(token)
		case xml.EndElement:
			if token.Name == start.Name {
				return element, nil
			}
		}
	}
}

func validatePlistDictionaries(element *plistElement) error {
	if element.name.Local == "dict" {
		if len(element.children)%2 != 0 {
			return ErrPermissionDenied
		}
		seen := make(map[string]struct{}, len(element.children)/2)
		for i := 0; i < len(element.children); i += 2 {
			key := element.children[i]
			if key.name.Local != "key" {
				return ErrPermissionDenied
			}
			if _, duplicate := seen[key.text]; duplicate {
				return ErrPermissionDenied
			}
			seen[key.text] = struct{}{}
			if err := validatePlistDictionaries(element.children[i+1]); err != nil {
				return err
			}
		}
		return nil
	}
	for _, child := range element.children {
		if err := validatePlistDictionaries(child); err != nil {
			return err
		}
	}
	return nil
}

func consumePlistRemainder(decoder *xml.Decoder) error {
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return ErrPermissionDenied
		}
		switch token := token.(type) {
		case xml.CharData:
			if strings.TrimSpace(string(token)) != "" {
				return ErrPermissionDenied
			}
		case xml.StartElement, xml.EndElement:
			return ErrPermissionDenied
		}
	}
}

func plistValue(dict *plistElement, key string) *plistElement {
	if dict == nil || dict.name.Local != "dict" {
		return nil
	}
	for i := 0; i+1 < len(dict.children); i += 2 {
		if dict.children[i].name.Local == "key" && dict.children[i].text == key {
			return dict.children[i+1]
		}
	}
	return nil
}

func readShellEnvironment(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > developmentEnvironmentMaxBytes {
		return nil, ErrPermissionDenied
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok || key == "" || len(key) > 128 {
			return nil, ErrPermissionDenied
		}
		for i, ch := range key {
			if (ch < 'A' || ch > 'Z') && ch != '_' && (i == 0 || ch < '0' || ch > '9') {
				return nil, ErrPermissionDenied
			}
		}
		if _, duplicate := values[key]; duplicate {
			return nil, ErrPermissionDenied
		}
		value, err := decodeShellEnvironmentValue(raw)
		if err != nil {
			return nil, err
		}
		values[key] = value
	}
	return values, nil
}

func decodeShellEnvironmentValue(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if raw[0] == '\'' {
		var out strings.Builder
		for i := 0; i < len(raw); {
			if raw[i] != '\'' {
				return "", ErrPermissionDenied
			}
			i++
			start := i
			for i < len(raw) && raw[i] != '\'' {
				i++
			}
			if i == len(raw) {
				return "", ErrPermissionDenied
			}
			out.WriteString(raw[start:i])
			i++
			if i == len(raw) {
				return out.String(), nil
			}
			if i+1 < len(raw) && raw[i] == '\\' && raw[i+1] == '\'' {
				out.WriteByte('\'')
				i += 2
			}
		}
		return "", ErrPermissionDenied
	}
	if raw[0] == '"' {
		if len(raw) < 2 || raw[len(raw)-1] != '"' {
			return "", ErrPermissionDenied
		}
		value := raw[1 : len(raw)-1]
		if strings.ContainsAny(value, "$`\n\r") {
			return "", ErrPermissionDenied
		}
		return value, nil
	}
	if strings.ContainsAny(raw, " \t\r\n$`'\\\";") {
		return "", ErrPermissionDenied
	}
	return raw, nil
}

func validateOptionalPrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return ErrPermissionDenied
	}
	resolved, err := validatePrivateDirectory(path)
	if err != nil || resolved != path {
		return ErrPermissionDenied
	}
	return nil
}

func requirePrivateFile(path string, mode os.FileMode) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrPermissionDenied
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode || !ownedByCurrentUser(info) || info.Size() > developmentEnvironmentMaxBytes {
		return ErrPermissionDenied
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return ErrPermissionDenied
	}
	return nil
}

func requireExecutable(path string) error {
	if !filepath.IsAbs(path) {
		return ErrPermissionDenied
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return ErrPermissionDenied
	}
	return nil
}

func validateOptionalSocket(path string) error {
	if path == "" {
		return nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(info) {
		return ErrPermissionDenied
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return ErrPermissionDenied
	}
	return nil
}

func requireOwnedSocket(path string) error {
	if err := validateOptionalSocket(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return ErrPermissionDenied
	}
	return nil
}

func canonicalMaybeMissing(path string) string {
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	parent := filepath.Dir(path)
	for parent != path {
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(resolved, strings.TrimPrefix(strings.TrimPrefix(path, parent), string(filepath.Separator)))
		}
		path, parent = parent, filepath.Dir(parent)
	}
	return filepath.Clean(path)
}

func sameAbsolutePath(left, right string) bool {
	return left != "" && right != "" && filepath.IsAbs(left) && filepath.IsAbs(right) &&
		filepath.Clean(left) == left && filepath.Clean(right) == right && left == right
}

func normalizePhoneAppOrigin(value string) (string, error) {
	return setuphelper.NormalizeExternalHTTPSOrigin(value)
}
