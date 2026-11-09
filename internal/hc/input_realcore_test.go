package hc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/hc/hctest"
)

func TestRealCoreInput(t *testing.T) {
	var root string
	socket, _ := hctest.StartCore(t, func(r string, env []string) []string { root = r; return env })
	c := New(socket)
	ctx := t.Context()
	start := func(script string) string {
		raw, err := c.call(ctx, "terminal.run", map[string]any{"argv": []string{"/bin/sh", "-c", script}, "cwd": root, "no_parent": true})
		if err != nil {
			t.Fatal(err)
		}
		var result struct{ Address string }
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.StopPane(t.Context(), result.Address) })
		return result.Address
	}
	screen := func(id string) string {
		r, err := c.ReadPaneVisible(ctx, id, 0, "text")
		if err != nil {
			t.Fatal(err)
		}
		return string(r.Content)
	}
	wait := func(f func() bool) {
		deadline := time.Now().Add(10 * time.Second)
		for !f() {
			if time.Now().After(deadline) {
				t.Fatal("reader timed out")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	scan := func(marker string) bool {
		found := false
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.Contains(data, []byte(marker)) {
				found = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return found
	}
	const secret = "RelayPrivate43 é漢 Upper ! spaces"
	sum := sha256.Sum256([]byte(secret))
	script := fmt.Sprintf("stty -echo; printf 'Password:'; IFS= read -r value; got=$(printf '%%s' \"$value\" | shasum -a 256); case \"$got\" in %s*) printf '\\nACCEPTED\\n';; *) printf '\\nBAD\\n';; esac; IFS= read -r finish", hex.EncodeToString(sum[:]))
	id := start(script)
	wait(func() bool { return strings.Contains(screen(id), "Password:") })
	before, err := c.getTerminal(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SendText(ctx, id, secret); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(screen(id), "ACCEPTED") {
		t.Fatal("text submitted without Enter")
	}
	if scan(secret) {
		t.Fatal("secret persisted before submission")
	}
	if err := c.SendKeys(ctx, id, []string{"Enter"}); err != nil {
		t.Fatal(err)
	}
	wait(func() bool { return strings.Contains(screen(id), "ACCEPTED") })
	after, err := c.getTerminal(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if before.Cols != after.Cols || before.Rows != after.Rows {
		t.Fatal("size changed")
	}
	second := start(script)
	wait(func() bool { return strings.Contains(screen(second), "Password:") })
	if err := c.SendSecret(ctx, second, secret); err != nil {
		t.Fatal(err)
	}
	wait(func() bool { return strings.Contains(screen(second), "ACCEPTED") })
	if scan(secret) {
		t.Fatal("secret persisted while live")
	}
	for _, addr := range []string{id, second} {
		if err := c.SendKeys(ctx, addr, []string{"Enter"}); err != nil {
			t.Fatal(err)
		}
	}
	wait(func() bool {
		panes, err := c.PaneList(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(panes) == 0
	})
	if scan(secret) {
		t.Fatal("secret persisted after finish")
	}
	const echoed = "RelayEchoControl43"
	echo := start("printf 'Ready:'; IFS= read -r value; printf '%s\\n' \"$value\"")
	wait(func() bool { return strings.Contains(screen(echo), "Ready:") })
	if err := c.SendText(ctx, echo, echoed); err != nil {
		t.Fatal(err)
	}
	if err := c.SendKeys(ctx, echo, []string{"Enter"}); err != nil {
		t.Fatal(err)
	}
	wait(func() bool { return scan(echoed) })
}
