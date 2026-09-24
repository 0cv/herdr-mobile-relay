package main

import (
	"errors"
	"io"

	"github.com/0cv/herdr-mobile-relay/internal/tailscale"
)

func runJSONField(args []string, stdin io.Reader, stdout io.Writer) (int, error) {
	if len(args) != 2 {
		return 2, errors.New("usage: json-field {bool|string|number} KEY")
	}
	switch args[0] {
	case "bool", "string", "number":
	default:
		return 2, errors.New("usage: json-field {bool|string|number} KEY")
	}

	data, err := io.ReadAll(io.LimitReader(stdin, int64(tailscale.MaxOutputBytes+1)))
	if err != nil {
		return 1, err
	}
	if len(data) > tailscale.MaxOutputBytes {
		return 1, tailscale.ErrOutputTooLong
	}
	value, err := tailscale.ExtractJSONField(data, args[1], args[0])
	if err != nil {
		return 1, err
	}
	if _, err := io.WriteString(stdout, value+"\n"); err != nil {
		return 1, err
	}
	return 0, nil
}
