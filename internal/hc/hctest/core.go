package hctest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func StartCore(t *testing.T, configure func(string, []string) []string) (socket string, env []string) {
	t.Helper()
	if os.Getenv("HERDR_RELAY_HC_INTEGRATION") != "1" {
		t.Skip("set HERDR_RELAY_HC_INTEGRATION=1 to run an isolated core")
	}
	_, err := exec.LookPath("hc")
	if err != nil {
		t.Skip("hc is not on PATH")
	}
	root, err := os.MkdirTemp("/tmp", "hcit-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	session := fmt.Sprintf("relayit%d", os.Getpid())
	env = []string{}
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "HC_") && !strings.HasPrefix(value, "XDG_") {
			env = append(env, value)
		}
	}
	env = append(env, "HC_SESSION="+session)
	for _, name := range []string{"STATE", "CONFIG", "DATA", "CACHE"} {
		env = append(env, "XDG_"+name+"_HOME="+filepath.Join(root, strings.ToLower(name)))
	}
	runtime := filepath.Join(root, "runtime")
	if err := os.MkdirAll(runtime, 0700); err != nil {
		t.Fatal(err)
	}
	env = append(env, "XDG_RUNTIME_DIR="+runtime, "HOME="+root)
	socket = filepath.Join(runtime, "herdr-core", session+".sock")
	relative, err := filepath.Rel(root, socket)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || filepath.IsAbs(relative) {
		t.Fatalf("socket outside scratch root: %s", socket)
	}
	t.Logf("scratch root: %s; socket: %s", root, socket)
	if configure != nil {
		env = configure(root, env)
	}
	run := func(args ...string) ([]byte, error) { return Run(env, args...) }
	t.Cleanup(func() {
		status, err := run("server", "status", "--json")
		t.Logf("isolated core status before stop: %s (error: %v)", status, err)
		if output, err := run("server", "stop"); err != nil {
			t.Errorf("stop isolated core: %v: %s", err, output)
		}
	})
	if output, err := run("server", "start"); err != nil {
		t.Fatalf("start isolated core: %v: %s", err, output)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socket); err == nil {
			return socket, env
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("core socket unavailable within 10s")
	return "", nil
}

func Run(env []string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "hc", args...)
	cmd.Env = env
	return cmd.CombinedOutput()
}
