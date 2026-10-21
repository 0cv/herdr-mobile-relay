package update

import (
	"maps"
	"strings"
	"testing"
)

func TestUpdaterRetainsPrivateFileWithoutRawTokens(t *testing.T) {
	t.Setenv("GH_TOKEN", "synthetic-raw-token")
	t.Setenv("GITHUB_TOKEN", "synthetic-raw-token")
	t.Setenv("HERDR_GITHUB_TOKEN_FILE", "/private/updater-file")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid")
	environment := strings.Join(environmentWith("HERDR_MOBILE_RELAY_NO_AUTO_SETUP", "1"), "\n")
	if strings.Contains(environment, "synthetic-raw-token") {
		t.Fatal("raw credential inherited by updater subprocess")
	}
	for _, expected := range []string{"HERDR_GITHUB_TOKEN_FILE=/private/updater-file", "HTTPS_PROXY=http://proxy.invalid"} {
		if !strings.Contains(environment, expected) {
			t.Fatal("updater lost required environment")
		}
	}
	job := updateWorkerJob("/relay", "/job", "cloudflare", func(key string) (string, bool) {
		if key == "HERDR_GITHUB_TOKEN_FILE" {
			return "/private/updater-file", true
		}
		if key == "GH_TOKEN" || key == "GITHUB_TOKEN" {
			return "synthetic-raw-token", true
		}
		return "", false
	})
	if !maps.Equal(job.Environment, map[string]string{
		"HERDR_RELAY_TRANSPORT":   "cloudflare",
		"HERDR_GITHUB_TOKEN_FILE": "/private/updater-file",
	}) {
		t.Fatal("incorrect updater launchd environment")
	}
	args := strings.Join(systemdRunArgs(job), "\n")
	if !strings.Contains(args, "HERDR_GITHUB_TOKEN_FILE=/private/updater-file") || strings.Contains(args, "synthetic-raw-token") {
		t.Fatal("incorrect updater systemd-run environment")
	}
}

func TestUpdaterHandsCredentialFreeProxyToDetachedWorker(t *testing.T) {
	values := map[string]string{
		"HTTPS_PROXY": "http://proxy.invalid:3128",
		"NO_PROXY":    "localhost,127.0.0.1",
		"HTTP_PROXY":  "http://user:synthetic-proxy-secret@proxy.invalid:3128",
		"ALL_PROXY":   " ",
		"https_proxy": "proxy.invalid:3128",
		"http_proxy":  "user:synthetic-proxy-secret@proxy.invalid:3128",
		"all_proxy":   "http://proxy.invalid:port",
		"no_proxy":    "\t ",
	}
	job := updateWorkerJob("/relay", "/job", "cloudflare", func(key string) (string, bool) {
		value, present := values[key]
		return value, present
	})
	if !maps.Equal(job.Environment, map[string]string{
		"HERDR_RELAY_TRANSPORT": "cloudflare",
		"HTTPS_PROXY":           "http://proxy.invalid:3128",
		"NO_PROXY":              "localhost,127.0.0.1",
		"https_proxy":           "proxy.invalid:3128",
	}) {
		t.Fatal("updater job must carry credential-free proxy settings and drop credentialed, blank and unparsable ones")
	}
	args := strings.Join(systemdRunArgs(job), "\n")
	for _, expected := range []string{
		"--setenv=HTTPS_PROXY=http://proxy.invalid:3128",
		"--setenv=NO_PROXY=localhost,127.0.0.1",
		"--setenv=https_proxy=proxy.invalid:3128",
	} {
		if !strings.Contains(args, expected) {
			t.Fatalf("systemd-run environment lost %s", expected)
		}
	}
	if strings.Contains(args, "synthetic-proxy-secret") || strings.Contains(args, "HTTP_PROXY=") ||
		strings.Contains(args, "http_proxy=") || strings.Contains(args, "ALL_PROXY=") || strings.Contains(args, "all_proxy=") {
		t.Fatal("a credentialed, blank or unparsable proxy reached the systemd-run argument vector")
	}
}

func TestUpdaterProxyForwardingFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name  string
		key   string
		value string
		want  bool
	}{
		{"http URL", "HTTPS_PROXY", "http://proxy.invalid:3128", true},
		{"http URL with a trailing slash", "HTTP_PROXY", "http://proxy.invalid:3128/", true},
		{"scheme-less host and port", "https_proxy", "proxy.invalid:3128", true},
		{"SOCKS URL", "ALL_PROXY", "socks5h://proxy.invalid:1080", true},
		{"bracketed IPv6 host", "all_proxy", "http://[::1]:3128", true},
		{"host list", "NO_PROXY", "localhost, 127.0.0.1, .internal.invalid", true},
		{"lowercase host list", "no_proxy", "localhost", true},
		{"userinfo with a password", "HTTP_PROXY", "http://user:synthetic-proxy-secret@proxy.invalid:3128", false},
		{"userinfo with only a token", "HTTPS_PROXY", "https://synthetic-proxy-secret@proxy.invalid", false},
		{"empty userinfo", "ALL_PROXY", "http://@proxy.invalid", false},
		{"scheme-less userinfo", "http_proxy", "user:synthetic-proxy-secret@proxy.invalid:3128", false},
		{"at sign outside the authority", "HTTPS_PROXY", "http://proxy.invalid/a@b", false},
		{"blank", "HTTP_PROXY", "", false},
		{"whitespace only", "ALL_PROXY", " \t ", false},
		{"blank host list", "NO_PROXY", "  ", false},
		{"unparsable port", "HTTP_PROXY", "http://proxy.invalid:port", false},
		{"unterminated IPv6 host", "HTTPS_PROXY", "http://[::1", false},
		{"missing scheme name", "ALL_PROXY", "://proxy.invalid", false},
		{"no host", "HTTP_PROXY", "http://", false},
		{"space inside the host", "HTTP_PROXY", "http://proxy invalid:3128", false},
		{"newline inside a URL", "HTTP_PROXY", "http://proxy.invalid:3128\nHERDR_RELAY_ENV=/x", false},
		{"NUL inside a host list", "NO_PROXY", "localhost\x00,example.invalid", false},
		{"newline inside a host list", "no_proxy", "localhost\n,example.invalid", false},
	} {
		job := updateWorkerJob("/relay", "/job", "cloudflare", func(key string) (string, bool) {
			return test.value, key == test.key
		})
		value, forwarded := job.Environment[test.key]
		if forwarded != test.want || (forwarded && value != test.value) {
			t.Errorf("%s: %s forwarded = %v, want %v", test.name, test.key, forwarded, test.want)
		}
		wantEntries := 1 // the transport
		if test.want {
			wantEntries++
		}
		if len(job.Environment) != wantEntries {
			t.Errorf("%s: job environment has %d entries, want %d", test.name, len(job.Environment), wantEntries)
		}
	}
}
