package agentsession

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/0cv/herdr-mobile-relay/internal/atomicfile"
)

func ompExtension(binary string) []byte {
	return []byte(fmt.Sprintf(`import { spawn } from "node:child_process";
export default pi => {
  for (const event of ["session_start", "session_switch"]) pi.on(event, (_, ctx) => {
    try {
      if (!ctx.hasUI || process.env.OMPCODE === "1" || !process.env.HC_TERMINAL_SOCKET) return;
      const session_id = ctx.sessionManager.getSessionFile() || ctx.sessionManager.getSessionId();
      const child = spawn(%s, ["agent-session", "omp"], { stdio: ["pipe", "ignore", "ignore"] });
      child.on("error", () => {});
      child.stdin.on("error", () => {});
      child.stdin.end(JSON.stringify({ session_id }));
    } catch {}
  });
};
`, raw(binary)))
}

func manageOmpExtension(home, binary, operation string, output io.Writer) error {
	dir := filepath.Join(home, ".omp", "agent")
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		fmt.Fprintln(output, "omp: config absent")
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s: not a directory", dir)
	}
	dir = filepath.Join(dir, "extensions")
	path := filepath.Join(dir, "herdr-mobile-relay-session.ts")
	_, err = os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	state := "not installed"
	if err == nil {
		state = "installed"
	}
	switch operation {
	case "install":
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		if _, err := atomicfile.Replace(dir, path, ".agent-hooks-*", ompExtension(binary), 0600); err != nil {
			return err
		}
		state = "installed"
	case "uninstall":
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		state = "not installed"
	}
	fmt.Fprintf(output, "omp: %s\n", state)
	return nil
}
