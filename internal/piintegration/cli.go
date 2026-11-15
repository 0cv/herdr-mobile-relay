package piintegration

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
)

var ErrUsage = errors.New("usage")

// Run is the relay's pi-commands command, which relay/pi-commands.sh and
// relay setup call to install or remove the integration in one Pi agent directory.
func Run(bundle fs.FS, args []string, out io.Writer) error {
	if len(args) != 2 || (args[0] != "install" && args[0] != "remove") {
		return fmt.Errorf("%w: herdr-mobile-relay pi-commands install|remove AGENT_DIRECTORY", ErrUsage)
	}
	action, dir := args[0], args[1]
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%w: Pi agent directory must be absolute", ErrUsage)
	}
	profile := New(bundle, func() []string { return []string{dir} }).profile(dir, action)
	if profile.Status == "refused" {
		return errors.New(profile.Reason)
	}
	target := filepath.Join(dir, "extensions", Directory)
	switch profile.Status {
	case "installed", "up to date":
		fmt.Fprintf(out, "Relay command metadata integration is %s in %s\n", profile.Status, target)
	case "removed":
		fmt.Fprintf(out, "Removed relay-owned command integration files from %s\n", target)
	default:
		return nil
	}
	fmt.Fprintln(out, "Run /reload in each affected Pi pane, or restart Pi. Other integrations and settings were not changed.")
	return nil
}
