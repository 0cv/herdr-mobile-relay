package pibridge

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/slashcmd"
)

func fmtPID(pid int) string { return strconv.Itoa(pid) }

func testEndpoint(t *testing.T, change func(*Response), raw string, delay time.Duration) (string, Identity) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pi-bridge-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	id := Identity{Instance: strings.Repeat("a", 64), Pane: "pane", Session: "session", PID: os.Getpid()}
	path := filepath.Join(dir, fmtPID(id.PID)+".sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var request struct {
			Identity
			Challenge string `json:"challenge"`
		}
		if json.NewDecoder(conn).Decode(&request) != nil {
			return
		}
		time.Sleep(delay)
		result := Response{Identity: request.Identity, Challenge: request.Challenge, Incarnation: strings.Repeat("b", 36), Revision: strings.Repeat("c", 64), Status: "available", Commands: []slashcmd.RuntimeCommand{{Command: slashcmd.Command{Command: "/orchestrate", Description: "Orchestration", Source: "personal"}, Metadata: slashcmd.Metadata{Kind: "extension", Provenance: &slashcmd.Provenance{Path: "/fixture/extension.ts", Source: "local", Scope: "user", Origin: "top-level"}}}}}
		if change != nil {
			change(&result)
		}
		if raw != "" {
			_, _ = conn.Write([]byte(raw))
			return
		}
		_ = json.NewEncoder(conn).Encode(result)
	}()
	return dir, id
}

func TestQuery(t *testing.T) {
	dir, id := testEndpoint(t, nil, "", 0)
	result, err := query(context.Background(), dir, id)
	if err != nil {
		t.Fatal(err)
	}
	if result.Commands[0].Command.Command != "/orchestrate" || result.Commands[0].Kind != "extension" {
		t.Fatalf("unexpected metadata: %+v", result)
	}
}

func TestProvenanceCredentialsNeverReachRelayCatalog(t *testing.T) {
	data, err := os.ReadFile("../../contracts/fixtures/pi_provenance_redaction.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Input           string `json:"input"`
		Expected        string `json:"expected"`
		Path            string `json:"path"`
		BaseDir         string `json:"base_dir"`
		ExpectedPath    string `json:"expected_path"`
		ExpectedBaseDir string `json:"expected_base_dir"`
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for index, fixture := range fixtures {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			dir, id := testEndpoint(t, func(response *Response) {
				response.Commands[0].Provenance.Source = fixture.Input
				if fixture.Path != "" {
					response.Commands[0].Provenance.Path = fixture.Path
				}
				response.Commands[0].Provenance.BaseDir = fixture.BaseDir
			}, "", 0)
			response, err := query(context.Background(), dir, id)
			if err != nil {
				t.Fatal(err)
			}
			catalog := slashcmd.MergePiRuntime(response.Commands, response.Status, response.Truncated)
			payload, err := json.Marshal(catalog)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(payload), "REDACTION_") {
				t.Fatal("synthetic authentication marker escaped into relay payload")
			}
			provenance := catalog.Metadata["/orchestrate"].Provenance
			if provenance.Source != fixture.Expected || (fixture.ExpectedPath != "" && provenance.Path != fixture.ExpectedPath) || (fixture.ExpectedBaseDir != "" && provenance.BaseDir != fixture.ExpectedBaseDir) {
				t.Fatal("incorrect nonsecret package provenance")
			}
		})
	}
}

func TestInstalledPiProvenanceCredentials(t *testing.T) {
	path := os.Getenv("PI_PROVENANCE_FIXTURE")
	if path == "" {
		t.Skip("run tests/pi-provenance-contract.mjs with PI_TEST_PACKAGE for installed-Pi metadata")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Entries  []slashcmd.RuntimeCommand      `json:"entries"`
		Expected map[string]slashcmd.Provenance `json:"expected"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	casesData, err := os.ReadFile("../../contracts/fixtures/pi_provenance_redaction.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		InstalledPi *struct {
			ID string `json:"id"`
		} `json:"installed_pi"`
	}
	if err := json.Unmarshal(casesData, &cases); err != nil {
		t.Fatal(err)
	}
	wanted := make(map[string]bool)
	for _, testCase := range cases {
		if testCase.InstalledPi == nil {
			continue
		}
		for _, scope := range []string{"user", "project", "temporary"} {
			name := "/private-" + testCase.InstalledPi.ID + "-" + scope
			p, ok := fixture.Expected[name]
			if !ok || p.Scope != scope || p.Source == "" || p.Path == "" || p.BaseDir == "" || p.Origin != "package" {
				t.Fatal("missing expected provenance for case and scope")
			}
			wanted[name] = true
		}
	}
	if len(wanted) == 0 || len(fixture.Entries) != len(wanted) || len(fixture.Expected) != len(wanted) {
		t.Fatal("incomplete installed-Pi case/scope coverage")
	}
	for _, entry := range fixture.Entries {
		if !wanted[entry.Command.Command] {
			t.Fatal("missing or duplicate installed-Pi command")
		}
		delete(wanted, entry.Command.Command)
		if entry.Provenance == nil {
			t.Fatal("missing Pi provenance")
		}
		for _, field := range []string{entry.Provenance.Source, entry.Provenance.Path, entry.Provenance.BaseDir} {
			if !strings.Contains(field, "REDACTION_") {
				t.Fatal("fixture did not exercise a credential-bearing Pi provenance field")
			}
		}
	}
	dir, id := testEndpoint(t, func(response *Response) { response.Commands = fixture.Entries }, "", 0)
	response, err := query(context.Background(), dir, id)
	if err != nil {
		t.Fatal(err)
	}
	catalog := slashcmd.MergePiRuntime(response.Commands, response.Status, response.Truncated)
	payload, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "REDACTION_") {
		t.Fatal("synthetic authentication escaped into relay payload")
	}
	if len(response.Commands) != len(fixture.Expected) {
		t.Fatal("sanitization removed commands")
	}
	for name, expected := range fixture.Expected {
		p := catalog.Metadata[name].Provenance
		if p == nil || *p != expected {
			t.Fatal("sanitization changed expected nonsecret package provenance")
		}
	}
}

func TestRejectInvalidResponses(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Response)
		raw    string
	}{
		{"pane", func(r *Response) { r.Pane = "another" }, ""},
		{"session", func(r *Response) { r.Session = "replaced" }, ""},
		{"instance", func(r *Response) { r.Instance = "another" }, ""},
		{"process", func(r *Response) { r.PID++ }, ""},
		{"challenge", func(r *Response) { r.Challenge = "stale" }, ""},
		{"status", func(r *Response) { r.Status = "unknown" }, ""},
		{"kind", func(r *Response) { r.Commands[0].Kind = "handler" }, ""},
		{"scope", func(r *Response) { r.Commands[0].Provenance.Scope = "elsewhere" }, ""},
		{"metadata", func(r *Response) { r.Commands[0].Description = strings.Repeat("x", 961) }, ""},
		{"count", func(r *Response) { r.Commands = make([]slashcmd.RuntimeCommand, 4097) }, ""},
		{"malformed", nil, "{"},
		{"oversize", nil, strings.Repeat("x", MaxBytes+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, id := testEndpoint(t, tt.change, tt.raw, 0)
			if _, err := query(context.Background(), dir, id); err == nil {
				t.Fatal("accepted invalid response")
			}
		})
	}
}

func TestRejectsForgedProcessRegistration(t *testing.T) {
	dir, id := testEndpoint(t, nil, "", 0)
	original := filepath.Join(dir, fmtPID(id.PID)+".sock")
	id.PID++
	if err := os.Rename(original, filepath.Join(dir, fmtPID(id.PID)+".sock")); err != nil {
		t.Fatal(err)
	}
	if _, err := query(context.Background(), dir, id); err == nil {
		t.Fatal("accepted a socket owned by another process")
	}
}

func TestRejectsEndpointReplacementDuringQuery(t *testing.T) {
	var dir string
	var id Identity
	replacements := make(chan net.Listener, 1)
	dir, id = testEndpoint(t, func(_ *Response) {
		path := filepath.Join(dir, fmtPID(id.PID)+".sock")
		_ = os.Remove(path)
		listener, err := net.Listen("unix", path)
		if err != nil {
			replacements <- nil
			return
		}
		_ = os.Chmod(path, 0600)
		replacements <- listener
	}, "", 0)
	if _, err := query(context.Background(), dir, id); err == nil {
		t.Error("accepted replaced endpoint")
	}
	if listener := <-replacements; listener != nil {
		_ = listener.Close()
	} else {
		t.Fatal("replacement fixture failed")
	}
}

func TestUnavailableTimeoutAndPermissions(t *testing.T) {
	if _, err := Query(context.Background(), Identity{}); err == nil {
		t.Fatal("accepted missing identity")
	}
	dir, id := testEndpoint(t, nil, "", 100*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := query(ctx, dir, id); err == nil {
		t.Fatal("expected timeout")
	}
	dir, id = testEndpoint(t, nil, "", 0)
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := query(context.Background(), dir, id); err == nil {
		t.Fatal("accepted public directory")
	}
	dir, id = testEndpoint(t, nil, "", 0)
	if err := os.Chmod(filepath.Join(dir, fmtPID(id.PID)+".sock"), 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := query(context.Background(), dir, id); err == nil {
		t.Fatal("accepted public socket")
	}
}
