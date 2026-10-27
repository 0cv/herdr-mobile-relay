package pibridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/0cv/herdr-mobile-relay/internal/herdr"
)

const PromptCapability = "pi_prompt_unconfirmed"
const MaxPromptBytes = 100000

var promptRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type PromptLease struct {
	Identity
	Incarnation string `json:"incarnation"`
	Connection  string `json:"connection"`
	TargetKey   string `json:"target_key"`
	Token       string `json:"token"`
	ExpiresAt   int64  `json:"expires_at"`
}

type PromptResponse struct {
	PromptLease
	Challenge  string `json:"challenge"`
	Status     string `json:"status"`
	Capability string `json:"capability,omitempty"`
	SentAt     int64  `json:"sent_at,omitempty"`
}

func ValidPromptText(text string) bool {
	return utf8.ValidString(text) && len(text) <= MaxPromptBytes && strings.TrimSpace(text) != "" && !strings.Contains(text, "[attachment:") && !strings.Contains(text, "Attachment: ") && strings.IndexFunc(text, func(r rune) bool { return r < 32 && r != '\n' && r != '\t' || r == 127 }) < 0
}

func Prompt(ctx context.Context, lease PromptLease, requestID, text string) (PromptResponse, bool, error) {
	return promptExchange(ctx, Directory(lease.Instance), lease, requestID, text)
}

func promptExchange(ctx context.Context, directory string, lease PromptLease, requestID, text string) (PromptResponse, bool, error) {
	var result PromptResponse
	if directory == "" || lease.PID <= 0 || lease.Pane == "" || lease.Session == "" || len(lease.Session) > 2048 || lease.Connection == "" || len(lease.Connection) > 160 || lease.TargetKey == "" || len(lease.TargetKey) > 4096 {
		return result, false, errors.New("missing bridge identity")
	}
	operation := "prompt_capability"
	if requestID != "" {
		operation = "send_prompt_unconfirmed"
		if !promptRequestID.MatchString(requestID) || !strings.HasPrefix(requestID, lease.Incarnation+"_") || !ValidPromptText(text) || len(lease.Incarnation) != 36 || len(lease.Token) != 36 {
			return result, false, errors.New("invalid bridge prompt")
		}
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		return result, false, errors.New("bridge busy")
	}
	ctx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer cancel()
	if _, err := owned(directory, false); err != nil {
		return result, false, err
	}
	path := filepath.Join(directory, fmt.Sprintf("%d.sock", lease.PID))
	before, err := owned(path, true)
	if err != nil {
		return result, false, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return result, false, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return result, false, err
	}
	if err := checkPeer(conn.(*net.UnixConn), lease.PID); err != nil {
		return result, false, err
	}
	challengeBytes := make([]byte, 16)
	if _, err := rand.Read(challengeBytes); err != nil {
		return result, false, err
	}
	challenge := hex.EncodeToString(challengeBytes)
	request := map[string]any{"instance": lease.Instance, "pane": lease.Pane, "session": lease.Session, "pid": lease.PID, "challenge": challenge, "operation": operation, "connection": lease.Connection, "target_key": lease.TargetKey}
	if requestID != "" {
		request["incarnation"], request["token"], request["request_id"], request["text"] = lease.Incarnation, lease.Token, requestID, text
	}
	if err := herdr.CheckDispatch(ctx); err != nil {
		return result, false, err
	}
	if err := ctx.Err(); err != nil {
		return result, false, err
	}
	attempted := requestID != ""
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return result, attempted, err
	}
	data, err := io.ReadAll(io.LimitReader(conn, 16385))
	if err != nil {
		return result, attempted, err
	}
	if len(data) > 16384 {
		return result, attempted, errors.New("bridge response too large")
	}
	if err := decodeBridge(data, &result); err != nil {
		return result, attempted, err
	}
	after, err := owned(path, true)
	if err != nil || !os.SameFile(before, after) {
		return result, attempted, errors.New("bridge replaced")
	}
	if result.Identity != lease.Identity || result.Challenge != challenge || len(result.Incarnation) != 36 {
		return result, attempted, errors.New("bridge identity mismatch")
	}
	if result.Status == "not_started" {
		return result, false, errors.New("Pi bridge refused the request")
	}
	if requestID == "" {
		if result.Status != "available" || result.Capability != PromptCapability || result.Connection != lease.Connection || result.TargetKey != lease.TargetKey || len(result.Token) != 36 || result.ExpiresAt <= time.Now().UnixMilli() {
			return result, false, errors.New("Pi prompt capability unavailable")
		}
	} else if result.Status != "unconfirmed" || result.Incarnation != lease.Incarnation || result.SentAt <= 0 {
		return result, attempted, errors.New("invalid bridge prompt result")
	}
	return result, attempted, nil
}
