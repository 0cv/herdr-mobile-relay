package supervisor

import (
	"sort"
	"strings"
)

var relayEnvironmentKeys = map[string]bool{
	"CLAUDE_CONFIG_DIR": true, "CODEX_HOME": true, "HOME": true, "KIMI_CODE_HOME": true,
	"HERMES_HOME": true, "HERDR_HERMES_DATA_DIRS": true,
	"LANG": true, "LC_ALL": true, "LC_CTYPE": true, "OMO_CODING_AGENT_DIR": true, "PATH": true,
	"PI_CODING_AGENT_DIR": true, "SENPI_CODING_AGENT_DIR": true,
	"SSL_CERT_DIR": true, "SSL_CERT_FILE": true, "TMPDIR": true, "TZ": true,
	"XDG_CACHE_HOME": true, "XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true,
	"HERDR_APP_DEPLOY_NODE_DIR": true, "HERDR_APP_DEPLOY_NPX": true, "HERDR_APP_DEPLOY_ORIGIN": true,
	"HERDR_ALLOWED_ORIGINS": true, "HERDR_BIN": true, "HERDR_CLAUDE_CONFIG_DIRS": true,
	"HERDR_CLOUDFLARE_PAGES_BRANCH": true, "HERDR_CLOUDFLARE_PAGES_PROJECT": true,
	"HERDR_CODEX_CONFIG_DIRS": true, "HERDR_GATEWAY_SELECTION": true, "HERDR_GATEWAY_URL": true,
	"HERDR_OMO_CONFIG_DIRS": true, "HERDR_OMP_CONFIG_DIRS": true, "HERDR_OPENCODE_DATA_DIRS": true,
	"HERDR_PIPER_RUNTIME_BASE_URL": true, "HERDR_PIPER_VOICES": true, "HERDR_PIPER_VOICE_BASE_URL": true,
	"HERDR_PI_CONFIG_DIRS": true, "HERDR_PLUGIN_CONFIG_DIR": true, "HERDR_QODER_CONFIG_DIRS": true,
	"HERDR_REACHABILITY_PORT_MAPPING": true, "HERDR_RELEASE_ROOT": true, "HERDR_SOCKET_PATH": true,
	"HERDR_TRANSPORT_FORCE_RELAY": true, "HERDR_WEBRTC_UDP_PORT": true,
	"HERDR_RELAY_ACTIVE_GENERATION": true, "HERDR_RELAY_ACTIVE_RUNTIME": true, "HERDR_RELAY_ENV": true, "HERDR_RELAY_EXPECTED_INVENTORY": true,
	"HERDR_RELAY_HOST": true, "HERDR_RELAY_INSTANCE_ID": true, "HERDR_RELAY_LOG_FORMAT": true,
	"HERDR_RELAY_MANAGED_DEPLOYMENT": true, "HERDR_RELAY_PLUGIN_PORT": true, "HERDR_RELAY_POLL_INTERVAL": true,
	"HERDR_RELAY_PORT": true, "HERDR_RELAY_REARM_BOOTSTRAP": true, "HERDR_RELAY_SERVICE_NAME": true,
	"HERDR_RELAY_TOKEN": true, "HERDR_RELAY_TOPOLOGY_COMMIT_HELPER": true,
	"OURO_LEDGER_ROOT": true, "OURO_REMOTE_CONFIG": true, "OURO_SESSION_MAP": true,
	"OURO_SHIM_DIRECTORY": true, "OURO_ZDOTDIR": true,
}

var tunnelEnvironmentKeys = map[string]bool{
	"LANG": true, "LC_ALL": true, "LC_CTYPE": true, "PATH": true,
	"SSL_CERT_DIR": true, "SSL_CERT_FILE": true, "TMPDIR": true, "TZ": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true,
}

func RelayEnvironment(source []string) []string {
	return allowEnvironment(source, relayEnvironmentKeys)
}

func TunnelEnvironment(source []string) []string {
	return allowEnvironment(source, tunnelEnvironmentKeys)
}

func allowEnvironment(source []string, allowed map[string]bool) []string {
	values := make(map[string]string, len(allowed))
	for _, entry := range source {
		key, value, found := strings.Cut(entry, "=")
		if found && allowed[key] {
			values[key] = value
		}
	}
	result := make([]string, 0, len(values))
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	sort.Strings(result)
	return result
}
