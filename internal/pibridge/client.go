package pibridge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/slashcmd"
)

const MaxBytes = 2 * 1024 * 1024

var slots = make(chan struct{}, 8)
var commandPattern = regexp.MustCompile(`^/[A-Za-z0-9][A-Za-z0-9._:-]{0,119}$`)
var sourceURLPattern = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://`)
var pathPartPattern = regexp.MustCompile(`[^/\\]+`)

func sanitizeSource(source string) string {
	source = strings.TrimSpace(source)
	prefix := ""
	if strings.HasPrefix(source, "git:") && !strings.HasPrefix(source, "git://") {
		prefix = "git:"
	}
	value := strings.TrimSpace(strings.TrimPrefix(source, prefix))
	if sourceURLPattern.MatchString(value) {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" {
			return "redacted"
		}
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		parsed.RawFragment = ""
		return prefix + parsed.String()
	}
	if strings.Contains(value, "://") {
		return "redacted"
	}
	if prefix != "" || strings.HasPrefix(value, "git@") {
		end := strings.IndexByte(value, '/')
		if end < 0 {
			end = len(value)
		}
		decoded, err := url.PathUnescape(value[:end])
		if err != nil {
			return "redacted"
		}
		if at := strings.LastIndexByte(decoded, '@'); at > 0 {
			value = decoded[at+1:] + value[end:]
		}
	}
	return stripSourceQuery(prefix + value)
}

func stripSourceQuery(value string) string {
	if index := strings.IndexAny(value, "?#"); index >= 0 {
		return value[:index]
	}
	return value
}

func sanitizePath(path string) string {
	return stripSourceQuery(pathPartPattern.ReplaceAllStringFunc(path, func(part string) string {
		decoded, err := url.PathUnescape(part)
		if err != nil {
			return "redacted"
		}
		if at := strings.LastIndexByte(decoded, '@'); at > 0 {
			return decoded[at+1:]
		}
		if strings.ContainsAny(decoded, "?#") {
			return decoded
		}
		return part
	}))
}

type Identity struct {
	Instance string `json:"instance"`
	Pane     string `json:"pane"`
	Session  string `json:"session"`
	PID      int    `json:"pid"`
}
type Response struct {
	Identity
	Challenge   string                    `json:"challenge"`
	Incarnation string                    `json:"incarnation"`
	Status      string                    `json:"status"`
	Revision    string                    `json:"revision"`
	Truncated   bool                      `json:"truncated"`
	Commands    []slashcmd.RuntimeCommand `json:"commands"`
}

func Instance(socketPath string) (string, error) {
	path, err := filepath.EvalSymlinks(socketPath)
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSocket == 0 || int(stat.Uid) != os.Getuid() {
		return "", errors.New("invalid Herdr socket")
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\n%d\n%d", path, stat.Dev, stat.Ino)))
	return hex.EncodeToString(sum[:]), nil
}

func Directory(instance string) string {
	if len(instance) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(instance); err != nil {
		return ""
	}
	return fmt.Sprintf("/tmp/herdr-pi-%d-%s", os.Getuid(), instance[:16])
}

func owned(path string, socket bool) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	expected := os.FileMode(0700)
	validType := info.IsDir()
	if socket {
		expected = 0600
		validType = info.Mode()&os.ModeSocket != 0
	}
	if !ok || int(stat.Uid) != os.Getuid() || !validType || info.Mode().Perm() != expected {
		return nil, errors.New("unsafe bridge endpoint")
	}
	return info, nil
}

func Query(ctx context.Context, identity Identity) (Response, error) {
	return query(ctx, Directory(identity.Instance), identity)
}

func query(ctx context.Context, directory string, identity Identity) (Response, error) {
	var result Response
	if directory == "" || identity.PID <= 0 || identity.Pane == "" || identity.Session == "" || len(identity.Session) > 2048 {
		return result, errors.New("missing bridge identity")
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		return result, errors.New("bridge busy")
	}
	ctx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer cancel()
	if _, err := owned(directory, false); err != nil {
		return result, err
	}
	path := filepath.Join(directory, fmt.Sprintf("%d.sock", identity.PID))
	before, err := owned(path, true)
	if err != nil {
		return result, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return result, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return result, err
	}
	if err := checkPeer(conn.(*net.UnixConn), identity.PID); err != nil {
		return result, err
	}
	challengeBytes := make([]byte, 16)
	if _, err := rand.Read(challengeBytes); err != nil {
		return result, err
	}
	challenge := hex.EncodeToString(challengeBytes)
	request := struct {
		Identity
		Challenge string `json:"challenge"`
	}{identity, challenge}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return result, err
	}
	data, err := io.ReadAll(io.LimitReader(conn, MaxBytes+1))
	if err != nil {
		return result, err
	}
	if len(data) > MaxBytes {
		return result, errors.New("bridge response too large")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return result, errors.New("trailing bridge data")
	}
	after, err := owned(path, true)
	if err != nil || !os.SameFile(before, after) {
		return result, errors.New("bridge replaced")
	}
	if result.Identity != identity || result.Challenge != challenge || len(result.Incarnation) != 36 || len(result.Revision) != 64 {
		return result, errors.New("bridge identity mismatch")
	}
	if result.Status != "available" && result.Status != "partial" && result.Status != "loading" && result.Status != "unavailable" {
		return result, errors.New("invalid bridge status")
	}
	if len(result.Commands) > 4096 {
		return result, errors.New("too many bridge commands")
	}
	for index, entry := range result.Commands {
		if err := validate(entry); err != nil {
			return Response{}, err
		}
		result.Commands[index].Provenance.Source = sanitizeSource(entry.Provenance.Source)
		result.Commands[index].Provenance.Path = sanitizePath(entry.Provenance.Path)
		result.Commands[index].Provenance.BaseDir = sanitizePath(entry.Provenance.BaseDir)
	}
	return result, nil
}

func validate(entry slashcmd.RuntimeCommand) error {
	valid := func(s string, max int) bool {
		return len(s) <= max && strings.IndexFunc(s, func(r rune) bool { return r < 32 || r == 127 }) == -1
	}
	if !commandPattern.MatchString(entry.Command.Command) {
		return errors.New("invalid command")
	}
	p := entry.Provenance
	if p == nil || p.Path == "" || p.Source == "" || !valid(entry.Description, 960) || !valid(entry.ArgumentHint, 480) || !valid(p.Path, 4096) || !valid(p.Source, 1024) || !valid(p.BaseDir, 4096) {
		return errors.New("invalid metadata")
	}
	if entry.Kind != "extension" && entry.Kind != "prompt" && entry.Kind != "skill" {
		return errors.New("invalid kind")
	}
	if p.Scope != "user" && p.Scope != "project" && p.Scope != "temporary" {
		return errors.New("invalid scope")
	}
	if p.Origin != "package" && p.Origin != "top-level" {
		return errors.New("invalid origin")
	}
	expected := p.Scope
	if expected == "user" {
		expected = "personal"
	}
	if entry.Source != expected {
		return errors.New("scope mismatch")
	}
	return nil
}
