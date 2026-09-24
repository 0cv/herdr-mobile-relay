package tailscale

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// This is a source contract, not binary provenance or runtime qualification.
const SourceRelease = "1.102.4"
const SourceCommit = "bbcd7d1fc2054b9189ebc1531acf74bd880ca0c8"

// strictJSON checks every member before decoding: encoding/json otherwise
// silently accepts duplicate members, case aliases, and invalid UTF-8.
func strictJSON(data []byte) error {
	if len(data) > MaxOutputBytes {
		return ErrOutputTooLong
	}
	if !utf8.Valid(data) {
		return ErrNotJSON
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	tokens := 0
	var value func(int) error
	value = func(depth int) error {
		if depth > 32 || tokens > 100000 {
			return ErrNotJSON
		}
		tokens++
		t, e := d.Token()
		if e != nil {
			return ErrNotJSON
		}
		delimiter, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, e := d.Token()
				if e != nil {
					return ErrNotJSON
				}
				s, ok := key.(string)
				if !ok {
					return ErrNotJSON
				}
				folded := strings.ToLower(s)
				if seen[folded] {
					return ErrNotJSON
				}
				seen[folded] = true
				if e = value(depth + 1); e != nil {
					return e
				}
			}
			t, e = d.Token()
			if e != nil || t != json.Delim('}') {
				return ErrNotJSON
			}
		case '[':
			for d.More() {
				if e := value(depth + 1); e != nil {
					return e
				}
			}
			t, e = d.Token()
			if e != nil || t != json.Delim(']') {
				return ErrNotJSON
			}
		default:
			return ErrNotJSON
		}
		return nil
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrNotJSON
	}
	return nil
}

func object(data []byte) (map[string]json.RawMessage, error) {
	if err := strictJSON(data); err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return nil, ErrNotJSON
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil || m == nil {
		return nil, ErrNotJSON
	}
	return m, nil
}

// ExtractJSONField returns one exact, top-level scalar after validating the
// complete bounded JSON document. It is used by the packaged CLI for shell
// callers; it never searches nested objects or quoted text.
func ExtractJSONField(data []byte, key, kind string) (string, error) {
	if key == "" || len(key) > 256 {
		return "", ErrNotJSON
	}
	fields, err := object(data)
	if err != nil {
		return "", err
	}
	raw, ok := fields[key]
	if !ok {
		return "", ErrNotJSON
	}
	switch kind {
	case "bool":
		var value bool
		if err := scalar(fields, key, &value, true); err != nil {
			return "", err
		}
		return strconv.FormatBool(value), nil
	case "string":
		var value string
		if err := scalar(fields, key, &value, true); err != nil {
			return "", err
		}
		if strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
			return "", ErrNotJSON
		}
		return value, nil
	case "number":
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 || (len(raw) > 1 && raw[0] == '0') {
			return "", ErrNotJSON
		}
		for _, digit := range raw {
			if digit < '0' || digit > '9' {
				return "", ErrNotJSON
			}
		}
		return string(raw), nil
	default:
		return "", ErrNotJSON
	}
}

func keys(m map[string]json.RawMessage, allowed ...string) error {
	for k := range m {
		found := false
		for _, a := range allowed {
			if k == a {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: unsupported field %s", ErrNotJSON, k)
		}
	}
	return nil
}
func scalar(m map[string]json.RawMessage, k string, target any, required bool) error {
	r, ok := m[k]
	if !ok {
		if required {
			return fmt.Errorf("%w: missing %s", ErrNotJSON, k)
		}
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(r), []byte("null")) || json.Unmarshal(r, target) != nil {
		return fmt.Errorf("%w: invalid %s", ErrNotJSON, k)
	}
	return nil
}
func stringField(m map[string]json.RawMessage, k string) (string, error) {
	var s string
	e := scalar(m, k, &s, true)
	return s, e
}
func nonemptyObject(r []byte) (map[string]json.RawMessage, error) {
	m, e := object(r)
	if e != nil {
		return nil, e
	}
	if len(m) == 0 {
		return nil, ErrNotJSON
	}
	return m, nil
}

// Route is observation only. Session is empty for background configuration.
type Route struct {
	Session  string `json:"session"`
	Listener string `json:"listener"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Handler  string `json:"handler"`
	Path     string `json:"path"`
	Backend  string `json:"backend"`
}

func canonicalHostPort(s string) (string, int, error) {
	h, p, e := net.SplitHostPort(s)
	if e != nil || !validHostname(h) || h != strings.ToLower(h) {
		return "", 0, ErrNotJSON
	}
	n, e := strconv.Atoi(p)
	if e != nil || n < 1 || n > 65535 || strconv.Itoa(n) != p {
		return "", 0, ErrNotJSON
	}
	return h, n, nil
}

func parseServe(data []byte) (ServeStatus, error) {
	root, e := object(data)
	if e != nil {
		return ServeStatus{}, e
	}
	result := ServeStatus{Complete: true, Configured: len(root) > 0}
	var scope func(map[string]json.RawMessage, string) error
	scope = func(m map[string]json.RawMessage, session string) error {
		if e := keys(m, "TCP", "Web", "AllowFunnel", "Foreground"); e != nil {
			return e
		}
		if raw, ok := m["AllowFunnel"]; ok {
			entries, e := nonemptyObject(raw)
			if e != nil {
				return e
			}
			result.RetainedFunnel = true
			for hp, r := range entries {
				if _, _, e := canonicalHostPort(hp); e != nil {
					return e
				}
				var b bool
				if string(r) == "null" || json.Unmarshal(r, &b) != nil {
					return ErrNotJSON
				}
				if b {
					result.FunnelConfigured = true
				}
			}
		}
		tcp := map[string]json.RawMessage{}
		web := map[string]json.RawMessage{}
		if r, ok := m["TCP"]; ok {
			var e error
			tcp, e = nonemptyObject(r)
			if e != nil {
				return e
			}
		}
		if r, ok := m["Web"]; ok {
			var e error
			web, e = nonemptyObject(r)
			if e != nil {
				return e
			}
		}
		used := map[string]bool{}
		for port, r := range tcp {
			n, e := strconv.Atoi(port)
			if e != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
				return ErrNotJSON
			}
			handler, e := object(r)
			if e != nil || keys(handler, "HTTPS") != nil {
				return ErrNotJSON
			}
			var https bool
			if scalar(handler, "HTTPS", &https, true) != nil || !https {
				return ErrNotJSON
			}
		}
		for hp, r := range web {
			host, port, e := canonicalHostPort(hp)
			if e != nil {
				return e
			}
			ps := strconv.Itoa(port)
			if _, ok := tcp[ps]; !ok {
				return ErrNotJSON
			}
			used[ps] = true
			w, e := object(r)
			if e != nil || keys(w, "Handlers") != nil {
				return ErrNotJSON
			}
			handlers, e := nonemptyObject(w["Handlers"])
			if e != nil {
				return e
			}
			for path, r := range handlers {
				if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#\\\r\n\t") {
					return ErrNotJSON
				}
				h, e := object(r)
				if e != nil || keys(h, "Proxy") != nil {
					return ErrNotJSON
				}
				backend, e := stringField(h, "Proxy")
				if e != nil || !validProxy(backend) {
					return ErrNotJSON
				}
				result.ObservedRoutes = append(result.ObservedRoutes, Route{Session: session, Listener: "HTTPS", Host: host, Port: port, Handler: "Proxy", Path: path, Backend: backend})
			}
		}
		if len(used) != len(tcp) {
			return ErrNotJSON
		}
		if r, ok := m["Foreground"]; ok {
			if session != "" {
				return ErrNotJSON
			}
			sessions, e := nonemptyObject(r)
			if e != nil {
				return e
			}
			for id, r := range sessions {
				if id == "" || strings.TrimSpace(id) != id {
					return ErrNotJSON
				}
				child, e := nonemptyObject(r)
				if e != nil {
					return e
				}
				if e = scope(child, id); e != nil {
					return e
				}
			}
		}
		return nil
	}
	if e = scope(root, ""); e != nil {
		return ServeStatus{}, e
	}
	sort.Slice(result.ObservedRoutes, func(i, j int) bool {
		a, _ := json.Marshal(result.ObservedRoutes[i])
		b, _ := json.Marshal(result.ObservedRoutes[j])
		return string(a) < string(b)
	})
	for _, r := range result.ObservedRoutes {
		result.Routes = append(result.Routes, net.JoinHostPort(r.Host, strconv.Itoa(r.Port))+r.Path)
	}
	return result, nil
}

// The supported subset is a full HTTP(S) URL, not shorthand or other schemes.
func validProxy(backend string) bool {
	if strings.ContainsAny(backend, " \t\r\n\\") {
		return false
	}
	u, err := url.Parse(backend)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	if net.ParseIP(u.Hostname()) == nil && !validHostname(u.Hostname()) {
		return false
	}
	if p := u.Port(); p != "" {
		n, e := strconv.Atoi(p)
		if e != nil || n < 1 || n > 65535 || strconv.Itoa(n) != p {
			return false
		}
	}
	return !strings.HasSuffix(u.Host, ":")
}

// Reject aliases of relevant keys while allowing unrelated status telemetry.
func canonicalKeys(m map[string]json.RawMessage, names ...string) error {
	for k := range m {
		for _, name := range names {
			if strings.EqualFold(strings.NewReplacer("_", "", "-", "").Replace(k), name) && k != name {
				return ErrNotJSON
			}
		}
	}
	return nil
}

func parseIdentity(data []byte) (Status, error) {
	m, e := object(data)
	if e != nil {
		return Status{}, e
	}
	if e = canonicalKeys(m, "BackendState", "Version", "Self", "CurrentTailnet", "CertDomains", "User", "MagicDNSSuffix"); e != nil {
		return Status{}, e
	}
	s := Status{}
	if scalar(m, "BackendState", &s.BackendState, true) != nil {
		return s, ErrNotJSON
	}
	switch s.BackendState {
	case "NoState", "NeedsLogin", "NeedsMachineAuth", "Stopped", "Starting":
		return s, nil
	case "Running":
	default:
		return s, ErrNotJSON
	}
	if scalar(m, "Version", &s.Version, true) != nil || s.Version == "" {
		return s, ErrNotJSON
	}
	self, e := object(m["Self"])
	if e != nil {
		return s, e
	}
	if canonicalKeys(self, "ID", "DNSName", "UserID") != nil {
		return s, ErrNotJSON
	}
	if scalar(self, "ID", &s.NodeID, true) != nil || s.NodeID == "" || strings.TrimSpace(s.NodeID) != s.NodeID || scalar(self, "DNSName", &s.DNSName, true) != nil || scalar(self, "UserID", &s.UserID, true) != nil || s.UserID <= 0 {
		return s, ErrNotJSON
	}
	if !strings.HasSuffix(s.DNSName, ".") {
		return s, ErrNotJSON
	}
	s.DNSName = strings.TrimSuffix(s.DNSName, ".")
	if !validHostname(s.DNSName) {
		return s, ErrNotJSON
	}
	tail, e := object(m["CurrentTailnet"])
	if e != nil {
		return s, e
	}
	if keys(tail, "Name", "MagicDNSSuffix", "MagicDNSEnabled") != nil || scalar(tail, "Name", &s.TailnetName, true) != nil || s.TailnetName == "" || scalar(tail, "MagicDNSSuffix", &s.MagicDNSSuffix, true) != nil || scalar(tail, "MagicDNSEnabled", &s.MagicDNSEnabled, true) != nil {
		return s, ErrNotJSON
	}
	if !validHostname(s.MagicDNSSuffix) || !strings.HasSuffix(strings.ToLower(s.DNSName), "."+strings.ToLower(s.MagicDNSSuffix)) {
		return s, ErrNotJSON
	}
	if _, ok := m["MagicDNSSuffix"]; ok {
		var suffix string
		if scalar(m, "MagicDNSSuffix", &suffix, true) != nil || (suffix != "" && suffix != s.MagicDNSSuffix) {
			return s, ErrNotJSON
		}
	}
	if r, ok := m["CertDomains"]; ok && string(r) != "null" {
		if json.Unmarshal(r, &s.CertDomains) != nil {
			return s, ErrNotJSON
		}
		for _, d := range s.CertDomains {
			if !validHostname(d) {
				return s, ErrNotJSON
			}
		}
	}
	if r, ok := m["User"]; ok && string(r) != "null" {
		users, e := object(r)
		if e != nil {
			return s, e
		}
		for id, r := range users {
			n, e := strconv.ParseInt(id, 10, 64)
			if e != nil || n <= 0 || strconv.FormatInt(n, 10) != id {
				return s, ErrNotJSON
			}
			account, e := object(r)
			if e != nil {
				return s, e
			}
			if canonicalKeys(account, "ID", "LoginName", "DisplayName", "ProfilePicURL") != nil {
				return s, ErrNotJSON
			}
			var uid int64
			if scalar(account, "ID", &uid, true) != nil || uid != n {
				return s, ErrNotJSON
			}
			for _, key := range []string{"LoginName", "DisplayName", "ProfilePicURL"} {
				var v string
				if scalar(account, key, &v, false) != nil {
					return s, ErrNotJSON
				}
			}
			if n == s.UserID {
				s.Account = account
			}
		}
	}
	s.SelfPresent = true
	s.LoggedIn = true
	return s, nil
}

func validateVersion(data []byte, statusVersion string) error {
	m, e := object(data)
	if e != nil {
		return e
	}
	if keys(m, "majorMinorPatch", "short", "long", "gitCommit", "daemonLong", "isDev", "gitDirty", "unstableBranch", "extraGitCommit", "osVariant", "gitCommitTime", "tailscaleGoGitHash", "cap") != nil {
		return ErrNotJSON
	}
	values := map[string]string{}
	for _, k := range []string{"majorMinorPatch", "short", "long", "gitCommit", "daemonLong", "extraGitCommit", "osVariant", "gitCommitTime", "tailscaleGoGitHash"} {
		var v string
		required := k == "majorMinorPatch" || k == "short" || k == "long" || k == "gitCommit" || k == "daemonLong"
		if scalar(m, k, &v, required) != nil {
			return ErrNotJSON
		}
		values[k] = v
	}
	for _, k := range []string{"isDev", "gitDirty", "unstableBranch"} {
		var b bool
		if scalar(m, k, &b, false) != nil || b {
			return ErrNotJSON
		}
	}
	var cap int
	if scalar(m, "cap", &cap, true) != nil || cap < 0 {
		return ErrNotJSON
	}
	if values["majorMinorPatch"] != SourceRelease || values["short"] != SourceRelease || values["gitCommit"] != SourceCommit || values["long"] != values["daemonLong"] || values["long"] != statusVersion {
		return fmt.Errorf("unsupported or inconsistent Tailscale version")
	}
	parts := strings.Split(values["long"], "-")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != SourceRelease {
		return ErrNotJSON
	}
	commit := strings.TrimPrefix(parts[1], "t")
	if len(commit) < 7 || !strings.HasPrefix(SourceCommit, commit) {
		return ErrNotJSON
	}
	extra := values["extraGitCommit"]
	if len(parts) == 3 {
		suffix := strings.TrimPrefix(parts[2], "g")
		if len(extra) != 40 || len(suffix) < 7 || !strings.HasPrefix(extra, suffix) {
			return ErrNotJSON
		}
		for _, c := range extra {
			if !strings.ContainsRune("0123456789abcdef", c) {
				return ErrNotJSON
			}
		}
	} else if extra != "" {
		return ErrNotJSON
	}
	return nil
}

// ExactRouteMatch compares independently supplied authority, never discovers it.
func ExactRouteMatch(observed ServeStatus, expected Route) bool {
	if !observed.Complete || !observed.Configured || observed.FunnelConfigured || len(observed.ObservedRoutes) != 1 || expected.Session == "" || expected.Listener != "HTTPS" || expected.Handler != "Proxy" || expected.Path != "/" {
		return false
	}
	if _, _, e := canonicalHostPort(net.JoinHostPort(expected.Host, strconv.Itoa(expected.Port))); e != nil {
		return false
	}
	const prefix = "http://127.0.0.1:"
	if !strings.HasPrefix(expected.Backend, prefix) {
		return false
	}
	p := strings.TrimPrefix(expected.Backend, prefix)
	n, e := strconv.Atoi(p)
	if e != nil || n < 1 || n > 65535 || strconv.Itoa(n) != p {
		return false
	}
	return observed.ObservedRoutes[0] == expected && !observed.RetainedFunnel
}
