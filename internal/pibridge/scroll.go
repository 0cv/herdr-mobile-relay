package pibridge

import (
	"context"
	"errors"
)

type ScrollResponse struct {
	Identity
	Challenge   string `json:"challenge"`
	Incarnation string `json:"incarnation"`
	Revision    string `json:"revision"`
	Renderer    string `json:"renderer"`
	Wheel       string `json:"wheel"`
	Reason      string `json:"reason"`
}

func QueryScrollState(ctx context.Context, identity Identity) (ScrollResponse, error) {
	return queryScrollState(ctx, Directory(identity.Instance), identity)
}

func queryScrollState(ctx context.Context, directory string, identity Identity) (ScrollResponse, error) {
	var result ScrollResponse
	data, err := queryBridge(ctx, directory, identity, "scroll_state", 4096)
	if err != nil {
		return result, err
	}
	if err := decodeBridge(data, &result); err != nil {
		return ScrollResponse{}, err
	}
	valid := result.Renderer == "fullscreen" && result.Wheel == "sgr_cell" && result.Reason == "pi_fullscreen" ||
		result.Renderer == "regular" && result.Wheel == "unavailable" && result.Reason == "pi_regular" ||
		result.Renderer == "unknown" && result.Wheel == "unavailable" && result.Reason == "ui_unavailable"
	if !valid {
		return ScrollResponse{}, errors.New("invalid Pi scroll state")
	}
	return result, nil
}
