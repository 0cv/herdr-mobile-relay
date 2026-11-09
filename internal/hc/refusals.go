package hc

import (
	"context"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func unsupported() error {
	return &herdr.CLIError{Code: "unsupported_method", Message: "Not available on core"}
}

func (c *Client) PaneRun(context.Context, string, []string) error { return unsupported() }

func (c *Client) StartAgent(context.Context, string, string, string, int, ...string) (string, error) {
	return "", unsupported()
}

func (c *Client) RenameAgent(context.Context, string, string) error { return unsupported() }

func (c *Client) AgentGet(context.Context, string) (*herdr.AgentInfo, error) {
	return nil, unsupported()
}

func (c *Client) TabCreate(context.Context, string, string, string) (*herdr.CreateResult, error) {
	return nil, unsupported()
}

func (c *Client) WorkspaceCreate(context.Context, string, string) (*herdr.CreateResult, error) {
	return nil, unsupported()
}

func (c *Client) WorkspaceClose(context.Context, string, bool) error { return unsupported() }

func (c *Client) WorktreeList(context.Context, string) (*herdr.WorktreeListResult, error) {
	return nil, unsupported()
}

func (c *Client) WorktreeCreate(context.Context, string, string, string, string) (*herdr.WorktreeMutationResult, error) {
	return nil, unsupported()
}

func (c *Client) WorktreeOpen(context.Context, string, string, string, string) (*herdr.WorktreeMutationResult, error) {
	return nil, unsupported()
}

func (c *Client) WorktreeRemove(context.Context, string, bool) (*herdr.WorktreeRemoveResult, error) {
	return nil, unsupported()
}
