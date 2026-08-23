package slashcmd

import (
	"os"
	"path/filepath"
)

// findProjectDirs returns every existing <ancestor>/<stem> directory between the
// git root and cwd, outermost first, and in stem order within one ancestor.
// Without a git root only cwd is examined. Directories are de-duplicated.
//
// os.Stat is used rather than DirEntry.IsDir so a symlinked directory is
// followed; symlinking an agent config tree out of a dotfiles repo is common.
func findProjectDirs(cwd string, stems []string) []string {
	if cwd == "" || len(stems) == 0 {
		return nil
	}
	var dirs []string
	seen := make(map[string]bool)

	appendStems := func(dir string, out *[]string) {
		for _, stem := range stems {
			candidate := filepath.Join(dir, stem)
			if seen[candidate] {
				continue
			}
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				*out = append(*out, candidate)
				seen[candidate] = true
			}
		}
	}

	gitRoot := findGitRoot(cwd)
	if gitRoot == "" {
		appendStems(cwd, &dirs)
		return dirs
	}

	appendStems(gitRoot, &dirs)

	// Walk from cwd upward to the git root, then reverse so the outermost scope
	// comes first (git root direction).
	var chain [][]string
	dir := cwd
	for range maxGitWalkDepth {
		var level []string
		appendStems(dir, &level)
		if len(level) > 0 {
			chain = append(chain, level)
		}
		if dir == gitRoot {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	for i := len(chain) - 1; i >= 0; i-- {
		dirs = append(dirs, chain[i]...)
	}
	return dirs
}
