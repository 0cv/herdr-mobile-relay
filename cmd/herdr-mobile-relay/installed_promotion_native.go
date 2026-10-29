package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/config"
	"github.com/0cv/herdr-mobile-relay/internal/installedpromotion"
)

// The driver observes both fixed labels, not a PID, an inactive definition or
// a matching route. It cannot choose an installation or substitute publication.
type nativeCLIFresh struct {
	driver                         func(context.Context, string) error
	definition, legacy, executable string
	claim                          string
	claimed                        bool
	digest                         [32]byte
	published                      bool
}

func (n *nativeCLIFresh) PinnedExecutable() string {
	if n == nil {
		return ""
	}
	return n.executable
}

func (n *nativeCLIFresh) Fresh(ctx context.Context) error {
	if n == nil || ctx == nil || ctx.Err() != nil || n.driver == nil || n.published || n.definition == "" || n.legacy == "" || n.executable == "" || n.claim == "" {
		return errPromotion
	}
	for _, path := range []string{n.definition, n.legacy} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return errPromotion
		}
	}
	if !n.claimed {
		if _, err := os.Lstat(n.claim); !errors.Is(err, os.ErrNotExist) {
			return errPromotion
		}
	}
	return n.driver(ctx, "fresh")
}

// The exact native resource claim is shared with existing shell installers.
// It is retained after publication and on every uncertain outcome: neither an
// old replacement transaction nor a new setup may overlap or adopt it.
func (n *nativeCLIFresh) Claim(ctx context.Context) (string, error) {
	if n.Fresh(ctx) != nil || n.claimed {
		return "", errPromotion
	}
	parent := filepath.Dir(n.claim)
	if err := promotionClaimParent(parent); err != nil {
		return "", errPromotion
	}
	if err := os.Mkdir(n.claim, 0700); err != nil {
		return "", errPromotion
	}
	n.claimed = true
	d, err := os.Open(parent)
	if err != nil {
		return "", errPromotion
	}
	e := d.Sync()
	ce := d.Close()
	if e != nil || ce != nil || ctx.Err() != nil {
		return "", errPromotion
	}
	return n.claim, nil
}

// Admission of the exact native parent happens after consent but before route
// publication, including when promotion is invoked without the menu.
func (n *nativeCLIFresh) PrepareParent(ctx context.Context) error {
	if n == nil || ctx == nil || ctx.Err() != nil || n.Fresh(ctx) != nil ||
		filepath.Dir(n.definition) != filepath.Dir(n.legacy) ||
		promotionClaimParent(filepath.Dir(n.definition)) != nil || ctx.Err() != nil {
		return errPromotion
	}
	return n.Fresh(ctx)
}

func (n *nativeCLIFresh) Publish(ctx context.Context, selected config.InstalledConfig, ref installedpromotion.Reference) error {
	if n.Fresh(ctx) != nil || !promotionHex(ref.Generation, 32) || !promotionHex(ref.Digest, 64) {
		return errPromotion
	}
	cfg := selected.Snapshot()
	if selected.Validate(&cfg) != nil || cfg.ConfigHome == "" || cfg.InstanceID == "" || cfg.ReleaseRoot == "" || !filepath.IsAbs(n.executable) || filepath.Clean(n.executable) != n.executable || !strings.HasPrefix(n.executable, cfg.ReleaseRoot+string(filepath.Separator)) || strings.ContainsAny(n.executable, "\n\r\x00") {
		return errPromotion
	}
	info, err := os.Lstat(n.executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return errPromotion
	}
	body, err := promotionDefinition(n.executable, cfg, ref)
	if err != nil || len(body) > 4096 || n.Fresh(ctx) != nil {
		return errPromotion
	}
	// The directory must already be an owned, non-writable-by-bystanders
	// native user-service directory. Never create or chmod it here.
	parent := filepath.Dir(n.definition)
	d, err := os.Open(parent)
	if err != nil {
		return errPromotion
	}
	defer d.Close()
	stat, err := d.Stat()
	if err != nil || !stat.IsDir() || stat.Mode().Perm()&0022 != 0 || !ownedPromotionDir(stat) || n.Fresh(ctx) != nil {
		return errPromotion
	}
	// O_EXCL is fresh publication: no replacement, no rollback, no adoption.
	f, err := os.OpenFile(n.definition, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errPromotion
	}
	n.published = true // sticky even on partial write/close/sync
	num, we := f.WriteString(body)
	se := f.Sync()
	ce := f.Close()
	de := d.Sync()
	if num != len(body) || errors.Join(we, se, ce, de) != nil || ctx.Err() != nil {
		return errPromotion
	}
	n.digest = sha256.Sum256([]byte(body))
	return nil
}

// Both publication and explicit owned transitions use this exact pinned argv.
func promotionDefinition(binary string, cfg config.Config, ref installedpromotion.Reference) (string, error) {
	if !promotionHex(ref.Generation, 32) || !promotionHex(ref.Digest, 64) || !filepath.IsAbs(binary) || filepath.Clean(binary) != binary || !strings.HasPrefix(binary, cfg.ReleaseRoot+string(filepath.Separator)) || strings.ContainsAny(binary, "\n\r\x00") {
		return "", errPromotion
	}
	args := []string{binary, "serve-installed", "--selection", ref.Generation, ref.Digest, cfg.ConfigHome, cfg.InstanceID}
	var body string
	if runtime.GOOS == "darwin" {
		for _, a := range args {
			if strings.ContainsAny(a, "&<>\"'\n\r") {
				return "", errPromotion
			}
		}
		body = fmt.Sprintf("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\"><dict><key>Label</key><string>com.herdr-mobile-relay.service</string><key>ProgramArguments</key><array><string>%s</string><string>serve-installed</string><string>--selection</string><string>%s</string><string>%s</string><string>%s</string><string>%s</string></array><key>RunAtLoad</key><true/><key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict></dict></plist>\n", args[0], args[3], args[4], args[5], args[6])
	} else if runtime.GOOS == "linux" {
		for _, a := range args {
			if strings.ContainsAny(a, "\n\r\x00%\\\" ;#") {
				return "", errPromotion
			}
		}
		body = fmt.Sprintf("[Unit]\nDescription=Herdr Mobile Relay installed CLI\n[Service]\nType=simple\nExecStart=%s serve-installed --selection %s %s %s %s\nRestart=on-failure\nRestartPreventExitStatus=69 78\n[Install]\nWantedBy=default.target\n", args[0], args[3], args[4], args[5], args[6])
	} else {
		return "", errPromotion
	}
	if len(body) > 4096 {
		return "", errPromotion
	}
	return body, nil
}

func (n *nativeCLIFresh) Activate(ctx context.Context) error {
	if n == nil || !n.published || n.digest == ([32]byte{}) || ctx == nil || ctx.Err() != nil {
		return errPromotion
	}
	b, err := os.ReadFile(n.definition)
	if err != nil || sha256.Sum256(b) != n.digest || len(b) > 4096 {
		return errPromotion
	}
	// The driver reobserves both labels immediately before the one activation;
	// a nonzero/ambiguous result retains the definition and all route evidence.
	return n.driver(ctx, "activate")
}

// Prospective parents are owned, real directories; never follow a symlink or
// chmod an existing directory to make a promotion claim appear private.
func promotionClaimParent(parent string) error {
	home, err := os.UserHomeDir()
	if err != nil || filepath.Clean(home) != home || !filepath.IsAbs(home) {
		return errPromotion
	}
	rel, err := filepath.Rel(home, parent)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return errPromotion
	}
	current := home
	for _, part := range append([]string{""}, strings.Split(rel, string(filepath.Separator))...) {
		if part != "" {
			current = filepath.Join(current, part)
			if err := os.Mkdir(current, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return errPromotion
			}
		}
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 || !ownedPromotionDir(info) {
			return errPromotion
		}
	}
	return nil
}

func promotionHex(s string, size int) bool {
	if len(s) != size {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' && c < 'a' || c > 'f' {
			return false
		}
	}
	return true
}
