package hc

import (
	"context"
	"encoding/json"
	"errors"
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
		if insertIndex < 0 || insertIndex > len(tabs) {
			return &herdr.CLIError{Code: "invalid_params"}
		}
		if insertIndex > index {
			insertIndex--
		}
		tab := tabs[index]
		if insertIndex < index {
			copy(tabs[insertIndex+1:index+1], tabs[insertIndex:index])
		} else {
			copy(tabs[index:insertIndex], tabs[index+1:insertIndex+1])
		}
		tabs[insertIndex] = tab
		return nil
	})
}

func (c *Client) editTab(ctx context.Context, tabID string, mutate func([]map[string]json.RawMessage, int) error) error {
	invalid := &herdr.CLIError{Code: "invalid_params", Message: "Tab layout is missing or ambiguous"}
	if tabID == "" || tabID == "core" || strings.Contains(tabID, "/") {
		return invalid
	}
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
	targetSpace, targetTab := -1, -1
	var targetTabs []map[string]json.RawMessage
	for i, space := range spaces {
		var id string
		if json.Unmarshal(space["id"], &id) != nil || id == "" || spaceIDs[id] {
			return invalid
		}
		spaceIDs[id] = true
		var tabs []map[string]json.RawMessage
		if json.Unmarshal(space["tabs"], &tabs) != nil {
			return invalid
		}
		for j, tab := range tabs {
			var tid string
			if json.Unmarshal(tab["id"], &tid) != nil || tid == "" || tabIDs[tid] {
				return invalid
			}
			tabIDs[tid] = true
			if tid == tabID {
				if id == "core" || strings.Contains(id, "/") {
					return invalid
				}
				targetSpace, targetTab, targetTabs = i, j, tabs
			}
		}
	}
	if targetSpace < 0 {
		return invalid
	}
	if err := mutate(targetTabs, targetTab); err != nil {
		return err
	}
	spaces[targetSpace]["tabs"], err = json.Marshal(targetTabs)
	if err != nil {
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
