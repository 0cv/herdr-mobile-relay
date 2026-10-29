package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
)

// The wizard is a separate authenticated installed-control client. It never
// creates setup authority, synthesizes a proposal or retries a publish after
// possible dispatch. The setup owner continues independently until its own
// retained acknowledgement and complete cancellation/join.
func runInstalledControlWizard(in io.Reader, out io.Writer) (int, error) {
	if !installedControlEnabled() {
		return 1, errControlRefused
	}
	cfg, err := config.LoadInstalled()
	if err != nil {
		return 1, errControlRefused
	}
	selected, err := config.BindInstalledCLI(cfg)
	if err != nil {
		return 1, errControlRefused
	}
	var proposal installedControlOutput
	for attempt := 0; attempt < 50; attempt++ {
		var buf bytes.Buffer
		code, e := runInstalledControl([]string{"proposal"}, nil, &buf)
		if e == nil && code == 0 && buf.Len() <= 8192 && json.Unmarshal(buf.Bytes(), &proposal) == nil && proposal.Code == "ok" && proposal.Transmission == localcontrol.InstalledValidatedReply && proposal.Proposal != nil {
			break
		}
		if attempt == 49 {
			return 1, errControlRefused
		}
		time.Sleep(100 * time.Millisecond)
	}
	p := proposal.Proposal
	if p == nil || !controlProposal(*p) || proposal.Run != p.Run || proposal.Installation != p.Installation || selected.Validate(cfg) != nil {
		return 1, errControlRefused
	}
	// Every acknowledgement is separately typed. A blank line, EOF or an
	// apparently agreeing status cannot authorize persistent publication.
	confirmation := fmt.Sprintf("PUBLISH INSTALLED ROUTE scope=production node=%s origin=%s https-port=%d backend=127.0.0.1:%d attempt=%s", p.Node, p.Origin, p.HTTPSPort, p.RelayPort, p.Attempt)
	reader := bufio.NewReaderSize(in, 512)
	for _, phrase := range []string{
		confirmation,
		"I ACCEPT PRODUCTION ROUTE " + p.ID,
		"I ACCEPT PERSISTENT SERVE " + p.ID,
		"I ACCEPT CHECK-TO-WRITE RACE " + p.ID,
		"I ACCEPT BACKEND PORT REUSE RISK " + p.ID,
		"I ACCEPT NO AUTOMATIC ROLLBACK " + p.ID,
		"I ACCEPT NO REMOTE DRAIN " + p.ID,
	} {
		if _, err := fmt.Fprintf(out, "Type exactly: %s\n", phrase); err != nil {
			return 1, errControlOutput
		}
		if _, err := readRouteConfirmation(reader, phrase); err != nil {
			return 1, errControlUsage
		}
	}
	if selected.Validate(cfg) != nil {
		return 1, errControlRefused
	}
	consent := localcontrol.InstalledConsent{Proposal: *p, Production: true, Persistent: true, CheckToWriteRace: true, PortReuseRisk: true, NoRollback: true, NoRemoteDrain: true}
	wire, err := json.Marshal(consent)
	if err != nil || len(wire) > 4096 {
		return 1, errControlUsage
	}
	var reply bytes.Buffer
	code, err := runInstalledControl([]string{"publish", "--run", p.Run}, bytes.NewReader(wire), &reply)
	if err != nil || code != 0 {
		return 1, errControlRefused
	}
	var result installedControlOutput
	if json.Unmarshal(reply.Bytes(), &result) != nil || result.Code != "ok" || result.Publication != "acknowledged" || result.Run != p.Run || result.Installation != p.Installation {
		return 1, errControlRefused
	}
	_, err = fmt.Fprintln(out, "Owned route publication acknowledged; waiting for complete setup retirement and fresh service activation.")
	return status(err)
}
