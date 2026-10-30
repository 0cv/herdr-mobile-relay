package pibridge

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/conversation"
)

const ActivityMaxBytes = 1024 * 1024

var (
	activityPageSlots   = make(chan struct{}, 8)
	activityStreamSlots = make(chan struct{}, 8)
)

type ActivityFrame struct {
	Identity
	Challenge      string                           `json:"challenge"`
	Incarnation    string                           `json:"incarnation"`
	Kind           string                           `json:"kind"`
	Epoch          uint64                           `json:"epoch,omitempty"`
	Sequence       uint64                           `json:"sequence,omitempty"`
	SourceRevision string                           `json:"source_revision,omitempty"`
	Entries        []conversation.Entry             `json:"entries,omitempty"`
	Remove         []string                         `json:"remove,omitempty"`
	State          string                           `json:"state,omitempty"`
	Capabilities   *conversation.SourceCapabilities `json:"capabilities,omitempty"`
	Page           *conversation.BrowsePage         `json:"page,omitempty"`
}

func ActivityPage(ctx context.Context, identity Identity, cursor string, limit int) (conversation.BrowsePage, error) {
	var page conversation.BrowsePage
	err := activityRead(ctx, identity, "page", cursor, limit, func(frame ActivityFrame) error {
		if frame.Kind != "page" || frame.Page == nil {
			return errors.New("invalid activity page")
		}
		page = *frame.Page
		return nil
	})
	return page, err
}

func ActivityStream(ctx context.Context, identity Identity, receive func(ActivityFrame) error) error {
	return activityRead(ctx, identity, "subscribe", "", 200, receive)
}

func activityRead(ctx context.Context, identity Identity, operation, cursor string, limit int, receive func(ActivityFrame) error) error {
	if identity.PID <= 0 || identity.Pane == "" || identity.Session == "" || len(identity.Session) > 2048 || len(cursor) > 128 {
		return errors.New("invalid activity identity")
	}
	slots := activityPageSlots
	if operation == "subscribe" {
		slots = activityStreamSlots
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		return errors.New("activity bridge busy")
	}
	directory := Directory(identity.Instance)
	if directory == "" {
		return errors.New("invalid instance")
	}
	conn, err := openBridgeTransport(ctx, directory, fmt.Sprintf("%d.activity.sock", identity.PID))
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := checkPeer(conn.Conn.(*net.UnixConn), identity.PID); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	challenge := hex.EncodeToString(nonce[:])
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	request := struct {
		Identity
		Challenge string `json:"challenge"`
		Operation string `json:"operation"`
		Cursor    string `json:"cursor,omitempty"`
		Limit     int    `json:"limit"`
	}{identity, challenge, operation, cursor, limit}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return err
	}
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), ActivityMaxBytes)
	incarnation := ""
	for scanner.Scan() {
		var frame ActivityFrame
		if err := decodeBridge(scanner.Bytes(), &frame); err != nil {
			return err
		}
		after, err := owned(conn.path, true)
		if err != nil || !os.SameFile(conn.before, after) || frame.Identity != identity || frame.Challenge != challenge || len(frame.Incarnation) != 36 || (incarnation != "" && incarnation != frame.Incarnation) {
			return errors.New("activity source replaced")
		}
		incarnation = frame.Incarnation
		if err := validateActivityFrame(frame); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := receive(frame); err != nil {
			return err
		}
		if operation == "page" {
			return nil
		}
		if err := conn.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return errors.New("activity stream disconnected")
}

func validateActivityFrame(frame ActivityFrame) error {
	if frame.Kind != "page" && frame.Kind != "snapshot" && frame.Kind != "batch" && frame.Kind != "reset" && frame.Kind != "heartbeat" {
		return errors.New("invalid activity frame")
	}
	entries := frame.Entries
	if frame.Page != nil {
		if frame.Page.SourceRevision == "" || frame.Page.Capabilities == nil || frame.Page.Capabilities.Source != "pi_events" {
			return errors.New("invalid activity snapshot")
		}
		if len(frame.Page.Provisional) > 16 {
			return errors.New("activity provisional limit")
		}
		entries = append(append([]conversation.Entry{}, frame.Page.Entries...), frame.Page.Provisional...)
	}
	if len(entries) > 216 || len(frame.Remove) > 200 {
		return errors.New("activity frame exceeds limits")
	}
	for _, entry := range entries {
		if (entry.Kind != "" && entry.Kind != "compaction" && entry.Kind != "notice") || len(entry.ID) > 160 || (!strings.HasPrefix(entry.ID, "pi:") && !strings.HasPrefix(entry.ID, "live:")) || (entry.Role != "user" && entry.Role != "assistant") || len(entry.Text) > 65536 || len(entry.Tools) > 64 {
			return errors.New("invalid activity entry")
		}
		for _, tool := range entry.Tools {
			if len(tool.ID) > 256 || len(tool.ParentID) > 256 || len(tool.Name) > 160 || len(tool.Input) > 16384 || len(tool.Output) > 16384 || (tool.State != "" && tool.State != "running" && tool.State != "completed" && tool.State != "failed") {
				return errors.New("invalid activity tool")
			}
		}
	}
	return nil
}
