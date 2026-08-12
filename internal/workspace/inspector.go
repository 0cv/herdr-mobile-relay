package workspace

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	maxTreeEntries = 4000
	maxTextBytes   = 1024 * 1024
	maxImageBytes  = 5 * 1024 * 1024
	maxDiffBytes   = 1024 * 1024
	maxGitBytes    = 8 * 1024 * 1024
	gitTimeout     = 8 * time.Second
)

var ignoredDirectories = map[string]bool{
	".git": true, ".expo": true, ".next": true, ".turbo": true, ".vite": true,
	"Pods": true, "build": true, "coverage": true, "dist": true, "node_modules": true,
}

var gitSlots = make(chan struct{}, 4)

type Error struct {
	Message string
}

func (e *Error) Error() string { return e.Message }

func publicError(message string) error { return &Error{Message: message} }

type TreeEntry struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	Size int64  `json:"size,omitempty"`
}

type Tree struct {
	Root      string      `json:"root"`
	Entries   []TreeEntry `json:"entries"`
	Truncated bool        `json:"truncated,omitempty"`
}

type File struct {
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	Kind      string `json:"kind"`
	Text      string `json:"text,omitempty"`
	DataURL   string `json:"data_url,omitempty"`
	Size      int64  `json:"size"`
}

type GitFile struct {
	Path         string `json:"path"`
	OriginalPath string `json:"original_path,omitempty"`
	Status       string `json:"status"`
}

type GitStatus struct {
	Available bool      `json:"available"`
	Branch    string    `json:"branch,omitempty"`
	Files     []GitFile `json:"files"`
	Truncated bool      `json:"truncated,omitempty"`
}

type GitDiff struct {
	Path string `json:"path"`
	Diff string `json:"diff"`
}

type rootContext struct {
	path string
	root *os.Root
}

func openWorkspace(path string) (*rootContext, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, publicError("Workspace path is unavailable")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, publicError("Workspace is unavailable")
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return nil, publicError("Workspace is unavailable")
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, publicError("Workspace could not be opened")
	}
	return &rootContext{path: canonical, root: root}, nil
}

func (r *rootContext) Close() { _ = r.root.Close() }

func safePath(path string) (string, error) {
	path = filepath.ToSlash(strings.TrimSpace(path))
	if path == "" || path == "." || !fs.ValidPath(path) {
		return "", publicError("Invalid workspace-relative path")
	}
	return path, nil
}

func (r *rootContext) regularFile(path string) (*os.File, fs.FileInfo, error) {
	path, err := safePath(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := r.root.Lstat(path)
	if err != nil {
		return nil, nil, publicError("Workspace file was not found")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, nil, publicError("Workspace preview only supports regular files")
	}
	file, err := r.root.Open(path)
	if err != nil {
		return nil, nil, publicError("Workspace file could not be opened")
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, nil, publicError("Workspace file changed while it was opening")
	}
	return file, opened, nil
}

func TreeFor(workspace string) (Tree, error) {
	root, err := openWorkspace(workspace)
	if err != nil {
		return Tree{}, err
	}
	defer root.Close()

	entries := make([]TreeEntry, 0, 256)
	truncated := false
	err = fs.WalkDir(root.root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == "." {
				return walkErr
			}
			return nil
		}
		if path == "." {
			return nil
		}
		name := entry.Name()
		if entry.IsDir() && ignoredDirectories[name] {
			return fs.SkipDir
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if len(entries) >= maxTreeEntries {
			truncated = true
			return fs.SkipAll
		}
		kind := "file"
		var size int64
		if entry.IsDir() {
			kind = "directory"
		} else {
			info, infoErr := entry.Info()
			if infoErr != nil || !info.Mode().IsRegular() {
				return nil
			}
			size = info.Size()
		}
		entries = append(entries, TreeEntry{Path: filepath.ToSlash(path), Name: name, Kind: kind, Size: size})
		return nil
	})
	if err != nil {
		return Tree{}, publicError("Workspace tree could not be read")
	}
	sort.SliceStable(entries, func(i, j int) bool {
		leftParent, rightParent := filepath.Dir(entries[i].Path), filepath.Dir(entries[j].Path)
		if leftParent != rightParent {
			return entries[i].Path < entries[j].Path
		}
		if entries[i].Kind != entries[j].Kind {
			return entries[i].Kind == "directory"
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return Tree{Root: root.path, Entries: entries, Truncated: truncated}, nil
}

func ReadFile(workspace, path string) (File, error) {
	root, err := openWorkspace(workspace)
	if err != nil {
		return File{}, err
	}
	defer root.Close()
	path, err = safePath(path)
	if err != nil {
		return File{}, err
	}
	file, info, err := root.regularFile(path)
	if err != nil {
		return File{}, err
	}
	defer file.Close()

	extensionType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	limit := int64(maxTextBytes)
	kind := "text"
	if strings.HasPrefix(extensionType, "image/") && extensionType != "image/svg+xml" {
		limit = maxImageBytes
		kind = "image"
	}
	if info.Size() > limit {
		return File{}, publicError(fmt.Sprintf("Workspace file exceeds the %d MB preview limit", limit/(1024*1024)))
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return File{}, publicError("Workspace file could not be read within the preview limit")
	}
	mediaType := extensionType
	if mediaType == "" {
		mediaType = http.DetectContentType(data)
	}
	mediaType = strings.Split(mediaType, ";")[0]
	if kind == "image" {
		if !strings.HasPrefix(mediaType, "image/") {
			return File{}, publicError("Workspace image type is not supported")
		}
		return File{
			Path: path, MediaType: mediaType, Kind: kind, Size: info.Size(),
			DataURL: "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data),
		}, nil
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return File{}, publicError("Binary workspace files cannot be previewed")
	}
	return File{Path: path, MediaType: mediaType, Kind: kind, Text: string(data), Size: info.Size()}, nil
}

type boundedBuffer struct {
	buffer bytes.Buffer
	limit  int
	over   bool
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - b.buffer.Len()
	if remaining <= 0 {
		b.over = true
		return original, nil
	}
	if len(value) > remaining {
		value = value[:remaining]
		b.over = true
	}
	_, err := b.buffer.Write(value)
	return original, err
}

func gitEnvironment() []string {
	environment := make([]string, 0, len(os.Environ())+8)
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if strings.HasPrefix(key, "GIT_") {
			continue
		}
		environment = append(environment, value)
	}
	return append(environment,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"GIT_EXTERNAL_DIFF=",
		"LC_ALL=C",
	)
}

func runGit(parent context.Context, root string, limit int, args ...string) (string, int, error) {
	ctx, cancel := context.WithTimeout(parent, gitTimeout)
	defer cancel()
	select {
	case gitSlots <- struct{}{}:
		defer func() { <-gitSlots }()
	case <-ctx.Done():
		return "", -1, publicError("Git inspection timed out")
	}
	commandArgs := []string{
		"--literal-pathspecs",
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=/dev/null",
		"-c", "color.ui=false",
		"-c", "color.diff=false",
		"-c", "diff.ignoreSubmodules=all",
		"-c", "status.relativePaths=true",
		"-C", root,
	}
	commandArgs = append(commandArgs, args...)
	command := exec.CommandContext(ctx, "git", commandArgs...)
	command.Env = gitEnvironment()
	var output, stderr boundedBuffer
	output.limit = limit
	stderr.limit = 64 * 1024
	command.Stdout = &output
	command.Stderr = &stderr
	err := command.Run()
	if ctx.Err() != nil {
		return "", -1, publicError("Git inspection timed out")
	}
	if output.over || stderr.over {
		return "", -1, publicError("Git output exceeded the preview limit")
	}
	code := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			return "", -1, publicError("Git inspection could not start")
		}
		code = exitError.ExitCode()
	}
	return output.buffer.String(), code, nil
}

func GitStatusFor(ctx context.Context, workspace string) (GitStatus, error) {
	root, err := openWorkspace(workspace)
	if err != nil {
		return GitStatus{}, err
	}
	root.Close()
	output, code, err := runGit(ctx, root.path, maxGitBytes,
		"status", "--porcelain=v1", "--branch", "-z", "--untracked-files=all", "--ignore-submodules=all", "--", ".")
	if err != nil {
		return GitStatus{}, err
	}
	if code != 0 {
		return GitStatus{Available: false, Files: []GitFile{}}, nil
	}
	parts := strings.Split(output, "\x00")
	status := GitStatus{Available: true, Files: []GitFile{}}
	for index := 0; index < len(parts); index++ {
		part := parts[index]
		if part == "" {
			continue
		}
		if strings.HasPrefix(part, "## ") {
			branch := strings.TrimPrefix(part, "## ")
			branch = strings.TrimPrefix(branch, "No commits yet on ")
			branch = strings.TrimPrefix(branch, "Initial commit on ")
			if before, _, found := strings.Cut(branch, "..."); found {
				branch = before
			}
			status.Branch = branch
			continue
		}
		if len(part) < 4 {
			continue
		}
		entry := GitFile{Status: part[:2], Path: filepath.ToSlash(part[3:])}
		if strings.ContainsAny(entry.Status, "RC") && index+1 < len(parts) {
			index++
			entry.OriginalPath = filepath.ToSlash(parts[index])
		}
		if len(status.Files) >= 2000 {
			status.Truncated = true
			continue
		}
		status.Files = append(status.Files, entry)
	}
	return status, nil
}

func GitDiffFor(ctx context.Context, workspace, path string) (GitDiff, error) {
	root, err := openWorkspace(workspace)
	if err != nil {
		return GitDiff{}, err
	}
	defer root.Close()
	path, err = safePath(path)
	if err != nil {
		return GitDiff{}, err
	}

	status, err := GitStatusFor(ctx, root.path)
	if err != nil {
		return GitDiff{}, err
	}
	if !status.Available {
		return GitDiff{}, publicError("Workspace is not inside a Git repository")
	}
	var changed *GitFile
	for index := range status.Files {
		if status.Files[index].Path == path {
			changed = &status.Files[index]
			break
		}
	}
	if changed == nil {
		return GitDiff{}, publicError("Workspace file is no longer reported as changed")
	}
	untracked := changed.Status == "??"
	if untracked {
		file, _, fileErr := root.regularFile(path)
		if fileErr != nil {
			return GitDiff{}, fileErr
		}
		_ = file.Close()
	}
	if untracked {
		output, code, runErr := runGit(ctx, root.path, maxDiffBytes,
			"diff", "--no-index", "--no-ext-diff", "--no-textconv", "--", "/dev/null", path)
		if runErr != nil {
			return GitDiff{}, runErr
		}
		if code != 0 && code != 1 {
			return GitDiff{}, publicError("Git diff could not be read")
		}
		return GitDiff{Path: path, Diff: output}, nil
	}

	staged, stagedCode, err := runGit(ctx, root.path, maxDiffBytes,
		"diff", "--cached", "--no-ext-diff", "--no-textconv", "--no-renames", "--", path)
	if err != nil {
		return GitDiff{}, err
	}
	unstaged, unstagedCode, err := runGit(ctx, root.path, maxDiffBytes,
		"diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--", path)
	if err != nil {
		return GitDiff{}, err
	}
	if stagedCode != 0 || unstagedCode != 0 {
		return GitDiff{}, publicError("Git diff could not be read")
	}
	combined := staged
	if combined != "" && unstaged != "" {
		combined += "\n"
	}
	combined += unstaged
	if len(combined) > maxDiffBytes {
		return GitDiff{}, publicError("Git diff exceeded the preview limit")
	}
	return GitDiff{Path: path, Diff: combined}, nil
}
