package support

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type Snapshot struct {
	GeneratedAt      string            `json:"generated_at"`
	Version          string            `json:"version"`
	Revision         string            `json:"revision"`
	Protocol         int               `json:"protocol"`
	ReleaseDirectory string            `json:"release_directory,omitempty"`
	WebHash          string            `json:"web_hash,omitempty"`
	WebVersion       string            `json:"web_version,omitempty"`
	Readiness        string            `json:"readiness"`
	Inventory        map[string]any    `json:"inventory"`
	Components       map[string]string `json:"components"`
	Scheduler        any               `json:"scheduler,omitempty"`
	Transport        any               `json:"transport,omitempty"`
	UDP              any               `json:"udp,omitempty"`
	ActivityFailures uint64            `json:"activity_failures"`
	TopologyRetries  uint64            `json:"topology_retries"`
	PollFailures     int               `json:"poll_failures"`
	RecentErrors     []string          `json:"recent_errors"`
}

func Path(runtimeDir string) string {
	return filepath.Join(runtimeDir, "support-state.json")
}

func Write(runtimeDir string, snapshot Snapshot) error {
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return err
	}
	snapshot.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	path := Path(runtimeDir)
	temp, err := os.CreateTemp(runtimeDir, ".support-state.*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	written, writeErr := temp.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = temp.Sync()
	}
	closeErr := temp.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	if directory, err := os.Open(runtimeDir); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func Load(runtimeDir string) (Snapshot, error) {
	data, err := os.ReadFile(Path(runtimeDir))
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, fmt.Errorf("support snapshot is not available; start the relay once")
	}
	if err != nil {
		return Snapshot{}, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}
