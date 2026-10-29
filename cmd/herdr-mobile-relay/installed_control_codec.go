package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
)

func controlProposal(p localcontrol.InstalledProposal) bool {
	for _, s := range []string{p.Selection, p.Node, p.Profile, p.Attempt, p.Installation} {
		if !controlIdentity(s) {
			return false
		}
	}
	u, e := url.Parse(p.Origin)
	return controlHex(p.Run) && controlHex(p.ID) && p.HTTPSPort != 0 && p.RelayPort != 0 && len(p.Origin) <= 512 && e == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && !u.ForceQuery && !strings.ContainsAny(p.Origin, "\\?#")
}

// Fixed schema token walk rejects duplicates/case/null and limits nesting before
// typed decoding. The entire input allocation is independently bounded to 4097.
func controlObject(d *json.Decoder, proposal bool) error {
	t, e := d.Token()
	if e != nil || t != json.Delim('{') {
		return errControlUsage
	}
	fields := []string{"proposal", "production", "persistent", "check_to_write_race", "port_reuse_risk", "no_rollback", "no_remote_drain"}
	if proposal {
		fields = []string{"selection", "node", "profile", "origin", "https_port", "relay_port", "attempt", "run", "installation", "id"}
	}
	seen := map[string]bool{}
	for d.More() {
		t, e = d.Token()
		k, ok := t.(string)
		if e != nil || !ok || seen[k] || !controlOneOf(k, fields...) {
			return errControlUsage
		}
		seen[k] = true
		if !proposal && k == "proposal" {
			if controlObject(d, true) != nil {
				return errControlUsage
			}
			continue
		}
		t, e = d.Token()
		if e != nil {
			return errControlUsage
		}
		if !proposal {
			if t != true {
				return errControlUsage
			}
			continue
		}
		if k == "https_port" || k == "relay_port" {
			n, ok := t.(json.Number)
			if !ok {
				return errControlUsage
			}
			v, e := n.Int64()
			if e != nil || v < 1 || v > 65535 {
				return errControlUsage
			}
			continue
		}
		s, ok := t.(string)
		if !ok || len(s) > 512 {
			return errControlUsage
		}
	}
	t, e = d.Token()
	if e != nil || t != json.Delim('}') || len(seen) != len(fields) {
		return errControlUsage
	}
	return nil
}
func readInstalledConsent(r io.Reader) (localcontrol.InstalledConsent, error) {
	var c localcontrol.InstalledConsent
	b, e := io.ReadAll(io.LimitReader(r, 4097))
	if e != nil || len(b) > 4096 || !utf8.Valid(b) {
		return c, errControlUsage
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if controlObject(d, false) != nil {
		return c, errControlUsage
	}
	if _, e = d.Token(); e != io.EOF {
		return c, errControlUsage
	}
	if json.Unmarshal(b, &c) != nil || !controlProposal(c.Proposal) {
		return localcontrol.InstalledConsent{}, errControlUsage
	}
	return c, nil
}
