package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/installedpromotion"
	"github.com/0cv/herdr-mobile-relay/internal/release"
	"github.com/0cv/herdr-mobile-relay/internal/tailscalecli"
)

// No mutable current pointer participates in service admission. An incomplete
// transition fences resume and further lifecycle operations until recovery.
type promotionTransition struct {
	Version       int    `json:"version"`
	Phase         string `json:"phase"`
	Old           string `json:"old"`
	New           string `json:"new"`
	OldHash       string `json:"old_hash"`
	NewHash       string `json:"new_hash"`
	OperationID   string `json:"operation_id,omitempty"`
	ReservationID string `json:"reservation_id,omitempty"`
	NodeID        string `json:"node_id,omitempty"`
}

func promotionClaimPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errPromotion
	}
	return filepath.Join(home, ".local/state/herdr-mobile-relay/recovery/.herdr-native-claim-v1"), nil
}

func promotionDefinitionPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errPromotion
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library/LaunchAgents/com.herdr-mobile-relay.service.plist"), nil
	case "linux":
		return filepath.Join(home, ".config/systemd/user/herdr-mobile-relay.service"), nil
	default:
		return "", errPromotion
	}
}

func promotionOwnedFile(path string, max int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm() != 0600 || !ownedPromotionDir(before) || before.Size() < 1 || before.Size() > max {
		return nil, errPromotion
	}
	data, err := os.ReadFile(path)
	after, e := os.Lstat(path)
	if err != nil || e != nil || !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() || int64(len(data)) != before.Size() {
		return nil, errPromotion
	}
	return data, nil
}

func promotionRecordWrite(root, name string, record promotionTransition) error {
	data, err := json.Marshal(record)
	if err != nil || len(data) > 4096 {
		return errPromotion
	}
	f, err := os.OpenFile(filepath.Join(root, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errPromotion
	}
	n, e := f.Write(data)
	e = errors.Join(e, f.Sync(), f.Close())
	d, de := os.Open(root)
	if de != nil {
		return errPromotion
	}
	e = errors.Join(e, d.Sync(), d.Close())
	if n != len(data) || e != nil {
		return errPromotion
	}
	return nil
}

// The journal's first pin is the original capsule's executable. A complete
// intent+outcome pair alone can authorize a later pin. A removal intent fences
// all starts even if the manager/route outcome becomes uncertain.
func promotionCurrentPin(root string, ref installedpromotion.Reference, allowRemoval ...bool) (string, error) {
	data, err := promotionOwnedFile(filepath.Join(root, "phase-a"), 4096)
	if err != nil {
		return "", errPromotion
	}
	var prepared promotionRecord
	if json.Unmarshal(data, &prepared) != nil || prepared.Version != 1 || prepared.Phase != "prepared" || prepared.Generation != ref.Generation || prepared.Digest != ref.Digest || !promotionHex(prepared.BinaryHash, 64) {
		return "", errPromotion
	}
	pin := prepared.Executable
	if _, err := os.Lstat(filepath.Join(root, "removal-intent")); !errors.Is(err, os.ErrNotExist) && (len(allowRemoval) == 0 || !allowRemoval[0]) {
		return "", errPromotion
	}
	expectedHash := prepared.BinaryHash
	for i := 0; i < 32; i++ {
		prefix := filepath.Join(root, "update-"+string([]byte{'a' + byte(i)}))
		intentData, e := promotionOwnedFile(prefix+"-intent", 4096)
		if e != nil {
			if _, missing := os.Lstat(prefix + "-intent"); errors.Is(missing, os.ErrNotExist) {
				if _, stray := os.Lstat(prefix + "-result"); errors.Is(stray, os.ErrNotExist) {
					hash, e := promotionBinaryHash(pin)
					if e == nil && hash == expectedHash {
						return pin, nil
					}
				}
			}
			return "", errPromotion
		}
		var intent promotionTransition
		if json.Unmarshal(intentData, &intent) != nil || intent.Version != 1 || intent.Phase != "intent" || intent.Old != pin || intent.New == pin || intent.OldHash != expectedHash || !promotionHex(intent.OldHash, 64) || !promotionHex(intent.NewHash, 64) {
			return "", errPromotion
		}
		resultData, e := promotionOwnedFile(prefix+"-result", 4096)
		if e != nil {
			return "", errPromotion
		}
		var result promotionTransition
		if json.Unmarshal(resultData, &result) != nil || result.Version != intent.Version || result.Old != intent.Old || result.New != intent.New || result.OldHash != intent.OldHash || result.NewHash != intent.NewHash {
			return "", errPromotion
		}
		switch result.Phase {
		case "committed":
			pin, expectedHash = intent.New, intent.NewHash
		case "rolled-back":
		default:
			return "", errPromotion
		}
	}
	return "", errPromotion
}

func promotionDefinitionAt(path, expected string) error {
	data, err := promotionOwnedFile(path, 4096)
	if err != nil || string(data) != expected {
		return errPromotion
	}
	return nil
}

func promotionOwnedBinding(selected config.InstalledConfig, ref installedpromotion.Reference, native *nativeCLIFresh, allowRemoval ...bool) (string, error) {
	cfg := selected.Snapshot()
	if selected.Validate(&cfg) != nil || native == nil || native.driver == nil {
		return "", errPromotion
	}
	root, err := promotionClaimPath()
	if err != nil || native.claim != root {
		return "", errPromotion
	}
	dir, err := os.Lstat(root)
	if err != nil || !dir.IsDir() || dir.Mode().Perm() != 0700 || !ownedPromotionDir(dir) || promotionAdmission(selected, ref, false, allowRemoval...) != nil {
		return "", errPromotion
	}
	pin, err := promotionCurrentPin(root, ref, allowRemoval...)
	if err != nil {
		return "", errPromotion
	}
	// Do not run an owned mutation from an arbitrary binary. The current
	// pointer may lag the pin, but its physical release must be complete.
	source, err := installedPinnedExecutable()
	if err != nil || filepath.Base(source) != "herdr-mobile-relay" || !strings.HasPrefix(source, cfg.ReleaseRoot+string(filepath.Separator)) {
		return "", errPromotion
	}
	physical, err := filepath.EvalSymlinks(source)
	if err != nil || physical != source {
		return "", errPromotion
	}
	if _, err := release.Verify(filepath.Dir(source), release.CurrentTarget()); err != nil {
		return "", errPromotion
	}
	body, err := promotionDefinition(pin, cfg, ref)
	if err != nil || promotionDefinitionAt(native.definition, body) != nil {
		return "", errPromotion
	}
	if _, err := os.Lstat(native.legacy); !errors.Is(err, os.ErrNotExist) {
		return "", errPromotion
	}
	return pin, nil
}

func promotionReference(root string) (installedpromotion.Reference, error) {
	data, err := promotionOwnedFile(filepath.Join(root, "phase-a"), 4096)
	if err != nil {
		return installedpromotion.Reference{}, errPromotion
	}
	var record promotionRecord
	if json.Unmarshal(data, &record) != nil || !promotionHex(record.Generation, 32) || !promotionHex(record.Digest, 64) {
		return installedpromotion.Reference{}, errPromotion
	}
	return installedpromotion.Reference{Generation: record.Generation, Digest: record.Digest}, nil
}

func promotionRouteReady(ctx context.Context, selected config.InstalledConfig, m *tailscalecli.Manager) error {
	cfg := selected.Snapshot()
	port, err := promotionHTTPSPort(cfg.TailscaleCLIOrigin)
	if err != nil {
		return errPromotion
	}
	route, err := m.VerifyRegisteredRoute(ctx, cfg.TailscaleCLIScope, cfg.InstanceID, cfg.TailscaleCLIOrigin, port, cfg.Port)
	if err != nil || route.JournalState != tailscalecli.StateRegistered || route.Readiness != tailscalecli.ReadinessReady || !promotionRouteQualification(m, route) {
		return errPromotion
	}
	return nil
}

// Replace only the definition we just observed, and retain a unique private
// stage on any failure. An ambiguous rename/sync is NOT rollback authority.
func promotionReplaceDefinition(ctx context.Context, path, old, next string) error {
	if ctx.Err() != nil || promotionDefinitionAt(path, old) != nil {
		return errPromotion
	}
	dir := filepath.Dir(path)
	f, err := os.OpenFile(path+".promotion-stage", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errPromotion
	}
	n, e := f.WriteString(next)
	e = errors.Join(e, f.Sync(), f.Close())
	if n != len(next) || e != nil {
		return errPromotion
	}
	if ctx.Err() != nil || promotionDefinitionAt(path, old) != nil {
		return errPromotion
	}
	if err := os.Rename(path+".promotion-stage", path); err != nil {
		return errPromotion
	}
	d, err := os.Open(dir)
	if err != nil {
		return errPromotion
	}
	err = errors.Join(d.Sync(), d.Close())
	if err != nil || ctx.Err() != nil || promotionDefinitionAt(path, next) != nil {
		return errPromotion
	}
	return nil
}

func promotionTransitionSlot(root string) (string, error) {
	for i := 0; i < 32; i++ {
		name := "update-" + string([]byte{'a' + byte(i)})
		_, e := os.Lstat(filepath.Join(root, name+"-intent"))
		if errors.Is(e, os.ErrNotExist) {
			return name, nil
		}
		if e != nil {
			return "", errPromotion
		}
	}
	return "", errPromotion
}

// Explicit update uses the retained selection and manager. It never prepares a
// new capsule, publishes a route or touches app/device stores. Rollback is only
// attempted after a *confirmed* second quiesce and exact new-definition check.
func updateInstalledPromotion(ctx context.Context, selected config.InstalledConfig, m *tailscalecli.Manager, native *nativeCLIFresh, candidate string) error {
	if ctx == nil || ctx.Err() != nil || m == nil || promotionRouteReady(ctx, selected, m) != nil {
		return errPromotion
	}
	root, err := promotionClaimPath()
	if err != nil {
		return errPromotion
	}
	ref, err := promotionReference(root)
	if err != nil {
		return errPromotion
	}
	old, err := promotionOwnedBinding(selected, ref, native)
	if err != nil || old == candidate {
		return errPromotion
	}
	cfg := selected.Snapshot()
	if filepath.Base(candidate) != "herdr-mobile-relay" || !filepath.IsAbs(candidate) || filepath.Clean(candidate) != candidate || !strings.HasPrefix(candidate, cfg.ReleaseRoot+string(filepath.Separator)) || strings.Contains(candidate, "/current/") {
		return errPromotion
	}
	physicalCandidate, e := filepath.EvalSymlinks(candidate)
	if e != nil || physicalCandidate != candidate {
		return errPromotion
	}
	candidateInfo, e := os.Lstat(candidate)
	if e != nil || !candidateInfo.Mode().IsRegular() || !ownedPromotionDir(candidateInfo) || candidateInfo.Mode().Perm()&0022 != 0 {
		return errPromotion
	}
	oldManifest, e1 := release.Verify(filepath.Dir(old), release.CurrentTarget())
	newManifest, e2 := release.Verify(filepath.Dir(candidate), release.CurrentTarget())
	if e1 != nil || e2 != nil || release.ValidateUpgradeCompatibility(oldManifest, newManifest) != nil || promotionBackendEligible(cfg, newManifest) != nil || oldManifest.Version == newManifest.Version && oldManifest.Revision == newManifest.Revision {
		return errPromotion
	}
	oldHash, e1 := promotionBinaryHash(old)
	newHash, e2 := promotionBinaryHash(candidate)
	if e1 != nil || e2 != nil || oldHash == newHash {
		return errPromotion
	}
	oldBody, e1 := promotionDefinition(old, cfg, ref)
	newBody, e2 := promotionDefinition(candidate, cfg, ref)
	if e1 != nil || e2 != nil {
		return errPromotion
	}
	slot, err := promotionTransitionSlot(root)
	if err != nil {
		return errPromotion
	}
	entry := promotionTransition{Version: 1, Phase: "intent", Old: old, New: candidate, OldHash: oldHash, NewHash: newHash}
	if promotionRecordWrite(root, slot+"-intent", entry) != nil {
		return errPromotion
	}
	result := func(phase string) error {
		entry.Phase = phase
		return promotionRecordWrite(root, slot+"-result", entry)
	}
	// Quiescence is confirmed by the native adapter before replacing argv.
	if native.driver(ctx, "quiesce") != nil || promotionDefinitionAt(native.definition, oldBody) != nil {
		return errPromotion
	}
	if ctx.Err() != nil || promotionReplaceDefinition(ctx, native.definition, oldBody, newBody) != nil {
		return errPromotion
	}
	if native.driver(ctx, "start") == nil && promotionBackendReady(ctx, cfg, newManifest) == nil && promotionRouteReady(ctx, selected, m) == nil && promotionDefinitionAt(native.definition, newBody) == nil {
		if hash, e := promotionBinaryHash(candidate); e == nil && hash == newHash && result("committed") == nil {
			return nil
		}
	}
	// The previous service may only be restored after a proven stop of this
	// attempt's replacement. Never overwrite a bystander or a moving definition.
	if native.driver(ctx, "quiesce") != nil || promotionDefinitionAt(native.definition, newBody) != nil {
		return errPromotion
	}
	if promotionReplaceDefinition(ctx, native.definition, newBody, oldBody) != nil || native.driver(ctx, "start") != nil || promotionBackendReady(ctx, cfg, oldManifest) != nil || promotionRouteReady(ctx, selected, m) != nil || promotionDefinitionAt(native.definition, oldBody) != nil || result("rolled-back") != nil {
		return errPromotion
	}
	return errPromotion // the requested update failed, even when old service recovered
}

func runInstalledLifecycleCommand(operation string, args []string) (int, error) {
	if operation == "update" && len(args) != 1 || operation == "remove" && len(args) != 1 || operation == "remove-missing" && len(args) != 4 || operation == "stop" && len(args) != 0 {
		return 2, errPromotion
	}
	cfg, err := config.LoadInstalled()
	if err != nil {
		return 1, errPromotion
	}
	selected, err := config.BindInstalledCLI(cfg)
	if err != nil {
		return 1, errPromotion
	}
	m, err := installedCommandManager(selected) // closed production gate
	if err != nil {
		return 1, errPromotion
	}
	n, err := installedPromotionNative(selected)
	if err != nil {
		return 1, errPromotion
	}
	native, ok := n.(*nativeCLIFresh)
	if !ok {
		return 1, errPromotion
	}
	ctx, cancel := installedCommandContext()
	defer cancel()
	switch operation {
	case "update":
		err = updateInstalledPromotion(ctx, selected, m, native, args[0])
	case "remove":
		err = removeInstalledPromotion(ctx, selected, m, native, args[0])
	case "remove-missing":
		err = removeMissingInstalledPromotion(ctx, selected, m, native, args)
	case "stop":
		err = stopInstalledPromotion(ctx, selected, native)
	default:
		return 2, errPromotion
	}
	if err != nil {
		return 1, errPromotion
	}
	return 0, nil
}

// A routine stop retains the exact route/journal and refuses to touch a
// name-matched but unowned native definition. No Serve operation is dispatched.
func stopInstalledPromotion(ctx context.Context, selected config.InstalledConfig, native *nativeCLIFresh) error {
	if ctx == nil || ctx.Err() != nil {
		return errPromotion
	}
	root, err := promotionClaimPath()
	if err != nil {
		return errPromotion
	}
	ref, err := promotionReference(root)
	if err != nil {
		return errPromotion
	}
	if _, err := promotionOwnedBinding(selected, ref, native); err != nil {
		return errPromotion
	}
	if native.driver(ctx, "quiesce") != nil || native.driver(ctx, "assert-stopped") != nil || ctx.Err() != nil {
		return errPromotion
	}
	return nil
}

func removeInstalledPromotion(ctx context.Context, selected config.InstalledConfig, m *tailscalecli.Manager, native *nativeCLIFresh, node string) error {
	if ctx == nil || ctx.Err() != nil || m == nil || node == "" || promotionRouteReady(ctx, selected, m) != nil {
		return errPromotion
	}
	root, err := promotionClaimPath()
	if err != nil {
		return errPromotion
	}
	ref, err := promotionReference(root)
	if err != nil {
		return errPromotion
	}
	pin, err := promotionOwnedBinding(selected, ref, native)
	if err != nil {
		return errPromotion
	}
	body, err := promotionDefinition(pin, selected.Snapshot(), ref)
	if err != nil {
		return errPromotion
	}
	if promotionRecordWrite(root, "removal-intent", promotionTransition{Version: 1, Phase: "remove"}) != nil {
		return errPromotion
	}
	if native.driver(ctx, "quiesce") != nil || native.driver(ctx, "assert-stopped") != nil {
		return errPromotion
	}
	cfg := selected.Snapshot()
	port, err := promotionHTTPSPort(cfg.TailscaleCLIOrigin)
	if err != nil {
		return errPromotion
	}
	if err := m.Unpublish(ctx, tailscalecli.Consent{Accepted: true, Scope: cfg.TailscaleCLIScope, NodeID: node, Origin: cfg.TailscaleCLIOrigin, HTTPSPort: port, BackendPort: cfg.Port, RouteRemovalAccepted: true, CheckToWriteRaceAccepted: true, NoRemoteDrainAccepted: true}); err != nil {
		return errPromotion
	}
	// Unknown manager outcomes retain the claim and definition.
	return promotionFinishRemoval(ctx, native, root, body)
}

// The absence path does not invoke Unpublish. The exact operation and optional
// reservation are confirmed before intent and again inside AbandonMissing.
// An interrupted intent may only continue with that same consent and tuple.
func removeMissingInstalledPromotion(ctx context.Context, selected config.InstalledConfig, m *tailscalecli.Manager, native *nativeCLIFresh, args []string) error {
	if ctx == nil || ctx.Err() != nil || m == nil || args[0] == "" || args[1] == "" || args[3] != "I-ACCEPT-ABSENCE-REMOVAL" {
		return errPromotion
	}
	cfg := selected.Snapshot()
	port, err := promotionHTTPSPort(cfg.TailscaleCLIOrigin)
	if err != nil {
		return errPromotion
	}
	report, _ := m.Recover(ctx, cfg.TailscaleCLIScope, cfg.InstanceID, cfg.TailscaleCLIOrigin, port, cfg.Port)
	if report.OperationID != args[1] || report.ReservationAttemptID != args[2] ||
		(report.Route.JournalState != tailscalecli.StateRegistered && report.Route.JournalState != tailscalecli.StateReconciledAbsent) ||
		report.Observation != "selected-listener-absent" {
		return errPromotion
	}
	root, err := promotionClaimPath()
	if err != nil {
		return errPromotion
	}
	ref, err := promotionReference(root)
	if err != nil {
		return errPromotion
	}
	intent := promotionTransition{Version: 1, Phase: "remove-missing", OperationID: args[1], ReservationID: args[2], NodeID: args[0]}
	intentPath := filepath.Join(root, "removal-intent")
	data, intentErr := promotionOwnedFile(intentPath, 4096)
	continuing := intentErr == nil
	if continuing {
		var prior promotionTransition
		if json.Unmarshal(data, &prior) != nil || prior != intent {
			return errPromotion
		}
	} else if _, e := os.Lstat(intentPath); !errors.Is(e, os.ErrNotExist) {
		return errPromotion
	}
	pin, err := promotionOwnedBinding(selected, ref, native, continuing)
	if err != nil {
		return errPromotion
	}
	consent := tailscalecli.Consent{
		Accepted: true, Scope: cfg.TailscaleCLIScope, NodeID: args[0], Origin: cfg.TailscaleCLIOrigin,
		HTTPSPort: port, BackendPort: cfg.Port, RecoveryAccepted: true, RecoveryObservation: "absent",
		OperationID: args[1], ReservationID: args[2],
	}
	if m.CheckAbandonMissingConsent(cfg.TailscaleCLIScope, cfg.InstanceID, cfg.TailscaleCLIOrigin, port, cfg.Port, consent) != nil {
		return errPromotion
	}
	body, err := promotionDefinition(pin, cfg, ref)
	if err != nil || native.driver(ctx, "quiesce") != nil || native.driver(ctx, "assert-stopped") != nil || ctx.Err() != nil {
		return errPromotion
	}
	if !continuing && promotionRecordWrite(root, "removal-intent", intent) != nil {
		return errPromotion
	}
	if err := m.AbandonMissing(ctx, cfg.TailscaleCLIScope, cfg.InstanceID, cfg.TailscaleCLIOrigin, port, cfg.Port, consent); err != nil {
		return errPromotion
	}
	return promotionFinishRemoval(ctx, native, root, body)
}

// Both removals retire only the pinned definition after the manager has
// acknowledged scoped unpublication or proved exact absence.
func promotionFinishRemoval(ctx context.Context, native *nativeCLIFresh, root, body string) error {
	if native.driver(ctx, "assert-stopped") != nil || promotionDefinitionAt(native.definition, body) != nil || native.driver(ctx, "remove") != nil || promotionDefinitionAt(native.definition, body) != nil || ctx.Err() != nil {
		return errPromotion
	}
	if err := os.Remove(native.definition); err != nil {
		return errPromotion
	}
	d, err := os.Open(filepath.Dir(native.definition))
	if err != nil {
		return errPromotion
	}
	if errors.Join(d.Sync(), d.Close()) != nil || native.driver(ctx, "retire") != nil {
		return errPromotion
	}
	// Release only our finite claim files after acknowledged route removal and
	// native retirement. Never recursively remove a replaced or extra child.
	return promotionRetireClaim(root)
}

func promotionRetireClaim(root string) error {
	dir, err := os.Open(root)
	if err != nil {
		return errPromotion
	}
	defer dir.Close()
	before, err := dir.Stat()
	if err != nil || !before.IsDir() || before.Mode().Perm() != 0700 || !ownedPromotionDir(before) {
		return errPromotion
	}
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return errPromotion
	}
	identities := make(map[string]os.FileInfo, len(names))
	for _, name := range names {
		valid := name == "removal-intent"
		if len(name) == 7 && strings.HasPrefix(name, "phase-") && name[6] >= 'a' && name[6] <= 'j' {
			valid = true
		}
		if len(name) >= 15 && strings.HasPrefix(name, "update-") && name[7] >= 'a' && name[7] <= 'z' && (name[8:] == "-intent" || name[8:] == "-result") {
			valid = true
		}
		if !valid {
			return errPromotion
		}
		path := filepath.Join(root, name)
		info, e := os.Lstat(path)
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ownedPromotionDir(info) {
			return errPromotion
		}
		identities[name] = info
	}
	if _, ok := identities["removal-intent"]; !ok {
		return errPromotion
	}
	for _, name := range names {
		path := filepath.Join(root, name)
		info, e := os.Lstat(path)
		if e != nil || !os.SameFile(identities[name], info) || info.Size() != identities[name].Size() || info.ModTime() != identities[name].ModTime() {
			return errPromotion
		}
		if os.Remove(path) != nil {
			return errPromotion
		}
	}
	if dir.Sync() != nil {
		return errPromotion
	}
	current, err := os.Lstat(root)
	if err != nil || !os.SameFile(before, current) {
		return errPromotion
	}
	if os.Remove(root) != nil || dir.Close() != nil {
		return errPromotion
	}
	parent, err := os.Open(filepath.Dir(root))
	if err != nil {
		return errPromotion
	}
	if errors.Join(parent.Sync(), parent.Close()) != nil {
		return errPromotion
	}
	return nil
}

// Avoid floating point/implicit port defaults in route matching.
func promotionHTTPSPort(origin string) (int, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" {
		return 0, errPromotion
	}
	if u.Port() == "" {
		return 443, nil
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return 0, errPromotion
	}
	return port, nil
}
