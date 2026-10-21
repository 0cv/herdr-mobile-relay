// Package hermesprofiles reports the Hermes Agent profiles installed for the
// relay's user.
//
// Hermes keeps each named profile in its own directory under
// <hermes home>/profiles/<name>, and `hermes -p <name>` selects one for a
// process. The relay never resolves a profile's config itself: it only has to
// name one on the agent's command line, so the only thing it needs from disk is
// the set of names that exist. That is what this package answers, for the phone's
// launch form and for the start validation that rejects a typo before a pane is
// created.
package hermesprofiles

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

// Default is the profile Hermes uses when none is named. It is always offered,
// even on a host with no profiles directory, because it is not a directory under
// profiles/: it is the hermes home itself (hermes_cli resolves `-p default` to
// the root home without checking for profiles/default).
const Default = "default"

// NamePattern is the shape a profile id may take. It mirrors the pattern Hermes
// applies to its own `-p` value, so a name this package accepts can never be
// rejected by the CLI after the pane is already created.
var NamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Valid reports whether name can be handed to `hermes -p` at all.
func Valid(name string) bool {
	return NamePattern.MatchString(name)
}

// liveMarkers are the files `hermes profile create` leaves behind in a profile
// directory. Hermes' own named_profile_is_live accepts a profile that carries one
// of them and refuses one that does not, so a directory with none of them is a
// half-created or half-deleted profile that `-p` cannot select.
var liveMarkers = []string{"config.yaml", ".env", "SOUL.md", "profile.yaml", "auth.json", "state.db"}

// tombstoneDir is where `hermes profile delete` moves a deleted profile's
// directory. A name found under it is dead even if a fresh directory of the same
// name was created later without a marker.
const tombstoneDir = ".deleted"

// liveProfileDir reports whether profiles/<name> under root is a profile hermes
// would start. It mirrors named_profile_is_live: an identity marker must exist
// inside the profile directory and no tombstone may sit at .deleted/<name>.
//
// os.Stat, not entry.IsDir: a profile reached through a symlink reports
// ModeSymlink on the directory entry while hermes follows it.
func liveProfileDir(root, name string) bool {
	if _, err := os.Stat(filepath.Join(root, "profiles", tombstoneDir, name)); err == nil {
		return false
	}
	dir := filepath.Join(root, "profiles", name)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	for _, marker := range liveMarkers {
		markerPath := filepath.Join(dir, marker)
		if _, err := os.Stat(markerPath); err == nil {
			return true
		}
		// Hermes accepts is_file() or is_symlink(), so a marker that is a symlink
		// to a target that moved away (clone or migration leftover) is still an
		// identity claim. os.Stat followed the link and reported ENOENT; os.Lstat
		// sees the link itself.
		if _, err := os.Lstat(markerPath); err == nil {
			return true
		}
	}
	return false
}

// List reports the profiles a launch may select, Default first and the named
// profiles sorted after it, so the phone's picker is stable across reconnects.
//
// Every hermes home the relay can see is scanned: HERMES_HOME when the service
// sets it, then ~/.hermes. A host that relocated its hermes home would otherwise
// offer a picker of nothing but Default.
//
// Caveat: names are unioned across every root, but `hermes -p <name>` resolves
// against its own single root. A name that lives only in a secondary,
// history-only root is therefore listed and still refused by ValidateStart at
// start time; that trade-off is accepted, because hiding it would make a
// relocated home's profiles unselectable.
func List(home string) []string {
	names := []string{Default}
	seen := map[string]bool{Default: true}
	for _, root := range agentroots.HermesData(home) {
		entries, err := os.ReadDir(filepath.Join(root, "profiles"))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if seen[name] || !Valid(name) || name == Default {
				continue
			}
			if !liveProfileDir(root, name) {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names[1:])
	return names
}

// Exists reports whether name resolves to a live profile on this host. Default
// always does; a named profile has to be one hermes itself would start, so a typo
// or a deleted profile fails at validation instead of in a pane the user has to
// clean up.
func Exists(home, name string) bool {
	if name == Default {
		return true
	}
	if !Valid(name) {
		return false
	}
	for _, root := range agentroots.HermesData(home) {
		if liveProfileDir(root, name) {
			return true
		}
	}
	return false
}
