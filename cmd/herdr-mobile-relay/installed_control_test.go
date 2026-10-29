package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
)

const controlTestRun = "0123456789abcdef0123456789abcdef"

func controlTestConsent() localcontrol.InstalledConsent {
	return localcontrol.InstalledConsent{Proposal: localcontrol.InstalledProposal{InstalledSelection: localcontrol.InstalledSelection{Selection: "selection", Node: "node", Profile: "profile", Origin: "https://fixture.invalid", HTTPSPort: 443, RelayPort: 9375, Attempt: "attempt"}, Run: controlTestRun, Installation: "installation", ID: controlTestRun}, Production: true, Persistent: true, CheckToWriteRace: true, PortReuseRisk: true, NoRollback: true, NoRemoteDrain: true}
}
func TestInstalledControlFlags(t *testing.T) {
	cases := map[string][]string{
		"empty": {}, "unknown": {"secret"}, "missing-run": {"arm", "--operation-id", "stable"}, "missing-id": {"arm", "--run", controlTestRun}, "receipt-run": {"status", "--operation-id", "stable"}, "reprint-run": {"reprint"}, "publish-run": {"publish"}, "duplicate": {"status", "--run", controlTestRun, "--run", controlTestRun}, "empty-value": {"status", "--run", ""}, "uppercase": {"status", "--run", strings.ToUpper(controlTestRun)}, "padding": {"arm", "--run", controlTestRun, "--operation-id", " stable"}, "long-id": {"arm", "--run", controlTestRun, "--operation-id", strings.Repeat("x", 129)}, "path": {"arm", "--run", controlTestRun, "--operation-id", "a/b"}, "inappropriate": {"publish", "--run", controlTestRun, "--operation-id", "id"}, "capability": {"status", "--capability", "canary"}, "positional": {"status", "canary"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, e := parseInstalledControl(args); e != errControlUsage {
				t.Fatal("accepted")
			}
		})
	}
	for _, op := range []string{"proposal", "status", "publish", "arm", "reprint"} {
		t.Run(op, func(t *testing.T) {
			a := []string{op, "--run", controlTestRun}
			if op == "arm" {
				a = append(a, "--operation-id", "stable:1")
			}
			i, e := parseInstalledControl(a)
			if e != nil || i.ExpectedRun != controlTestRun {
				t.Fatal("refused")
			}
		})
	}
}
func TestInstalledControlConsent(t *testing.T) {
	c := controlTestConsent()
	b, _ := json.Marshal(c)
	s := string(b)
	cases := map[string]string{"duplicate": strings.Replace(s, `"production":true`, `"production":true,"production":true`, 1), "case": strings.Replace(s, `"run"`, `"Run"`, 1), "missing": strings.Replace(s, `"production":true,`, "", 1), "false": strings.Replace(s, `"production":true`, `"production":false`, 1), "null": strings.Replace(s, `"proposal":{`, `"proposal":null,"extra":{`, 1), "unknown": strings.Replace(s, `"node"`, `"capability"`, 1), "array": "[]", "trailing": s + " {}", "oversize": strings.Repeat(" ", 4097), "nested": `{"proposal":{"selection":` + strings.Repeat("[", 1000), "utf8": s + string([]byte{255}), "port": strings.Replace(s, `"https_port":443`, `"https_port":65536`, 1), "wrong-type": strings.Replace(s, `"production":true`, `"production":"true"`, 1)}
	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			if _, e := readInstalledConsent(strings.NewReader(s)); e != errControlUsage {
				t.Fatal("accepted")
			}
		})
	}
	t.Run("exact-values", func(t *testing.T) {
		c.Proposal.Selection = "<exact>&"
		b, _ := json.Marshal(c)
		got, e := readInstalledConsent(bytes.NewReader(b))
		if e != nil || got != c {
			t.Fatal("changed values")
		}
	})
	t.Run("bounded-read", func(t *testing.T) {
		r := strings.NewReader(strings.Repeat(" ", 5000))
		_, _ = readInstalledConsent(r)
		if r.Len() != 903 {
			t.Fatal("unbounded read")
		}
	})
}

type controlTestWriter struct {
	calls int
	n     int
	err   error
}

func (w *controlTestWriter) Write(b []byte) (int, error) {
	w.calls++
	if w.n < 0 {
		return len(b), w.err
	}
	return w.n, w.err
}
func TestInstalledControlOutput(t *testing.T) {
	for _, name := range []string{"short", "error", "oversize", "success"} {
		t.Run(name, func(t *testing.T) {
			w := &controlTestWriter{}
			o := installedControlOutput{Version: 1}
			if name == "error" {
				w.err = errors.New("private-canary")
			}
			if name == "success" {
				w.n = -1
			}
			if name == "oversize" {
				o.Invitation = strings.Repeat("<", 8192)
			}
			code, e := writeInstalledControl(w, o, 3)
			if name == "success" {
				if code != 3 || e != nil || w.calls != 1 {
					t.Fatal("outcome changed")
				}
				return
			}
			if code != 1 || e != errControlOutput {
				t.Fatal("output failure lost")
			}
			want := 1
			if name == "oversize" {
				want = 0
			}
			if w.calls != want {
				t.Fatal("retried or wrote before bound")
			}
		})
	}
}
func TestInstalledControlProjection(t *testing.T) {
	for _, name := range []string{"status", "proposal", "publish", "arm", "reprint", "negative", "lost", "cancel", "mismatch", "replay", "unknown-code"} {
		t.Run(name, func(t *testing.T) {
			op := localcontrol.InstalledIntentKind(name)
			if !controlOneOf(name, "status", "proposal", "publish", "arm", "reprint") {
				op = localcontrol.InstalledArm
			}
			i := localcontrol.InstalledIntent{Kind: op, ExpectedRun: controlTestRun, OperationID: "stable"}
			p := controlTestConsent().Proposal
			r := localcontrol.InstalledClientResult{Transmission: localcontrol.InstalledValidatedReply, Run: controlTestRun, Installation: "installation", Reply: localcontrol.InstalledReply{Code: "ok", Publication: "acknowledged", ArmReceipt: "acknowledged", Invitation: "SECRET", Status: &localcontrol.InstalledObservation{}, Proposal: &p}}
			if name == "proposal" {
				r.Reply.Publication = "not_dispatched"
			}
			var e error
			switch name {
			case "negative":
				r.Reply.Code = "admission_refused"
			case "lost":
				r.Transmission = localcontrol.InstalledPossibleDispatch
			case "cancel":
				e = io.ErrUnexpectedEOF
			case "mismatch":
				r.Run = strings.Repeat("a", 32)
			case "replay":
				r.Reply.Code = "consumed"
			case "unknown-code":
				r.Reply.Code = "secret-canary"
			}
			o, code := projectInstalledControl(i, "installation", r, e)
			secret := name == "arm" || name == "reprint"
			if (o.Invitation != "") == (!secret) {
				t.Fatal("invitation policy")
			}
			if secret && code != 0 {
				t.Fatal("success refused")
			}
			b, _ := json.Marshal(o)
			if strings.Contains(string(b), "secret-canary") {
				t.Fatal("raw code leaked")
			}
		})
	}
}

type controlNeverRead struct{ t *testing.T }

func (r controlNeverRead) Read([]byte) (int, error) {
	r.t.Fatal("closed gate consumed stdin")
	return 0, io.EOF
}
func TestInstalledControlClosedGate(t *testing.T) {
	root := installedCLICommandEnv(t)
	t.Chdir(root)
	t.Setenv("TMPDIR", root)
	t.Setenv("PATH", root)
	var out bytes.Buffer
	code, e := runInstalledControl([]string{"publish", "--run", controlTestRun}, controlNeverRead{t}, &out)
	if code != 1 || e != errControlRefused || out.Len() != 0 {
		t.Fatal("gate")
	}
}
func TestInstalledControlDispatchUsage(t *testing.T) {
	code, e := runCommand([]string{"installed-control", "arm"}, nil, nil)
	if code != 2 || e != errControlUsage {
		t.Fatal("dispatch")
	}
}
