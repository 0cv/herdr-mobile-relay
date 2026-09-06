package readiness

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
)

type State string

const (
	StateReady              State = "ready"
	StateAcknowledgedEmpty  State = "acknowledged_empty"
	StateUnexpectedEmpty    State = "unexpected_empty"
	StateInventoryMismatch  State = "inventory_mismatch"
	StateGenerationMismatch State = "generation_mismatch"
	StateInvalidManifest    State = "invalid_manifest"
	StateInvalidInventory   State = "invalid_inventory"
	StateUnavailable        State = "unavailable"
)

type Pane struct {
	PaneID          string `json:"pane_id"`
	NativeSessionID string `json:"native_session_id"`
	ProfileID       string `json:"profile_id"`
}

type manifest struct {
	Version           int    `json:"version"`
	Generation        string `json:"generation"`
	AcknowledgedEmpty bool   `json:"acknowledged_empty,omitempty"`
	Panes             []Pane `json:"panes"`
}

type Result struct {
	Ready      bool   `json:"ready"`
	State      State  `json:"state"`
	Generation string `json:"generation,omitempty"`
	Expected   int    `json:"expected"`
	Observed   int    `json:"observed"`
}

func Check(path, activeGeneration string, observed []Pane) Result {
	result := Result{State: StateUnavailable, Observed: len(observed)}
	info, err := os.Lstat(path)
	if err != nil {
		return result
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		result.State = StateInvalidManifest
		return result
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return result
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var expected manifest
	if decoder.Decode(&expected) != nil || trailingJSON(decoder) != nil || expected.Version != 1 {
		result.State = StateInvalidManifest
		return result
	}
	result.Generation = expected.Generation
	result.Expected = len(expected.Panes)
	if !validValue(expected.Generation) || expected.Generation != strings.TrimSpace(activeGeneration) {
		result.State = StateGenerationMismatch
		return result
	}
	expectedSet, valid := paneSet(expected.Panes)
	if !valid || expected.AcknowledgedEmpty && len(expected.Panes) != 0 {
		result.State = StateInvalidManifest
		return result
	}
	observedSet, valid := paneSet(observed)
	if !valid {
		result.State = StateInvalidInventory
		return result
	}
	if len(expectedSet) == 0 && len(observedSet) == 0 {
		if expected.AcknowledgedEmpty {
			result.Ready = true
			result.State = StateAcknowledgedEmpty
		} else {
			result.State = StateUnexpectedEmpty
		}
		return result
	}
	if len(observedSet) == 0 {
		result.State = StateUnexpectedEmpty
		return result
	}
	if !samePaneSet(expectedSet, observedSet) {
		result.State = StateInventoryMismatch
		return result
	}
	result.Ready = true
	result.State = StateReady
	return result
}

func paneSet(panes []Pane) (map[string]Pane, bool) {
	result := make(map[string]Pane, len(panes))
	nativeSessions := make(map[string]bool, len(panes))
	for _, pane := range panes {
		pane.PaneID = strings.TrimSpace(pane.PaneID)
		pane.NativeSessionID = strings.TrimSpace(pane.NativeSessionID)
		pane.ProfileID = strings.TrimSpace(pane.ProfileID)
		if !validValue(pane.PaneID) || !validValue(pane.NativeSessionID) || !validValue(pane.ProfileID) {
			return nil, false
		}
		if _, duplicate := result[pane.PaneID]; duplicate || nativeSessions[pane.NativeSessionID] {
			return nil, false
		}
		result[pane.PaneID] = pane
		nativeSessions[pane.NativeSessionID] = true
	}
	return result, true
}

func samePaneSet(expected, observed map[string]Pane) bool {
	if len(expected) != len(observed) {
		return false
	}
	for paneID, pane := range expected {
		if observed[paneID] != pane {
			return false
		}
	}
	return true
}

func validValue(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func trailingJSON(decoder *json.Decoder) error {
	var value any
	err := decoder.Decode(&value)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("trailing JSON")
	}
	return err
}
