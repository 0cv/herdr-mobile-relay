package p1fixture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/childenv"
	"github.com/0cv/herdr-mobile-relay/internal/ownedproc"
)

const FileLimit = 512 * 1024
const EvidenceLimit = 4 * 1024 * 1024

var ErrContract = errors.New("invalid P1 fixture contract")

type Contract struct {
	StageRoot string `json:"stageRoot"`
	StageID string `json:"stageId"`
	RunID string `json:"runId"`
	AttemptID string `json:"attemptId"`
	Root string `json:"root"`
	SocketPath string `json:"socketPath"`
	LockToken string `json:"lockToken"`
	UID int `json:"uid"`
	LaunchInputPath string `json:"launchInputPath"`
	LaunchInputSHA256 string `json:"launchInputSha256"`
	EvidenceDirectory string `json:"evidenceDirectory"`
}

type stageIdentity struct {
	StageRoot string `json:"stage_root"`
	StageID string `json:"stage_id"`
	RunID string `json:"run_id"`
	AttemptID string `json:"attempt_id"`
	Root string `json:"root_path"`
	UID int `json:"uid"`
	OwnerUID int `json:"owner_uid"`
	Kind string `json:"kind"`
	Seq int `json:"seq"`
	Number int `json:"attempt_number"`
	MaxAttempts int `json:"max_attempts"`
	AllocationLimit int `json:"allocation_limit"`
	Parent string `json:"fixture_root_parent"`
	Prefix string `json:"fixture_root_prefix"`
	Successor struct {
		StageRoot string `json:"stageRoot"`
		StageID string `json:"stageId"`
		RunID string `json:"runId"`
		DispositionPath string `json:"dispositionPath"`
		DispositionSHA256 string `json:"dispositionSha256"`
	} `json:"successor"`
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

func readJSON(path string, uid int, target any) error {
	data, err := ReadPrivate(path, uid)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func identityMatches(value stageIdentity, c Contract) bool {
	return value.StageID == c.StageID && value.RunID == c.RunID && value.AttemptID == c.AttemptID && value.Root == c.Root
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validateSuccessorAuthority(owner stageIdentity, c Contract) error {
	pin := owner.Successor
	if pin.StageRoot == "" || pin.StageRoot == c.StageRoot || pin.StageID == "" || pin.StageID == c.StageID || pin.RunID == "" || pin.RunID == c.RunID {
		return ErrContract
	}
	data, err := readPrivateMode(pin.DispositionPath, c.UID, 0400)
	if err != nil || len(pin.DispositionSHA256) != 64 || digest(data) != pin.DispositionSHA256 {
		return errors.Join(err, ErrContract)
	}
	var disposition struct {
		Schema int `json:"schema_version"`
		Kind string `json:"kind"`
		HistoricalClosureProved *bool `json:"historicalClosureProved"`
		Namespace map[string]json.RawMessage `json:"namespace"`
		Decision struct {
			Path string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"decision"`
	}
	if err := json.Unmarshal(data, &disposition); err != nil || disposition.Schema != 1 || disposition.Kind != "successor-historical-disposition" || disposition.HistoricalClosureProved == nil || *disposition.HistoricalClosureProved {
		return errors.Join(err, ErrContract)
	}
	for key, expected := range map[string]string{"stage": c.StageRoot, "fixture": c.Root, "home": filepath.Join(c.Root, "home"), "config": filepath.Join(c.Root, "config"), "tmp": filepath.Join(c.Root, "tmp"), "profiles": filepath.Join(c.Root, "profiles"), "cache": filepath.Join(c.Root, "cache"), "data": filepath.Join(c.Root, "data"), "state": filepath.Join(c.Root, "state"), "runtime": filepath.Join(c.Root, "runtime")} {
		var actual string
		if err := json.Unmarshal(disposition.Namespace[key], &actual); err != nil || actual != expected {
			return errors.Join(err, ErrContract)
		}
	}
	for key, expected := range map[string]int{"localAllocationLimit": 1, "historicalCount": 6, "cumulativeCount": 7} {
		var actual int
		if err := json.Unmarshal(disposition.Namespace[key], &actual); err != nil || actual != expected {
			return errors.Join(err, ErrContract)
		}
	}
	if disposition.Decision.Path == pin.DispositionPath {
		return ErrContract
	}
	decisionBytes, err := readPrivateMode(disposition.Decision.Path, c.UID, 0400)
	if err != nil || digest(decisionBytes) != disposition.Decision.SHA256 {
		return errors.Join(err, ErrContract)
	}
	var decision struct {
		Kind string `json:"kind"`
		Phase string `json:"phase"`
		SourceOnly *bool `json:"sourceOnly"`
		StageID string `json:"stageId"`
		RunID string `json:"runId"`
		HistoricalClosureProved *bool `json:"historicalClosureProved"`
		LocalAllocationLimit int `json:"localAllocationLimit"`
		HistoricalCount int `json:"historicalCount"`
		CumulativeCount int `json:"cumulativeCount"`
		Namespace map[string]json.RawMessage `json:"namespace"`
	}
	if err := json.Unmarshal(decisionBytes, &decision); err != nil || decision.Kind != "p1-native-runtime-authority" || decision.Phase != "N" || decision.SourceOnly == nil || *decision.SourceOnly || decision.StageID != c.StageID || decision.RunID != c.RunID || decision.HistoricalClosureProved == nil || *decision.HistoricalClosureProved || decision.LocalAllocationLimit != 1 || decision.HistoricalCount != 6 || decision.CumulativeCount != 7 {
		return errors.Join(err, ErrContract)
	}
	if len(decision.Namespace) != len(disposition.Namespace) {
		return ErrContract
	}
	for key, value := range disposition.Namespace {
		if !bytes.Equal(bytes.TrimSpace(decision.Namespace[key]), bytes.TrimSpace(value)) {
			return ErrContract
		}
	}
	return nil
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
	if c.UID != os.Getuid() || c.StageID == "" || c.RunID == "" || c.AttemptID != "attempt-001" || c.LockToken == "" || c.SocketPath != filepath.Join(c.Root, "config/herdr/herdr.sock") || len(c.SocketPath) >= 104 || c.EvidenceDirectory != filepath.Join(c.Root, "evidence") {
		return nil, ErrContract
	}
	for _, path := range []string{c.StageRoot, c.Root, filepath.Join(c.StageRoot, "attempts"), filepath.Join(c.StageRoot, "attempts", c.AttemptID), filepath.Join(c.StageRoot, "active.lock"), c.EvidenceDirectory} {
		if err := privatePath(path, true, c.UID); err != nil {
			return nil, err
		}
	}
	paths := map[string]string{"HOME": "home", "XDG_CONFIG_HOME": "config", "XDG_CACHE_HOME": "cache", "XDG_DATA_HOME": "data", "XDG_STATE_HOME": "state", "XDG_RUNTIME_DIR": "runtime", "TMPDIR": "tmp", "NODE_COMPILE_CACHE": "cache/node"}
	if getenv("BUN_RUNTIME_TRANSPILER_CACHE_PATH") != "0" {
		return nil, fmt.Errorf("%w: Bun cache is not disabled", ErrContract)
	}
	for key, relative := range paths {
		path := filepath.Join(c.Root, relative)
		if getenv(key) != path {
			return nil, fmt.Errorf("%w: %s does not match owned root", ErrContract, key)
		}
		if err := privatePath(path, true, c.UID); err != nil {
			return nil, err
		}
	}
	var owner stageIdentity
	ownerPath := filepath.Join(c.StageRoot, "stage-owner.json")
	ownerBytes, err := ReadPrivate(ownerPath, c.UID)
	if err != nil || json.Unmarshal(ownerBytes, &owner) != nil || owner.StageRoot != c.StageRoot || owner.StageID != c.StageID || owner.RunID != c.RunID || owner.OwnerUID != c.UID || owner.MaxAttempts != 6 || owner.AllocationLimit != 1 || owner.Prefix != "herdr-scroll-p1-"+c.StageID+"-" || c.Root != filepath.Join(owner.Parent, owner.Prefix+c.AttemptID) {
		return nil, errors.Join(err, ErrContract)
	}
	if err := validateSuccessorAuthority(owner, c); err != nil {
		return nil, err
	}
	var lock struct {
		Token string `json:"token"`
		RunID string `json:"run_id"`
	}
	if err := readJSON(filepath.Join(c.StageRoot, "active.lock/owner.json"), c.UID, &lock); err != nil || lock.Token != c.LockToken || lock.RunID != c.RunID {
		return nil, errors.Join(err, ErrContract)
	}
	entries, err := os.ReadDir(filepath.Join(c.StageRoot, "active.lock"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "owner.json" {
		return nil, errors.Join(err, ErrContract)
	}
	for _, path := range []string{filepath.Join(c.StageRoot, "attempts", c.AttemptID, "attempt-owner.json"), filepath.Join(c.StageRoot, "attempts", c.AttemptID, "root-created.json"), filepath.Join(c.Root, ".stage-owner.json")} {
		var marker stageIdentity
		if err := readJSON(path, c.UID, &marker); err != nil || !identityMatches(marker, c) {
			return nil, errors.Join(err, ErrContract)
		}
		if filepath.Base(path) == ".stage-owner.json" && marker.UID != c.UID {
			return nil, ErrContract
		}
	}
	marker, err := ReadPrivate(filepath.Join(c.Root, "fixture-owner"), c.UID)
	if err != nil || string(marker) != "disposable-scroll-fixture\n" {
		return nil, errors.Join(err, ErrContract)
	}
	ledger, err := ReadPrivate(filepath.Join(c.StageRoot, "allocation-ledger.jsonl"), c.UID)
	if err != nil || len(ledger) == 0 || ledger[len(ledger)-1] != '\n' {
		return nil, errors.Join(err, ErrContract)
	}
	allocations := 0
	for index, line := range bytes.Split(bytes.TrimSuffix(ledger, []byte{'\n'}), []byte{'\n'}) {
		var record stageIdentity
		if err := json.Unmarshal(line, &record); err != nil || record.Seq != index+1 || record.StageID != c.StageID || record.RunID != c.RunID {
			return nil, errors.Join(err, ErrContract)
		}
		if record.Kind != "allocation" || !identityMatches(record, c) || record.Number != 1 {
			return nil, ErrContract
		}
		allocations++
	}
	if allocations != 1 {
		return nil, ErrContract
	}
	launchBytes, err := readPrivateMode(c.LaunchInputPath, c.UID, 0400)
	if err != nil || len(c.LaunchInputSHA256) != 64 || digest(launchBytes) != c.LaunchInputSHA256 {
		return nil, errors.Join(err, ErrContract)
	}
	var launch struct {
		SchemaVersion int `json:"schema_version"`
		SourceKind string `json:"source_kind"`
		Complete bool `json:"complete"`
		Truncated *bool `json:"truncated"`
		Ambiguous *bool `json:"ambiguous"`
		Skipped *bool `json:"skipped"`
		Kind string `json:"kind"`
		Phase string `json:"phase"`
		Contract Contract `json:"contract"`
		OwnerSHA256 string `json:"ownerSha256"`
		AllocationSHA256 string `json:"allocationSha256"`
		Environment map[string]string `json:"environment"`
	}
	launchDecoder := json.NewDecoder(bytes.NewReader(launchBytes))
	launchDecoder.DisallowUnknownFields()
	if err := launchDecoder.Decode(&launch); err != nil || launch.SchemaVersion != 1 || launch.SourceKind != "real" || !launch.Complete || launch.Truncated == nil || *launch.Truncated || launch.Ambiguous == nil || *launch.Ambiguous || launch.Skipped == nil || *launch.Skipped || launch.Kind != "p1-owned-native-launch" || launch.Phase != "N" || launch.Contract.StageRoot != c.StageRoot || launch.Contract.StageID != c.StageID || launch.Contract.RunID != c.RunID || launch.Contract.Root != c.Root || launch.Contract.AttemptID != c.AttemptID || launch.Contract.SocketPath != c.SocketPath || launch.Contract.LockToken != c.LockToken || launch.Contract.UID != c.UID || launch.Contract.EvidenceDirectory != c.EvidenceDirectory || launch.Contract.LaunchInputPath != c.LaunchInputPath || launch.Contract.LaunchInputSHA256 != "" || launch.OwnerSHA256 != digest(ownerBytes) || launch.AllocationSHA256 != digest(ledger) {
		return nil, errors.Join(err, ErrContract)
	}
	if err := launchDecoder.Decode(new(any)); !errors.Is(err, io.EOF) || len(launch.Environment) != len(paths)+2 {
		return nil, ErrContract
	}
	for key, relative := range paths {
		if launch.Environment[key] != filepath.Join(c.Root, relative) {
			return nil, ErrContract
		}
	}
	for key, value := range launch.Environment {
		if getenv(key) != value {
			return nil, fmt.Errorf("%w: pinned launch environment %s changed", ErrContract, key)
		}
	}
	if launch.Environment["BUN_RUNTIME_TRANSPILER_CACHE_PATH"] != "0" || launch.Environment["HERDR_P1_CONTRACT_BUN"] == "" {
		return nil, ErrContract
	}
	for _, path := range []string{c.SocketPath, filepath.Join(c.Root, "config/herdr/herdr-client.sock")} {
		if err := privatePath(filepath.Dir(path), true, c.UID); err != nil {
			return nil, err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != c.UID || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 {
			return nil, ErrContract
		}
	}
	return &c, nil
}

func PrepareEnvironment() error {
	input, supplied := os.LookupEnv("HERDR_P1_CONTRACT_JSON")
	if supplied {
		_, err := Validate(input, os.Getenv)
		return err
	}
	for _, key := range []string{"HERDR_P1_CONTRACT_BUN", "HERDR_SCROLL_FIXTURE_SOCKET", "HERDR_SCROLL_FIXTURE_PANE", "HERDR_SCROLL_FIXTURE_SESSION", "HERDR_SCROLL_FIXTURE_BIN", "HERDR_SCROLL_FIXTURE_MODE", "HERDR_ROW_FIXTURE_SOCKET", "HERDR_ROW_FIXTURE_PANE", "HERDR_ROW_FIXTURE_TERMINAL", "HERDR_ROW_FIXTURE_SESSION", "HERDR_ROW_FIXTURE_BIN", "HERDR_LATENCY_FIXTURE_SOCKET", "HERDR_LATENCY_FIXTURE_PANE", "HERDR_LATENCY_FIXTURE_TERMINAL", "HERDR_LATENCY_FIXTURE_SESSION", "HERDR_LATENCY_FIXTURE_BIN"} {
		if _, supplied := os.LookupEnv(key); supplied {
			return fmt.Errorf("%w: partial fixture environment", ErrContract)
		}
	}
	if runtime.GOOS != "darwin" {
		return nil
	}
	path, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		return err
	}
	return os.Setenv("TMPDIR", path)
}

func Current() (*Contract, error) {
	return Validate(os.Getenv("HERDR_P1_CONTRACT_JSON"), os.Getenv)
}

type commandStream struct {
	mu sync.Mutex
	data []byte
	overflow bool
	frozen bool
	cancel context.CancelFunc
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
	command.Env = append(command.Env, "BUN_RUNTIME_TRANSPILER_CACHE_PATH=0")
	command.Stdin = strings.NewReader(input)
	command.WaitDelay = 200*time.Millisecond
	stdout, stderr := &commandStream{cancel: cancel}, &commandStream{cancel: cancel}
	command.Stdout, command.Stderr = stdout, stderr
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	inspection, stop := context.WithTimeout(context.Background(), ownedproc.InspectionBudget)
	registry, ownershipErr := ownedproc.New(inspection, ownedproc.Native{}, command.Process.Pid, os.Getpid())
	stop()
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	if ownershipErr != nil {
		cancel()
	}
	var waitErr, cancelErr error
	select {
	case waitErr = <-wait:
	case <-ctx.Done():
		cancelErr = ctx.Err()
		for _, force := range []bool{false, true} {
			if ownershipErr != nil {
				cancelErr = errors.Join(cancelErr, ownedproc.ErrIncomplete)
			} else {
				inspection, stop := context.WithTimeout(context.Background(), ownedproc.InspectionBudget)
				cancelErr = errors.Join(cancelErr, registry.Signal(inspection, command.Process.Pid, force))
				stop()
			}
			timer := time.NewTimer(200*time.Millisecond)
			select {
			case waitErr = <-wait:
				timer.Stop()
				output, stdoutErr := stdout.snapshot()
				_, stderrErr := stderr.snapshot()
				return output, errors.Join(ownershipErr, waitErr, cancelErr, stdoutErr, stderrErr)
			case <-timer.C:
				waitErr = ownedproc.ErrIncomplete
			}
		}
	}
	output, stdoutErr := stdout.snapshot()
	_, stderrErr := stderr.snapshot()
	return output, errors.Join(ownershipErr, waitErr, cancelErr, stdoutErr, stderrErr, ctx.Err())
}

type CommandObservation struct {
	Name string `json:"name"`
	Args []string `json:"args"`
	Output []byte `json:"output_base64"`
	Started time.Time `json:"started"`
	Elapsed time.Duration `json:"elapsed_ns"`
	Error string `json:"error,omitempty"`
}

type Runner struct {
	mu sync.Mutex
	records []CommandObservation
	bytes int
	failed bool
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
