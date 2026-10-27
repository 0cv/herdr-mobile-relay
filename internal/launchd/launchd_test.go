package launchd

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func testJob() Job {
	return Job{
		Label:   UpdateWorkerPrefix + "1",
		Program: []string{"/Users/a&b/<relay>", "update-worker", "/tmp/job \"1\".json"},
		Environment: map[string]string{
			"HERDR_RELAY_ENV":         "/Users/a&b/relay.env",
			"HERDR_PLUGIN_CONFIG_DIR": "/Users/a&b/<config>",
		},
	}
}

func TestPlistEscapesValuesAndRunsOnce(t *testing.T) {
	job := testJob()
	document := plist(job)
	if strings.Contains(string(document), "KeepAlive") {
		t.Fatalf("plist asks launchd to keep the worker alive:\n%s", document)
	}

	var texts []string
	decoder := xml.NewDecoder(strings.NewReader(string(document)))
	decoder.Strict = true
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("plist is not well-formed XML: %v\n%s", err, document)
		}
		if data, ok := token.(xml.CharData); ok && strings.TrimSpace(string(data)) != "" {
			texts = append(texts, string(data))
		}
	}
	want := []string{
		"Label", job.Label,
		"ProgramArguments", job.Program[0], job.Program[1], job.Program[2],
		"EnvironmentVariables",
		"HERDR_PLUGIN_CONFIG_DIR", "/Users/a&b/<config>",
		"HERDR_RELAY_ENV", "/Users/a&b/relay.env",
		"RunAtLoad",
	}
	if !slices.Equal(texts, want) {
		t.Fatalf("plist text = %q, want %q", texts, want)
	}
	if !strings.Contains(string(document), "<key>RunAtLoad</key>\n\t<true/>") {
		t.Fatalf("plist does not run the job at load:\n%s", document)
	}
}

func TestPlistParsesAsPropertyList(t *testing.T) {
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil is only available on macOS")
	}
	job := testJob()
	path := filepath.Join(t.TempDir(), "job.plist")
	if err := os.WriteFile(path, plist(job), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(plutil, "-convert", "json", "-o", "-", path).Output()
	if err != nil {
		t.Fatalf("plutil rejected the plist: %v", err)
	}
	var parsed struct {
		Label                string
		ProgramArguments     []string
		EnvironmentVariables map[string]string
		RunAtLoad            bool
		KeepAlive            any
	}
	if err := json.Unmarshal(output, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Label != job.Label ||
		!slices.Equal(parsed.ProgramArguments, job.Program) ||
		!reflect.DeepEqual(parsed.EnvironmentVariables, job.Environment) ||
		!parsed.RunAtLoad ||
		parsed.KeepAlive != nil {
		t.Fatalf("parsed plist = %#v", parsed)
	}
}

func TestPlistOmitsEmptyEnvironment(t *testing.T) {
	job := testJob()
	job.Environment = nil
	if strings.Contains(string(plist(job)), "EnvironmentVariables") {
		t.Fatal("plist declares an empty environment")
	}
}

func TestBootstrapLoadsPlistIntoGUIDomainAndRemovesIt(t *testing.T) {
	for _, bootstrapErr := range []error{nil, errors.New("exit status 5")} {
		dir := filepath.Join(t.TempDir(), "runtime")
		job := testJob()
		path := filepath.Join(dir, job.Label+".plist")
		var calls [][]string
		run := func(_ context.Context, args ...string) ([]byte, error) {
			calls = append(calls, args)
			if args[0] != "bootstrap" {
				return nil, nil
			}
			loaded, err := os.ReadFile(args[2])
			if err != nil || string(loaded) != string(plist(job)) {
				t.Fatalf("bootstrap read %q, %v", loaded, err)
			}
			if bootstrapErr != nil {
				return []byte("Bootstrap failed: 5: Input/output error\n"), bootstrapErr
			}
			return nil, nil
		}

		err := bootstrap(t.Context(), run, dir, 501, job)
		if bootstrapErr == nil && err != nil {
			t.Fatal(err)
		}
		if bootstrapErr != nil && (err == nil || !strings.Contains(err.Error(), "Bootstrap failed: 5")) {
			t.Fatalf("bootstrap error = %v", err)
		}
		want := [][]string{{"print", "gui/501"}, {"bootstrap", "gui/501", path}}
		if !reflect.DeepEqual(calls, want) {
			t.Fatalf("launchctl calls = %q, want %q", calls, want)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("plist left behind after bootstrap error %v: %v", bootstrapErr, err)
		}
	}
}

func TestBootstrapFallsBackToUserDomainWithoutGUISession(t *testing.T) {
	dir := t.TempDir()
	job := testJob()
	var calls [][]string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		if args[0] == "print" {
			return []byte("Bad request.\n"), errors.New("exit status 113")
		}
		return nil, nil
	}
	if err := bootstrap(t.Context(), run, dir, 501, job); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"print", "gui/501"},
		{"bootstrap", "user/501", filepath.Join(dir, job.Label+".plist")},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("launchctl calls = %q, want %q", calls, want)
	}
}

const listOutput = "PID\tStatus\tLabel\n" +
	"767\t0\tcom.apple.trustd.agent\n" +
	"22409\t-15\tcom.herdr-mobile-relay.service\n" +
	"-\t0\tcom.herdr-mobile-relay.service.backup\n" +
	"-\t1\therdr-mobile-relay-app-deploy-1790792801\n" +
	"-\t127\therdr-mobile-relay-update-1790406352\n" +
	"4242\t0\therdr-mobile-relay-update-1790406999000000000\n" +
	"-\t0\therdr-mobile-relay-updater\n" +
	"-\t0\tcom.example.herdr-mobile-relay-update-1\n" +
	"-\t3\therdr-mobile-relay-update-1790407000000000000\r\n"

func TestStaleWorkersSkipsRunningAndUnrelatedJobs(t *testing.T) {
	got := staleWorkers(listOutput, UpdateWorkerPrefix, AppDeployWorkerPrefix)
	want := []string{
		"herdr-mobile-relay-app-deploy-1790792801",
		"herdr-mobile-relay-update-1790406352",
		"herdr-mobile-relay-update-1790407000000000000",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("stale workers = %q, want %q", got, want)
	}
	if got := staleWorkers(listOutput, AppDeployWorkerPrefix); !slices.Equal(got, want[:1]) {
		t.Fatalf("app-deploy stale workers = %q", got)
	}
	if got := staleWorkers("", UpdateWorkerPrefix); len(got) != 0 {
		t.Fatalf("empty list produced %q", got)
	}
}

func TestSweepRemovesEveryStaleWorker(t *testing.T) {
	var removed []string
	run := func(_ context.Context, args ...string) ([]byte, error) {
		switch args[0] {
		case "list":
			return []byte(listOutput), nil
		case "remove":
			removed = append(removed, args[1])
			if args[1] == "herdr-mobile-relay-app-deploy-1790792801" {
				return []byte("Could not find specified service\n"), errors.New("exit status 113")
			}
			return nil, nil
		}
		t.Fatalf("unexpected launchctl call %q", args)
		return nil, nil
	}
	err := sweep(t.Context(), run, UpdateWorkerPrefix, AppDeployWorkerPrefix)
	if err == nil || !strings.Contains(err.Error(), "herdr-mobile-relay-app-deploy-1790792801") {
		t.Fatalf("sweep error = %v", err)
	}
	want := []string{
		"herdr-mobile-relay-app-deploy-1790792801",
		"herdr-mobile-relay-update-1790406352",
		"herdr-mobile-relay-update-1790407000000000000",
	}
	if !slices.Equal(removed, want) {
		t.Fatalf("removed = %q, want %q", removed, want)
	}
}

func TestSweepReportsListFailure(t *testing.T) {
	run := func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] != "list" {
			t.Fatalf("launchctl %q ran after list failed", args)
		}
		return nil, errors.New("exit status 1")
	}
	if err := sweep(t.Context(), run, UpdateWorkerPrefix); err == nil {
		t.Fatal("list failure was not reported")
	}
}
