package pibridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestInstanceMatchesBridgeIdentity(t *testing.T) {
	runtimes := map[string]string{}
	for _, name := range []string{"bun", "node"} {
		if path, err := exec.LookPath(name); err == nil {
			runtimes[name] = path
		}
	}
	if len(runtimes) == 0 {
		t.Skip("Bun or Node required for cross-runtime identity")
	}
	dir, err := os.MkdirTemp("/tmp", "pi-instance-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	real := filepath.Join(dir, "real")
	for _, directory := range []string{filepath.Join(real, "sub"), filepath.Join(dir, "elsewhere")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	socket := filepath.Join(real, "herdr.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for link, target := range map[string]string{
		filepath.Join(dir, "linked"):               "real",
		filepath.Join(real, "alias.sock"):          "herdr.sock",
		filepath.Join(dir, "chain.sock"):           filepath.Join(real, "alias.sock"),
		filepath.Join(dir, "elsewhere", "hop"):     "../real/sub",
		filepath.Join(dir, "elsewhere", "up.sock"): "../linked/sub/../alias.sock",
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		filepath.Join(dir, "loop.sock"):     "loop.sock",
		filepath.Join(dir, "dangling.sock"): "missing.sock",
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < 260; index++ {
		target := "real/herdr.sock"
		if index > 0 {
			target = fmt.Sprintf("link-%d", index-1)
		}
		if err := os.Symlink(target, filepath.Join(dir, fmt.Sprintf("link-%d", index))); err != nil {
			t.Fatal(err)
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, socket)
	if err != nil {
		t.Fatal(err)
	}
	canonicalDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{
		socket,
		dir + "/linked/herdr.sock",
		dir + "//linked/./herdr.sock",
		dir + "/elsewhere/hop/../herdr.sock",
		real + "/alias.sock",
		dir + "/chain.sock",
		dir + "/elsewhere/up.sock",
		dir + "/link-40",
		canonicalDir + "/link-254",
		relative,
	}
	want, err := Instance(socket)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if got, err := Instance(path); err != nil || got != want {
			t.Fatalf("Instance(%s) = %q, %v; want %q", path, got, err, want)
		}
	}
	invalid := []string{
		"", dir, dir + "/file", dir + "/missing.sock", dir + "/dangling.sock",
		dir + "/loop.sock", canonicalDir + "/link-255", dir + "/file/../real/herdr.sock",
		socket + "/../herdr.sock", socket + "/", socket + "/.",
	}
	for _, path := range invalid {
		if got, err := Instance(path); err == nil {
			t.Fatalf("Instance(%s) = %q; want an error", path, got)
		}
	}
	for name, runtime := range runtimes {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			args := append([]string{"../../tests/fixtures/pi-instance-identity.mjs"}, paths...)
			output, err := exec.CommandContext(ctx, runtime, append(args, invalid...)...).CombinedOutput()
			if err != nil {
				t.Fatalf("identity: %v\n%s", err, output)
			}
			var got []struct {
				Instance string `json:"instance"`
				Error    bool   `json:"error"`
			}
			if err := json.Unmarshal(output, &got); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(paths)+len(invalid) {
				t.Fatalf("identities = %+v", got)
			}
			for index, path := range paths {
				if got[index].Error || got[index].Instance != want {
					t.Fatalf("identity of %s = %+v, want %q", path, got[index], want)
				}
			}
			for index, path := range invalid {
				if !got[len(paths)+index].Error {
					t.Fatalf("identity of invalid path %s = %+v", path, got[len(paths)+index])
				}
			}
		})
	}
}
