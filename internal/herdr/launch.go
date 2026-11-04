package herdr

import "context"

func (c *Client) LaunchTerminal(context.Context, []string, string, string, string, bool) (*Pane, bool, error) {
	return nil, false, &CLIError{Code: "unsupported_method"}
}
