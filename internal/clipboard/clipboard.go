package clipboard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
)

var ErrUnavailable = errors.New("clipboard tooling is unavailable")

type commandSpec struct {
	name      string
	readArgs  []string
	writeArgs []string
}

var commandSpecs = []commandSpec{
	{name: "wl-paste", readArgs: []string{"--no-newline"}, writeArgs: []string{}},
	{name: "xsel", readArgs: []string{"-b", "-o"}, writeArgs: []string{"-b", "-i"}},
	{name: "xclip", readArgs: []string{"-selection", "clipboard", "-o"}, writeArgs: []string{"-selection", "clipboard", "-i"}},
	{name: "pbpaste", readArgs: nil, writeArgs: nil},
}

var (
	probeMu       sync.Mutex
	probed        bool
	readerName    string
	readerCommand []string
	writerCommand []string
)

func Reader() (name string, read func(context.Context) ([]byte, error), ok bool) {
	probe()
	probeMu.Lock()
	defer probeMu.Unlock()
	if readerName == "" || len(readerCommand) == 0 || len(writerCommand) == 0 {
		return "", nil, false
	}
	name = readerName
	args := append([]string(nil), readerCommand[1:]...)
	binary := readerCommand[0]
	return name, func(ctx context.Context) ([]byte, error) {
		out, err := exec.CommandContext(ctx, binary, args...).Output()
		if err != nil {
			return nil, fmt.Errorf("read clipboard with %s: %w", name, err)
		}
		return out, nil
	}, true
}

func Write(ctx context.Context, data []byte) error {
	probe()
	probeMu.Lock()
	command := append([]string(nil), writerCommand...)
	probeMu.Unlock()
	if len(command) == 0 {
		return ErrUnavailable
	}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stdin = bytes.NewReader(data)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("write clipboard with %s: %w", command[0], err)
	}
	return nil
}

func probe() {
	probeMu.Lock()
	defer probeMu.Unlock()
	if probed {
		return
	}
	probed = true
	for _, spec := range commandSpecs {
		writerName := writerNameFor(spec.name)
		if writerName == "" {
			continue
		}
		readerPath, readerErr := exec.LookPath(spec.name)
		if readerErr != nil {
			continue
		}
		writerPath, writerErr := exec.LookPath(writerName)
		if writerErr != nil {
			continue
		}
		readerName = spec.name
		readerCommand = append([]string{readerPath}, spec.readArgs...)
		writerCommand = append([]string{writerPath}, writerArgsFor(spec.name)...)
		return
	}
}

func writerNameFor(name string) string {
	switch name {
	case "wl-paste":
		return "wl-copy"
	case "xsel":
		return "xsel"
	case "xclip":
		return "xclip"
	case "pbpaste":
		return "pbcopy"
	default:
		return ""
	}
}

func writerArgsFor(name string) []string {
	for _, spec := range commandSpecs {
		if spec.name == name {
			return spec.writeArgs
		}
	}
	return nil
}

func resetForTest() {
	probeMu.Lock()
	defer probeMu.Unlock()
	probed = false
	readerName = ""
	readerCommand = nil
	writerCommand = nil
}
