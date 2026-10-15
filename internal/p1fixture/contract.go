package p1fixture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/childenv"
)

const FileLimit = 512 * 1024
const EvidenceLimit = 4 * 1024 * 1024

var ErrContract = errors.New("invalid P1 fixture contract")

type Contract struct {
	Root              string `json:"root"`
	SocketPath        string `json:"socketPath"`
	UID               int    `json:"uid"`
	ServerPID         int    `json:"serverPid"`
	ServerStart       string `json:"serverStart"`
	EvidenceDirectory string `json:"evidenceDirectory"`
}

func privatePathMode(path string, directory bool, uid int, mode os.FileMode) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("%w: noncanonical path %q", ErrContract, path)
	}
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(path, current), current) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || current != path && !info.IsDir() {
			return fmt.Errorf("%w: unsafe component %q", ErrContract, current)
		}
		if current != path {
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != uid || directory != info.IsDir() {
			return fmt.Errorf("%w: unowned path %q", ErrContract, path)
		}
		if info.Mode().Perm() != mode || !directory && !info.Mode().IsRegular() {
			return fmt.Errorf("%w: unsafe mode %q", ErrContract, path)
		}
	}
	return nil
}

func privatePath(path string, directory bool, uid int) error {
	mode := os.FileMode(0600)
	if directory {
		mode = 0700
	}
	return privatePathMode(path, directory, uid, mode)
}

func ReadPrivate(path string, uid int) ([]byte, error) {
	return readPrivateMode(path, uid, 0600)
}

func readPrivateMode(path string, uid int, mode os.FileMode) ([]byte, error) {
	if err := privatePathMode(path, false, uid, mode); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != uid || !info.Mode().IsRegular() || info.Mode().Perm() != mode || info.Size() > FileLimit {
		return nil, ErrContract
	}
	data, err := io.ReadAll(io.LimitReader(file, FileLimit+1))
	if err != nil || len(data) > FileLimit {
		return nil, errors.Join(err, fmt.Errorf("%w: bounded read %q", ErrContract, path))
	}
	return data, nil
}

func Validate(input string, getenv func(string) string) (*Contract, error) {
	if len(input) == 0 || len(input) > FileLimit || getenv == nil {
		return nil, ErrContract
	}
	var c Contract
	decoder := json.NewDecoder(strings.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&c); err != nil {
		return nil, errors.Join(ErrContract, err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, ErrContract
	}
	parent, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		return nil, err
	}
	if c.UID != os.Getuid() || filepath.Dir(c.Root) != parent || !strings.HasPrefix(filepath.Base(c.Root), "herdr-scroll-p1-") || c.SocketPath != filepath.Join(c.Root, "config/herdr/herdr.sock") || c.ServerPID <= 0 || c.ServerStart == "" || c.EvidenceDirectory != filepath.Join(c.Root, "evidence") {
		return nil, ErrContract
	}
	if err := privatePath(c.Root, true, c.UID); err != nil {
		return nil, err
	}
	marker, err := ReadPrivate(filepath.Join(c.Root, "fixture-owner"), c.UID)
	if err != nil || string(marker) != "disposable-scroll-fixture\n" {
		return nil, errors.Join(err, ErrContract)
	}
	for key, relative := range map[string]string{"HOME": "home", "XDG_CONFIG_HOME": "config", "XDG_CACHE_HOME": "cache", "XDG_DATA_HOME": "data", "XDG_STATE_HOME": "state", "XDG_RUNTIME_DIR": "runtime", "TMPDIR": "tmp", "PI_CODING_AGENT_DIR": "pi"} {
		path := filepath.Join(c.Root, relative)
		if getenv(key) != path {
			return nil, fmt.Errorf("%w: %s escaped fixture", ErrContract, key)
		}
		if err := privatePath(path, true, c.UID); err != nil {
			return nil, err
		}
	}
	if getenv("HERDR_SOCKET_PATH") != c.SocketPath || getenv("HERDR_CONFIG_PATH") != filepath.Join(c.Root, "config/herdr/config.toml") {
		return nil, ErrContract
	}
	info, err := os.Lstat(c.SocketPath)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != c.UID || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 {
		return nil, ErrContract
	}
	return &c, nil
}

func PrepareEnvironment() error {
	if input, supplied := os.LookupEnv("HERDR_P1_CONTRACT_JSON"); supplied {
		_, err := Validate(input, os.Getenv)
		return err
	}
	for _, key := range os.Environ() {
		if strings.HasPrefix(key, "HERDR_SCROLL_FIXTURE_") || strings.HasPrefix(key, "HERDR_ROW_FIXTURE_") || strings.HasPrefix(key, "HERDR_LATENCY_FIXTURE_") {
			return ErrContract
		}
	}
	if runtime.GOOS == "darwin" {
		path, err := filepath.EvalSymlinks("/tmp")
		if err != nil {
			return err
		}
		return os.Setenv("TMPDIR", path)
	}
	return nil
}

func Current() (*Contract, error) {
	c, err := Validate(os.Getenv("HERDR_P1_CONTRACT_JSON"), os.Getenv)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	birth, err := processStart(ctx, c.ServerPID)
	if err != nil || birth != c.ServerStart {
		return nil, errors.Join(err, ErrContract)
	}
	owners, err := Run(ctx, "lsof", "", "-n", "-P", "-a", "-p", strconv.Itoa(c.ServerPID), "-U", "-Fn")
	if err != nil || !strings.Contains(string(owners), "\nn"+c.SocketPath+"\n") {
		return nil, errors.Join(err, ErrContract)
	}
	return c, nil
}

func processStart(ctx context.Context, pid int) (string, error) {
	out, err := childenv.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	return strings.Join(strings.Fields(string(out)), " "), err
}

type commandStream struct {
	mu       sync.Mutex
	data     []byte
	overflow bool
	frozen   bool
	cancel   context.CancelFunc
}

func (s *commandStream) Write(data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.frozen {
		return len(data), nil
	}
	n := min(len(data), 64*1024-len(s.data))
	s.data = append(s.data, data[:n]...)
	if n != len(data) {
		s.overflow = true
		s.cancel()
		return n, errors.New("P1 helper stream limit exceeded")
	}
	return n, nil
}

func (s *commandStream) snapshot() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frozen = true
	if s.overflow {
		return append([]byte(nil), s.data...), errors.New("P1 helper stream limit exceeded")
	}
	return append([]byte(nil), s.data...), nil
}

func Run(ctx context.Context, name, input string, args ...string) ([]byte, error) {
	if len(input) > 64*1024 {
		return nil, errors.New("P1 helper input limit exceeded")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := childenv.Command(name, args...)
	command.Stdin = strings.NewReader(input)
	command.WaitDelay = 200 * time.Millisecond
	stdout, stderr := &commandStream{cancel: cancel}, &commandStream{cancel: cancel}
	command.Stdout, command.Stderr = stdout, stderr
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	birth, birthErr := childBirth(command.Process.Pid)
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	var waitErr error
	select {
	case waitErr = <-wait:
	case <-ctx.Done():
		signal := func(value os.Signal) error {
			current, err := childBirth(command.Process.Pid)
			if birthErr != nil || birth == "" || err != nil || current != birth {
				return errors.New("P1 child identity unavailable; no signal sent")
			}
			return command.Process.Signal(value)
		}
		waitErr = errors.Join(ctx.Err(), signal(syscall.SIGTERM))
		select {
		case err := <-wait:
			waitErr = errors.Join(waitErr, err)
		case <-time.After(200 * time.Millisecond):
			waitErr = errors.Join(waitErr, signal(syscall.SIGKILL))
			select {
			case err := <-wait:
				waitErr = errors.Join(waitErr, err)
			case <-time.After(200 * time.Millisecond):
				waitErr = errors.Join(waitErr, errors.New("P1 child cleanup incomplete"))
			}
		}
	}
	output, stdoutErr := stdout.snapshot()
	_, stderrErr := stderr.snapshot()
	return output, errors.Join(waitErr, stdoutErr, stderrErr)
}

type CommandObservation struct {
	Name    string        `json:"name"`
	Args    []string      `json:"args"`
	Output  []byte        `json:"output_base64"`
	Started time.Time     `json:"started"`
	Elapsed time.Duration `json:"elapsed_ns"`
	Error   string        `json:"error,omitempty"`
}

type Runner struct {
	mu      sync.Mutex
	records []CommandObservation
	bytes   int
	failed  bool
}

func (r *Runner) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name != "ps" && name != "stty" {
		return nil, errors.New("unsupported P1 geometry helper")
	}
	start := time.Now()
	output, err := Run(ctx, name, "", args...)
	record := CommandObservation{Name: name, Args: append([]string(nil), args...), Output: append([]byte(nil), output...), Started: start, Elapsed: time.Since(start)}
	if err != nil {
		record.Error = err.Error()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.records) >= 128 || r.bytes+len(output) > 256*1024 {
		r.failed = true
		return output, errors.Join(err, errors.New("P1 geometry observation overflow"))
	}
	r.records = append(r.records, record)
	r.bytes += len(output)
	return output, err
}

func (r *Runner) Observations() ([]CommandObservation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed {
		return nil, errors.New("P1 geometry observation overflow")
	}
	result := append([]CommandObservation(nil), r.records...)
	return result, nil
}

func (c Contract) Save(name string, value any) error {
	if c.UID != os.Getuid() || c.EvidenceDirectory != filepath.Join(c.Root, "evidence") || filepath.Base(name) != name || !strings.HasSuffix(name, ".json") {
		return ErrContract
	}
	if err := privatePath(c.Root, true, c.UID); err != nil {
		return err
	}
	if err := privatePath(c.EvidenceDirectory, true, c.UID); err != nil {
		return err
	}
	directoryInfo, err := os.Lstat(c.EvidenceDirectory)
	if err != nil {
		return err
	}
	lockPath := c.EvidenceDirectory + ".lock"
	fd, err := syscall.Open(lockPath, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	lock := os.NewFile(uintptr(fd), lockPath)
	defer lock.Close()
	if err := privatePath(lockPath, false, c.UID); err != nil {
		return err
	}
	lockInfo, err := lock.Stat()
	pathInfo, pathErr := os.Lstat(lockPath)
	if err != nil || pathErr != nil || !os.SameFile(lockInfo, pathInfo) {
		return errors.Join(err, pathErr, ErrContract)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || !time.Now().Before(deadline) {
			return errors.Join(err, errors.New("P1 evidence accounting lock unavailable"))
		}
		time.Sleep(time.Millisecond)
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	currentDirectory, err := os.Lstat(c.EvidenceDirectory)
	currentLock, lockErr := os.Lstat(lockPath)
	if err != nil || lockErr != nil || !os.SameFile(directoryInfo, currentDirectory) || !os.SameFile(lockInfo, currentLock) {
		return errors.Join(err, lockErr, ErrContract)
	}
	if err := privatePath(c.EvidenceDirectory, true, c.UID); err != nil {
		return err
	}
	if err := privatePath(lockPath, false, c.UID); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil || len(data)+1 > FileLimit {
		return errors.Join(err, errors.New("P1 evidence file limit exceeded"))
	}
	entries, err := os.ReadDir(c.EvidenceDirectory)
	if err != nil {
		return err
	}
	total := int64(len(data) + 1)
	for _, entry := range entries {
		path := filepath.Join(c.EvidenceDirectory, entry.Name())
		if err := privatePath(path, false, c.UID); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > FileLimit {
			return errors.New("P1 existing evidence file limit exceeded")
		}
		total += info.Size()
	}
	if total > EvidenceLimit {
		return errors.New("P1 total evidence limit exceeded")
	}
	file, err := os.OpenFile(filepath.Join(c.EvidenceDirectory, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	payload := append(data, '\n')
	n, writeErr := file.Write(payload)
	if n != len(payload) && writeErr == nil {
		writeErr = io.ErrShortWrite
	}
	return errors.Join(writeErr, file.Sync(), file.Close())
}
