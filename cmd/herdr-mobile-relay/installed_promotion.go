package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/app"
	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/installedpromotion"
	"github.com/0cv/herdr-mobile-relay/internal/localcontrol"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

var errPromotion = errors.New("installed promotion refused; retain any promotion and route evidence for explicit recovery")

// The journal is append-only. An interrupted attempt is never interpreted as
// permission to retry setup, publication, definition installation or activation.
// Its directory is the native-resource claim shared with old installers.
type promotionJournal struct {
	root       string
	ref        installedpromotion.Reference
	executable string
	binaryHash string
	step       int
}

type promotionRecord struct {
	Version     int                               `json:"version"`
	Phase       string                            `json:"phase"`
	Generation  string                            `json:"generation"`
	Digest      string                            `json:"digest"`
	Publication *tailscalecli.PublicationSnapshot `json:"publication,omitempty"`
	Executable  string                            `json:"executable,omitempty"`
	BinaryHash  string                            `json:"binary_hash,omitempty"`
}

// Resume requires a complete, privately retained setup/publication/definition
// intent, not merely a valid prepared capsule or a matching Serve observation.
// Activation intent is durable before the native manager can start the service.
func promotionBinaryHash(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 128<<20 {
		return "", errPromotion
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errPromotion
	}
	opened, e := f.Stat()
	if e != nil || !os.SameFile(info, opened) {
		_ = f.Close()
		return "", errPromotion
	}
	h := sha256.New()
	count, e := io.CopyN(h, f, info.Size())
	end, statErr := f.Stat()
	closeErr := f.Close()
	if e != nil || count != info.Size() || statErr != nil || !os.SameFile(info, end) || end.Size() != info.Size() || end.ModTime() != info.ModTime() || closeErr != nil {
		return "", errPromotion
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func promotionResumeAdmitted(selected config.InstalledConfig, ref installedpromotion.Reference) error {
	return promotionAdmission(selected, ref, true)
}

// Lifecycle commands may run from another *verified* release, but STRICT
// service resume always requires the executing physical binary to be the pin.
func promotionAdmission(selected config.InstalledConfig, ref installedpromotion.Reference, requirePin bool, allowRemoval ...bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return errPromotion
	}
	root := filepath.Join(home, ".local/state/herdr-mobile-relay/recovery/.herdr-native-claim-v1")
	dir, err := os.Lstat(root)
	if err != nil || !dir.IsDir() || dir.Mode().Perm() != 0700 || !ownedPromotionDir(dir) {
		return errPromotion
	}
	phases := []string{"prepared", "setup-entering", "publish-intent", "publication-acknowledged", "setup-stopping", "setup-joined", "definition-intent", "definition-published", "activation-intent"}
	cfg := selected.Snapshot()
	origin, err := url.Parse(cfg.TailscaleCLIOrigin)
	if err != nil {
		return errPromotion
	}
	port := 443
	if origin.Port() != "" {
		port, err = strconv.Atoi(origin.Port())
		if err != nil {
			return errPromotion
		}
	}
	for index, phase := range phases {
		path := filepath.Join(root, "phase-"+string(rune('a'+index)))
		info, e := os.Lstat(path)
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() < 1 || info.Size() > 4096 {
			return errPromotion
		}
		file, e := os.Open(path)
		if e != nil {
			return errPromotion
		}
		opened, statErr := file.Stat()
		var data []byte
		if statErr == nil && os.SameFile(info, opened) {
			data, e = io.ReadAll(io.LimitReader(file, 4097))
		} else {
			e = errPromotion
		}
		closeErr := file.Close()
		if e != nil || closeErr != nil || len(data) > 4096 {
			return errPromotion
		}
		var record promotionRecord
		if json.Unmarshal(data, &record) != nil || record.Version != 1 || record.Phase != phase || record.Generation != ref.Generation || record.Digest != ref.Digest {
			return errPromotion
		}
		if index == 0 {
			if !filepath.IsAbs(record.Executable) || !strings.HasPrefix(record.Executable, cfg.ReleaseRoot+string(filepath.Separator)) || !promotionHex(record.BinaryHash, 64) {
				return errPromotion
			}
		} else if record.Executable != "" || record.BinaryHash != "" {
			return errPromotion
		}
		if index == 3 {
			if record.Publication == nil || record.Publication.Installation != cfg.InstanceID || record.Publication.Origin != cfg.TailscaleCLIOrigin || record.Publication.HTTPSPort != port || record.Publication.BackendPort != cfg.Port {
				return errPromotion
			}
		} else if record.Publication != nil {
			return errPromotion
		}
	}
	pin, err := promotionCurrentPin(root, ref, allowRemoval...)
	if err != nil {
		return errPromotion
	}
	if requirePin {
		binary, err := installedPinnedExecutable()
		if err != nil || pin != binary {
			return errPromotion
		}
	}
	hash, err := promotionBinaryHash(pin)
	if err != nil {
		return errPromotion
	}
	if pin == "" || hash == "" {
		return errPromotion
	}
	body, err := promotionDefinition(pin, cfg, ref)
	if err != nil {
		return errPromotion
	}
	definition, err := promotionDefinitionPath()
	if err != nil || promotionDefinitionAt(definition, body) != nil {
		return errPromotion
	}
	return nil
}

func newPromotionJournal(root string, ref installedpromotion.Reference, executable string) (*promotionJournal, error) {
	hash, err := promotionBinaryHash(executable)
	if err != nil {
		return nil, errPromotion
	}
	return &promotionJournal{root: root, ref: ref, executable: executable, binaryHash: hash}, nil
}

func (j *promotionJournal) record(ctx context.Context, phase string, ack *tailscalecli.PublicationSnapshot) error {
	if ctx == nil || ctx.Err() != nil || j.step >= 16 {
		return errPromotion
	}
	record := promotionRecord{Version: 1, Phase: phase, Generation: j.ref.Generation, Digest: j.ref.Digest, Publication: ack}
	if phase == "prepared" {
		record.Executable, record.BinaryHash = j.executable, j.binaryHash
	}
	b, err := json.Marshal(record)
	if err != nil || len(b) > 4096 {
		return errPromotion
	}
	path := filepath.Join(j.root, "phase-"+string(rune('a'+j.step)))
	j.step++ // a failed write is uncertain, not an unused slot
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errPromotion
	}
	n, writeErr := f.Write(b)
	syncErr := f.Sync()
	closeErr := f.Close()
	d, dirErr := os.Open(j.root)
	if dirErr != nil {
		return errPromotion
	}
	parentErr := d.Sync()
	closeDirErr := d.Close()
	if n != len(b) || errors.Join(writeErr, syncErr, closeErr, parentErr, closeDirErr) != nil || ctx.Err() != nil {
		return errPromotion
	}
	return nil
}

// External adapters observe finite native state and perform only named effects.
// The coordinator owns the fresh-only decisions and phase order, not the adapter.
type promotionNative interface {
	Fresh(context.Context) error
	PrepareParent(context.Context) error
	Claim(context.Context) (string, error)
	PinnedExecutable() string
	Publish(context.Context, config.InstalledConfig, installedpromotion.Reference) error
	Activate(context.Context) error
}

func runInstalledPromotionCommand() (int, error) {
	cfg, err := config.LoadInstalled()
	if err != nil {
		return 1, errPromotion
	}
	selected, err := config.BindInstalledCLI(cfg)
	if err != nil {
		return 1, errPromotion
	}
	manager, err := installedCommandManager(selected) // production gate before any effects
	if err != nil {
		return 1, errInstalledCLIUnavailable
	}
	ctx, stop := installedCommandContext()
	defer stop()
	native, err := installedPromotionNative(selected)
	if err != nil {
		return 1, errPromotion
	}
	logger := newRelayLogger(os.Stderr, cfg.LogFormat, cfg.LogLevel, stderrIsJournal(os.Stderr))
	if err := promoteInstalled(ctx, selected, manager, native, logger); err != nil {
		return 1, errPromotion
	}
	return 0, nil
}

func promoteInstalled(ctx context.Context, selected config.InstalledConfig, manager *tailscalecli.Manager, native promotionNative, logger *slog.Logger) error {
	if ctx == nil || ctx.Err() != nil || manager == nil || native == nil {
		return errPromotion
	}
	cfg := selected.Snapshot()
	if selected.Validate(&cfg) != nil || native.Fresh(ctx) != nil {
		return errPromotion
	}
	// The full original binding is persisted before setup can reserve a backend.
	prepared, err := installedpromotion.Prepare(ctx, selected, cfg.ConfigHome, cfg.InstanceID)
	if err != nil || prepared.Outcome != installedpromotion.Durable {
		return errPromotion
	}
	claim, err := native.Claim(ctx)
	if err != nil {
		return errPromotion
	}
	j, err := newPromotionJournal(claim, prepared.Reference, native.PinnedExecutable())
	if err != nil {
		return errPromotion
	}
	if j.record(ctx, "prepared", nil) != nil || j.record(ctx, "setup-entering", nil) != nil || native.Fresh(ctx) != nil {
		return errPromotion
	}
	setupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ack := make(chan tailscalecli.AcknowledgedPublication, 1)
	joined := make(chan error, 1)
	beforePublish := func(call context.Context) error {
		// The core reaches this callback only after exact operator consent. Do
		// not publish a persistent route before the native parent is admitted.
		if native.PrepareParent(call) != nil {
			return errPromotion
		}
		return j.record(call, "publish-intent", nil)
	}
	operator := func(call context.Context, core localcontrol.InstalledControl, credentials localcontrol.InstalledCredentials) error {
		// The endpoint owns the authenticated proposal/publish interaction. Only
		// the core can return the retained acknowledgement after real publication.
		endpointDone := make(chan error, 1)
		endpointCtx, end := context.WithCancel(call)
		go func() { endpointDone <- installedCommandEndpoint(endpointCtx, selected, core, credentials) }()
		endpointJoined := false
		defer func() {
			end()
			if !endpointJoined {
				<-endpointDone
			}
		}()
		tick := time.NewTicker(150 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-call.Done():
				return call.Err()
			case err := <-endpointDone:
				endpointJoined = true
				return errors.Join(err, errPromotion)
			case <-tick.C:
				value, err := core.AcknowledgedPublication(call)
				if err == nil {
					ack <- value
					<-call.Done()
					return call.Err()
				}
			}
		}
	}
	go func() {
		joined <- app.RunInstalledSetupPromotionCLI(setupCtx, selected, version, revision, logger, manager, beforePublish, operator)
	}()
	var publication tailscalecli.AcknowledgedPublication
	var early error
	alreadyJoined := false
	select {
	case publication = <-ack:
	case early = <-joined:
		alreadyJoined = true
	case <-ctx.Done():
	}
	// Always cancel and join the complete setup owner before any native write.
	if publication != (tailscalecli.AcknowledgedPublication{}) && ctx.Err() == nil {
		projection := publication.Snapshot()
		if projection.Installation == cfg.InstanceID && projection.Origin == cfg.TailscaleCLIOrigin {
			if err = j.record(ctx, "publication-acknowledged", &projection); err == nil {
				err = j.record(ctx, "setup-stopping", nil)
			}
		} else {
			err = errPromotion
		}
	}
	cancel()
	if !alreadyJoined {
		early = <-joined
	}
	if publication == (tailscalecli.AcknowledgedPublication{}) || err != nil || !installedCancellationOnly(early) || ctx.Err() != nil {
		return errPromotion
	}
	if j.record(ctx, "setup-joined", nil) != nil || selected.Validate(&cfg) != nil || native.Fresh(ctx) != nil {
		return errPromotion
	}
	if j.record(ctx, "definition-intent", nil) != nil || native.Fresh(ctx) != nil || native.Publish(ctx, selected, prepared.Reference) != nil {
		return errPromotion
	}
	if j.record(ctx, "definition-published", nil) != nil || j.record(ctx, "activation-intent", nil) != nil || native.Activate(ctx) != nil {
		return errPromotion
	}
	return j.record(ctx, "activation-attempted", nil)
}
