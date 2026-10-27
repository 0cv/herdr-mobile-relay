package conversation

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"

	"github.com/0cv/herdr-mobile-relay/internal/agentroots"
)

const maxKimiSelectionPaths = 8192

type kimiSelectionFile struct {
	path         string
	identityOnly bool
	token        string
}

type kimiSelection struct {
	id       string
	location Location
	code     string
	files    []kimiSelectionFile
}

func kimiSelectionToken(path string, identityOnly bool) string {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Sprint(err)
	}
	return kimiStatToken(info, identityOnly)
}

func kimiStatToken(info os.FileInfo, identityOnly bool) string {
	token := fmt.Sprintf("%s:%s", fileIdentity(info), info.Mode())
	if !identityOnly {
		token += fmt.Sprintf(":%d:%d:%s", info.Size(), info.ModTime().UnixNano(), fileChangeToken(info))
	}
	return token
}

func (selection kimiSelection) current() bool {
	for _, file := range selection.files {
		if kimiSelectionToken(file.path, file.identityOnly) != file.token {
			return false
		}
	}
	return true
}

func (r *Reader) SelectKimiSession(project ProjectContext, reported string) (string, Location, string) {
	if reported != "" {
		return r.selectKimiSession(project, reported, func(string, bool) {})
	}
	roots := agentroots.KimiCodeHomes(r.home)
	if len(roots) > 64 {
		return "", Location{}, "output_limit"
	}
	encoded, _ := json.Marshal(struct {
		CWD   string
		Roots []string
	}{kimiCWD(project), roots})
	key := string(encoded)
	r.kimiSelectionMu.Lock()
	defer r.kimiSelectionMu.Unlock()
	if selection, ok := r.kimiSelections[key]; ok && selection.current() {
		return selection.id, selection.location, selection.code
	}
	selection := kimiSelection{}
	watch := func(path string, identityOnly bool) {
		if len(selection.files) <= maxKimiSelectionPaths {
			selection.files = append(selection.files, kimiSelectionFile{path, identityOnly, kimiSelectionToken(path, identityOnly)})
		}
	}
	selection.id, selection.location, selection.code = r.selectKimiSession(project, "", watch)
	if len(selection.files) > maxKimiSelectionPaths {
		return "", Location{}, "output_limit"
	}
	if !selection.current() {
		return "", Location{}, "source_unavailable"
	}
	if r.kimiSelections == nil {
		r.kimiSelections = make(map[string]kimiSelection)
	}
	if _, exists := r.kimiSelections[key]; !exists && len(r.kimiSelections) >= 8 {
		clear(r.kimiSelections)
	}
	r.kimiSelections[key] = selection
	return selection.id, selection.location, selection.code
}

func KimiSourceIdentity(location Location) string {
	if location.Path == "" || location.Root == "" {
		return ""
	}
	encoded, _ := json.Marshal([]string{location.Root, location.Path})
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func (r *Reader) KimiSelectionCurrent(scope BrowseScope) bool {
	if !scope.InferredSession || scope.SelectionReason == "ambiguous_session" {
		return true
	}
	id, location, code := r.SelectKimiSession(ProjectContext{CWD: scope.CWD, ForegroundCWD: scope.ForegroundCWD}, "")
	return id == scope.SessionID && code == scope.SelectionReason && KimiSourceIdentity(location) == scope.SelectionSource
}
