package update

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	relayrelease "github.com/0cv/herdr-mobile-relay/internal/release"
)

type s3Transport func(*http.Request) (*http.Response, error)

func (f s3Transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Snapshots include directories and absent paths, not just existing state bytes.
func s3Tree(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.Walk(root, func(p string, i os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		if i.IsDir() {
			result[p] = "directory"
			return nil
		}
		b, e := os.ReadFile(p)
		result[p] = string(b)
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func s3Available() State {
	return State{State: "available", Eligible: true, CanInstall: true, AvailableVersion: "1.2.4", AvailableRevision: nextTestRevision, TargetVersion: "1.2.4", TargetRevision: nextTestRevision}
}
func s3Manager(t *testing.T, root string, policy string) (*Manager, *int, *int) {
	t.Helper()
	m := NewManager(policy, filepath.Join(root, "release"), filepath.Join(root, "runtime"), testHerdrBinary(t), "1.2.3", currentTestRevision, "http://127.0.0.1/healthz")
	requests, launches := new(int), new(int)
	m.client.Transport = s3Transport(func(*http.Request) (*http.Response, error) {
		*requests++
		return nil, errors.New("injected network refusal")
	})
	m.launch = func(context.Context, string) error { *launches++; return errors.New("injected launch refusal") }
	m.tokenFile = "" // never consult an inherited token file in a test
	m.apiBase = "https://metadata.example.test"
	return m, requests, launches
}
func s3AssertBlocked(t *testing.T, state State) {
	t.Helper()
	if state.CanInstall || state.Eligible || state.Mode != "foreground" || !strings.Contains(state.Reason, "manual") {
		t.Errorf("stale admission: %#v", state)
	}
}
func s3Seed(t *testing.T, root string, state State) {
	t.Helper()
	if err := writeState(filepath.Join(root, "runtime", "update-state.json"), state); err != nil {
		t.Fatal(err)
	}
}
func TestExternalTailscaleBlocksPhoneManagedUpdates(t *testing.T) {
	for _, resolved := range []string{"tailscale-external", "cloudflare"} {
		for _, current := range []string{"", "tailscale-external", "cloudflare"} {
			err := transportAdmission(resolved, current, false)
			if resolved == "tailscale-external" || current == "tailscale-external" {
				if err == nil || !strings.Contains(err.Error(), "foreground Tailscale Serve") {
					t.Errorf("transportAdmission(%q, %q) = %v, want foreground refusal", resolved, current, err)
				}
			}
		}
	}
}

func TestTailscaleS3State(t *testing.T) {
	for _, raw := range []string{"tailscale", "", "cloudflare", "gateway", "unknown"} {
		for _, phase := range []string{"available", "installing", "restarting", "memory"} {
			t.Run(raw+"/"+phase, func(t *testing.T) {
				t.Setenv("HERDR_RELAY_TRANSPORT", raw)
				root := t.TempDir()
				state := s3Available()
				if phase != "memory" {
					state.State = phase
				}
				if phase == "restarting" {
					state.TargetVersion = "1.2.3"
					state.TargetRevision = currentTestRevision
				}
				if phase != "memory" {
					s3Seed(t, root, state)
				}
				before := s3Tree(t, root)
				m, requests, launches := s3Manager(t, root, "tailscale")
				if phase == "memory" {
					m.state = state
				}
				for _, got := range []State{m.State(), m.Check(context.Background()), m.State()} {
					s3AssertBlocked(t, got)
					if got.AvailableVersion != state.AvailableVersion || got.TargetRevision != state.TargetRevision {
						t.Error("lost release metadata")
					}
					if phase == "available" || phase == "memory" {
						if got.State != "blocked" {
							t.Error("available state not blocked")
						}
					} else if got.State != phase {
						t.Error("projection claimed foreign worker changed state")
					}
				}
				if !reflect.DeepEqual(before, s3Tree(t, root)) {
					t.Error("State/Check/constructor mutated existing tree")
				}
				if *requests != 0 || *launches != 0 {
					t.Error("unexpected boundary call")
				}
			})
		}
	}
	t.Run("executable-disappears", func(t *testing.T) {
		t.Setenv("HERDR_RELAY_TRANSPORT", "cloudflare")
		root := t.TempDir()
		s3Seed(t, root, s3Available())
		m, _, _ := s3Manager(t, root, "cloudflare")
		if !m.State().CanInstall {
			t.Fatal("positive eligibility missing")
		}
		if err := os.Remove(m.herdrBin); err != nil {
			t.Fatal(err)
		}
		state := m.State()
		if state.CanInstall || state.Eligible || !strings.Contains(state.Reason, "unavailable") {
			t.Fatalf("stale executable eligibility: %#v", state)
		}
	})
}
func TestTailscaleS3Schedule(t *testing.T) {
	for _, policy := range []string{"tailscale", "cloudflare", "", "unknown"} {
		for _, metadata := range []string{"cached", "missing", "mismatched"} {
			for _, app := range []bool{false, true} {
				t.Run(policy+"/"+metadata+map[bool]string{true: "/app", false: "/relay"}[app], func(t *testing.T) {
					t.Setenv("HERDR_RELAY_TRANSPORT", "tailscale")
					root := t.TempDir()
					s3Seed(t, root, s3Available())
					m, requests, launches := s3Manager(t, root, policy)
					if metadata == "cached" {
						m.metadata = releaseMetadata{Version: "1.2.4", Revision: nextTestRevision}
					} else if metadata == "mismatched" {
						m.metadata = releaseMetadata{Version: "1.2.5", Revision: currentTestRevision}
					}
					before := s3Tree(t, root)
					origin := ""
					if app {
						origin = "https://app.example.test"
					}
					id, state, err := m.Schedule(context.Background(), "1.2.4", nextTestRevision, app, origin)
					if err == nil || id != "" {
						t.Errorf("accepted schedule: id=%q err=%v", id, err)
					}
					s3AssertBlocked(t, state)
					s3AssertBlocked(t, m.State())
					if *requests != 0 || *launches != 0 {
						t.Errorf("side effects: requests=%d launches=%d", *requests, *launches)
					}
					if !reflect.DeepEqual(before, s3Tree(t, root)) {
						t.Error("Schedule mutated tree")
					}
				})
			}
		}
	}
	for _, change := range []string{"before-schedule", "during-lookup", "removed-executable", "absent-runtime", "check-completion"} {
		t.Run(change, func(t *testing.T) {
			t.Setenv("HERDR_RELAY_TRANSPORT", "cloudflare")
			root := t.TempDir()
			if change != "absent-runtime" {
				s3Seed(t, root, s3Available())
			}
			m, requests, launches := s3Manager(t, root, "cloudflare")
			m.state = s3Available()
			if change != "absent-runtime" && !m.State().CanInstall {
				t.Fatal("positive control not eligible")
			}
			var atVeto map[string]string
			switch change {
			case "before-schedule", "absent-runtime":
				t.Setenv("HERDR_RELAY_TRANSPORT", "tailscale")
			case "removed-executable":
				if err := os.Remove(m.herdrBin); err != nil {
					t.Fatal(err)
				}
			case "during-lookup", "check-completion":
				m.client.Transport = s3Transport(func(r *http.Request) (*http.Response, error) {
					*requests++
					t.Setenv("HERDR_RELAY_TRANSPORT", "tailscale")
					atVeto = s3Tree(t, root)
					return s3Metadata(r), nil
				})
			}
			before := s3Tree(t, root)
			if change == "check-completion" {
				state := m.Check(context.Background())
				s3AssertBlocked(t, state)
				saved, err := readState(m.statePath())
				if err != nil {
					t.Fatal(err)
				}
				if saved.CanInstall {
					t.Error("check republished installable result")
				}
				if atVeto == nil || !reflect.DeepEqual(atVeto, s3Tree(t, root)) {
					t.Error("Check completion mutated tree after policy veto")
				}
			} else {
				id, state, err := m.Schedule(context.Background(), "1.2.4", nextTestRevision, false, "")
				if err == nil || id != "" || state.CanInstall || state.Eligible {
					t.Errorf("changed eligibility admitted: %q %#v %v", id, state, err)
				}
				if !reflect.DeepEqual(before, s3Tree(t, root)) {
					t.Error("refusal mutated tree")
				}
			}
			if *launches != 0 {
				t.Error("launched after veto")
			}
			if change != "during-lookup" && change != "check-completion" && *requests != 0 {
				t.Error("request before admission")
			}
		})
	}
}
func s3Metadata(r *http.Request) *http.Response {
	body := `{"tag_name":"v1.2.4"}`
	if strings.Contains(r.URL.Path, "/git/ref/") {
		body = `{"object":{"type":"commit","sha":"` + nextTestRevision + `"}}`
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}
}
func s3Job(t *testing.T, root, policy string) Job {
	t.Helper()
	return Job{Transport: policy, ReleaseRoot: filepath.Join(root, "release"), HerdrBin: testHerdrBinary(t), TargetVersion: "1.2.4", TargetRevision: nextTestRevision, StatePath: filepath.Join(root, "runtime", "update-state.json"), HealthURL: "http://127.0.0.1/healthz", DeployAppFirst: true, ExpectedAppOrigin: "https://app.example.test"}
}
func s3RefusingWorker(calls *int) Worker {
	return Worker{Prepare: func(context.Context, Job) (stagedRelease, error) {
		*calls++
		return stagedRelease{}, errors.New("injected prepare refusal")
	}, Deploy: func(context.Context, Job, stagedRelease) error {
		*calls++
		return errors.New("injected deploy refusal")
	}, Install: func(context.Context, Job) error { *calls++; return errors.New("injected install refusal") }, Verify: func(context.Context, string, relayrelease.Manifest) error {
		*calls++
		return errors.New("injected verify refusal")
	}}
}
func TestTailscaleS3Worker(t *testing.T) {
	for _, policy := range []string{"tailscale", "cloudflare", "gateway", "", "unknown"} {
		for _, raw := range []string{"tailscale", "cloudflare", "gateway", "", "unknown"} {
			if policy == raw && (policy == "cloudflare" || policy == "gateway") {
				continue
			}
			t.Run(policy+"/"+raw, func(t *testing.T) {
				t.Setenv("HERDR_RELAY_TRANSPORT", raw)
				root := t.TempDir()
				job := s3Job(t, root, policy)
				if err := validateJob(job); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(root, "job.json")
				if err := writeJSONAtomic(path, job); err != nil {
					t.Fatal(err)
				}
				s3Seed(t, root, s3Available())
				before := s3Tree(t, root)
				calls := 0
				w := s3RefusingWorker(&calls)
				if err := w.Run(context.Background(), path); err == nil {
					t.Error("worker accepted disallowed policy")
				}
				if calls != 0 {
					t.Errorf("worker reached callbacks: %d", calls)
				}
				if !reflect.DeepEqual(before, s3Tree(t, root)) {
					t.Error("worker mutated tree before admission")
				}
			})
		}
	}
	t.Run("invalid-job-policy-refusal-read-only", func(t *testing.T) {
		t.Setenv("HERDR_RELAY_TRANSPORT", "tailscale")
		root := t.TempDir()
		job := s3Job(t, root, "cloudflare")
		job.TargetRevision = "bad"
		path := filepath.Join(root, "job.json")
		if err := writeJSONAtomic(path, job); err != nil {
			t.Fatal(err)
		}
		before := s3Tree(t, root)
		calls := 0
		w := s3RefusingWorker(&calls)
		if err := w.Run(context.Background(), path); err == nil {
			t.Error("accepted invalid policy")
		}
		if calls != 0 || !reflect.DeepEqual(before, s3Tree(t, root)) {
			t.Error("policy refusal persisted validation failure")
		}
	})
}
func TestTailscaleS3LegacyUpdates(t *testing.T) {
	for _, policy := range []string{"cloudflare", "gateway"} {
		for _, goos := range []string{"linux", "darwin"} {
			for _, app := range []bool{false, true} {
				t.Run(policy+"/"+goos+map[bool]string{true: "/app", false: "/relay"}[app], func(t *testing.T) {
					t.Setenv("HERDR_RELAY_TRANSPORT", "")
					root := t.TempDir()
					s3Seed(t, root, s3Available())
					m, requests, _ := s3Manager(t, root, policy)
					m.client.Transport = s3Transport(func(r *http.Request) (*http.Response, error) { *requests++; return s3Metadata(r), nil })
					var path string
					m.launch = func(_ context.Context, p string) error { path = p; return nil }
					origin := ""
					if app {
						origin = "https://app.example.test"
					}
					id, state, err := m.Schedule(context.Background(), "1.2.4", nextTestRevision, app, origin)
					if err != nil || id != filepath.Base(path) || state.State != "scheduled" {
						t.Fatalf("schedule: %q %#v %v", id, state, err)
					}
					job, err := loadJob(path)
					if err != nil {
						t.Fatal(err)
					}
					if job.Transport != policy || job.TargetVersion != "1.2.4" || job.TargetRevision != nextTestRevision || job.DeployAppFirst != app || job.ExpectedAppOrigin != origin {
						t.Fatalf("job: %#v", job)
					}
					// Use the actual argument generator with contradictory inherited transport.
					// Only the generated assignment is installed in this in-process worker fixture.
					launch := updateWorkerLaunch(goos, "fixture", filepath.Join(root, "never-executed"), path, m.transport, func(key string) (string, bool) {
						if key == "HERDR_RELAY_TRANSPORT" {
							return "tailscale", true
						}
						return "", false
					})
					generated := ""
					count := 0
					for _, arg := range launch.args {
						arg = strings.TrimPrefix(arg, "--setenv=")
						if strings.HasPrefix(arg, "HERDR_RELAY_TRANSPORT=") {
							generated = strings.TrimPrefix(arg, "HERDR_RELAY_TRANSPORT=")
							count++
						}
					}
					if generated != policy || count != 1 {
						t.Fatalf("policy propagation: %#v", launch)
					}
					t.Setenv("HERDR_RELAY_TRANSPORT", generated)
					var calls []string
					w := Worker{Prepare: func(_ context.Context, j Job) (stagedRelease, error) {
						calls = append(calls, "prepare")
						return stagedRelease{Root: t.TempDir(), Manifest: relayrelease.Manifest{Version: j.TargetVersion, Revision: j.TargetRevision}}, nil
					}, Deploy: func(context.Context, Job, stagedRelease) error { calls = append(calls, "deploy"); return nil }, Install: func(context.Context, Job) error { calls = append(calls, "install"); return nil }, Verify: func(_ context.Context, url string, manifest relayrelease.Manifest) error {
						if url != job.HealthURL || manifest.Revision != nextTestRevision {
							t.Error("incorrect verification identity")
						}
						calls = append(calls, "verify")
						return nil
					}}
					if err := w.Run(context.Background(), path); err != nil {
						t.Fatal(err)
					}
					want := []string{"prepare", "install", "verify"}
					if app {
						want = []string{"prepare", "deploy", "install", "verify"}
					}
					if !reflect.DeepEqual(calls, want) {
						t.Fatalf("callbacks=%v want=%v", calls, want)
					}
					saved, err := readState(job.StatePath)
					if err != nil || saved.State != "succeeded" || saved.CurrentRevision != nextTestRevision {
						t.Fatalf("completion: %#v %v", saved, err)
					}
					if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
						t.Error("completed job retained")
					}
				})
			}
		}
	}
	for _, invalid := range []string{"revision", "version", "executable", "advertised-revision"} {
		t.Run(invalid, func(t *testing.T) {
			t.Setenv("HERDR_RELAY_TRANSPORT", "cloudflare")
			root := t.TempDir()
			if invalid == "advertised-revision" {
				s3Seed(t, root, s3Available())
				m, requests, launches := s3Manager(t, root, "cloudflare")
				before := s3Tree(t, root)
				id, _, err := m.Schedule(context.Background(), "1.2.4", currentTestRevision, false, "")
				if err == nil || id != "" || *requests != 0 || *launches != 0 || !reflect.DeepEqual(before, s3Tree(t, root)) {
					t.Error("advertised mismatch admitted")
				}
				return
			}
			job := s3Job(t, root, "cloudflare")
			switch invalid {
			case "revision":
				job.TargetRevision = "bad"
			case "version":
				job.TargetVersion = "latest"
			case "executable":
				job.HerdrBin = filepath.Join(root, "missing")
			}
			path := filepath.Join(root, "job.json")
			if err := writeJSONAtomic(path, job); err != nil {
				t.Fatal(err)
			}
			calls := 0
			w := s3RefusingWorker(&calls)
			if err := w.Run(context.Background(), path); err == nil || calls != 0 {
				t.Errorf("invalid job admitted: %v calls=%d", err, calls)
			}
		})
	}
}
