package fsutil

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type DirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type DirListing struct {
	Current struct {
		Path  string `json:"path"`
		Label string `json:"label"`
	} `json:"current"`
	Parent      string     `json:"parent"`
	Directories []DirEntry `json:"directories"`
}

func ListDirectories(path, home string) DirListing {
	home = filepath.Clean(home)
	if path == "" {
		path = home
	}

	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(home, path)
	}

	relative, err := filepath.Rel(home, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		path = home
		relative = "."
	}

	root, err := os.OpenRoot(home)
	if err != nil {
		return DirListing{}
	}
	defer root.Close()

	directory, err := root.Open(relative)
	if err != nil {
		path = home
		relative = "."
		directory, err = root.Open(".")
	}
	if err != nil {
		return DirListing{}
	}
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil || !info.IsDir() {
		return DirListing{}
	}

	var result DirListing
	result.Current.Path = path
	result.Current.Label = displayPath(path, home)

	if path != home {
		result.Parent = filepath.Dir(path)
	}

	entries, err := directory.ReadDir(-1)
	if err != nil {
		return result
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		childRelative := filepath.Join(relative, name)
		child, err := root.Open(childRelative)
		if err != nil {
			continue
		}
		childInfo, statErr := child.Stat()
		_ = child.Close()
		if statErr != nil || !childInfo.IsDir() {
			continue
		}
		result.Directories = append(result.Directories, DirEntry{
			Name: name,
			Path: filepath.Join(path, name),
		})
	}

	sort.Slice(result.Directories, func(i, j int) bool {
		return strings.ToLower(result.Directories[i].Name) < strings.ToLower(result.Directories[j].Name)
	})

	return result
}

func displayPath(path, home string) string {
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~/" + path[len(home)+1:]
	}
	return path
}
