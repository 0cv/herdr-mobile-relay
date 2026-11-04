package hc

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

func (c *Client) TabRename(ctx context.Context, tabID, name string) error {
	return c.editTab(ctx, tabID, func(tabs []map[string]json.RawMessage, index int) error {
		tabs[index]["name"], _ = json.Marshal(name)
		return nil
	})
}

func (c *Client) TabMove(ctx context.Context, tabID string, insertIndex int) error {
	return c.editTab(ctx, tabID, func(tabs []map[string]json.RawMessage, index int) error {
		return moveLayoutItem(tabs, index, insertIndex)
	})
}

func (c *Client) editTab(ctx context.Context, tabID string, mutate func([]map[string]json.RawMessage, int) error) error {
	if !localLayoutID(tabID) {
		return invalidLayout()
	}
	return c.editLayout(ctx, func(spaces []map[string]json.RawMessage) error {
		for _, space := range spaces {
			var tabs []map[string]json.RawMessage
			json.Unmarshal(space["tabs"], &tabs)
			for index, tab := range tabs {
				if layoutID(tab) != tabID {
					continue
				}
				if !localLayoutID(layoutID(space)) {
					return invalidLayout()
				}
				if err := mutate(tabs, index); err != nil {
					return err
				}
				space["tabs"], _ = json.Marshal(tabs)
				return nil
			}
		}
		return invalidLayout()
	})
}

func (c *Client) WorkspaceRename(ctx context.Context, workspaceID, name string) error {
	return c.editSpace(ctx, workspaceID, func(spaces []map[string]json.RawMessage, index int) error {
		spaces[index]["name"], _ = json.Marshal(name)
		return nil
	})
}

func (c *Client) WorkspaceMove(ctx context.Context, workspaceID string, insertIndex int) error {
	return c.editSpace(ctx, workspaceID, func(spaces []map[string]json.RawMessage, index int) error {
		return moveLayoutItem(spaces, index, insertIndex)
	})
}

func (c *Client) WorkspaceMoveBlock(ctx context.Context, workspaceIDs []string, beforeWorkspaceID string) error {
	selected := make(map[string]bool, len(workspaceIDs))
	for _, id := range workspaceIDs {
		if !localLayoutID(id) || selected[id] {
			return invalidLayout()
		}
		selected[id] = true
	}
	if len(selected) == 0 || beforeWorkspaceID != "" && (!localLayoutID(beforeWorkspaceID) || selected[beforeWorkspaceID]) {
		return invalidLayout()
	}
	return c.editLayout(ctx, func(spaces []map[string]json.RawMessage) error {
		block := make([]map[string]json.RawMessage, 0, len(selected))
		remaining := make([]map[string]json.RawMessage, 0, len(spaces))
		for _, space := range spaces {
			if selected[layoutID(space)] {
				block = append(block, space)
			} else {
				remaining = append(remaining, space)
			}
		}
		if len(block) != len(selected) {
			return invalidLayout()
		}
		index := len(remaining)
		if beforeWorkspaceID != "" {
			index = slices.IndexFunc(remaining, func(space map[string]json.RawMessage) bool {
				return layoutID(space) == beforeWorkspaceID
			})
			if index < 0 {
				return invalidLayout()
			}
		}
		copy(spaces, slices.Insert(remaining, index, block...))
		return nil
	})
}

func (c *Client) editSpace(ctx context.Context, id string, mutate func([]map[string]json.RawMessage, int) error) error {
	if !localLayoutID(id) {
		return invalidLayout()
	}
	return c.editLayout(ctx, func(spaces []map[string]json.RawMessage) error {
		index := slices.IndexFunc(spaces, func(space map[string]json.RawMessage) bool { return layoutID(space) == id })
		if index < 0 {
			return invalidLayout()
		}
		return mutate(spaces, index)
	})
}

func moveLayoutItem(items []map[string]json.RawMessage, index, insertIndex int) error {
	if insertIndex < 0 || insertIndex > len(items) {
		return &herdr.CLIError{Code: "invalid_params"}
	}
	if insertIndex > index {
		insertIndex--
	}
	item := items[index]
	if insertIndex < index {
		copy(items[insertIndex+1:index+1], items[insertIndex:index])
	} else {
		copy(items[index:insertIndex], items[index+1:insertIndex+1])
	}
	items[insertIndex] = item
	return nil
}

func invalidLayout() error {
	return &herdr.CLIError{Code: "invalid_params", Message: "Layout is missing or ambiguous"}
}

func localLayoutID(id string) bool {
	return id != "" && id != "core" && !strings.Contains(id, "/")
}

func layoutID(item map[string]json.RawMessage) string {
	var id string
	json.Unmarshal(item["id"], &id)
	return id
}

func (c *Client) editLayout(ctx context.Context, mutate func([]map[string]json.RawMessage) error) error {
	invalid := invalidLayout()
	raw, err := c.call(ctx, "doc.get", map[string]any{"name": "herdr-app"})
	if err != nil {
		return err
	}
	var doc struct {
		Value   map[string]json.RawMessage `json:"value"`
		Version *uint64                    `json:"version"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Value == nil || doc.Version == nil {
		return invalid
	}
	var spaces []map[string]json.RawMessage
	if json.Unmarshal(doc.Value["spaces"], &spaces) != nil {
		return invalid
	}
	spaceIDs, tabIDs := map[string]bool{}, map[string]bool{}
	for _, space := range spaces {
		var id string
		if json.Unmarshal(space["id"], &id) != nil || id == "" || spaceIDs[id] {
			return invalid
		}
		spaceIDs[id] = true
		var tabs []map[string]json.RawMessage
		if json.Unmarshal(space["tabs"], &tabs) != nil {
			return invalid
		}
		for _, tab := range tabs {
			var tid string
			if json.Unmarshal(tab["id"], &tid) != nil || tid == "" || tabIDs[tid] {
				return invalid
			}
			tabIDs[tid] = true
		}
	}
	if err := mutate(spaces); err != nil {
		return err
	}
	doc.Value["spaces"], err = json.Marshal(spaces)
	if err != nil {
		return err
	}
	value, err := json.Marshal(doc.Value)
	if err != nil {
		return err
	}
	if len(value) > 1<<20 {
		return &herdr.CLIError{Code: "invalid_params", Message: "Layout exceeds the 1 MiB limit"}
	}
	_, err = c.call(ctx, "doc.set", map[string]any{"name": "herdr-app", "value": json.RawMessage(value), "version": *doc.Version})
	var cliErr *herdr.CLIError
	if errors.As(err, &cliErr) && cliErr.Code == "conflict" {
		return &herdr.CLIError{Code: "layout_conflict", Message: herdr.RefusalMessage("layout_conflict")}
	}
	return err
}
