// Package agentkind resolves the name an agent reports onto one canonical kind.
package agentkind

import "strings"

const (
	Claude   = "claude"
	Codex    = "codex"
	Cursor   = "cursor"
	Grok     = "grok"
	Qoder    = "qoder"
	Pi       = "pi"
	OMP      = "omp"
	Kimi     = "kimi"
	OpenCode = "opencode"
	OMO      = "omo"
	Hermes   = "hermes"
	Agy      = "agy"
)

var aliases = map[string]string{
	"claude":         Claude,
	"claudecode":     Claude,
	"codex":          Codex,
	"openaicodex":    Codex,
	"codexcli":       Codex,
	"cursor":         Cursor,
	"cursoragent":    Cursor,
	"grok":           Grok,
	"grokbuild":      Grok,
	"qoder":          Qoder,
	"qodercli":       Qoder,
	"pi":             Pi,
	"picodingagent":  Pi,
	"omp":            OMP,
	"ohmypi":         OMP,
	"ompquestion":    OMP,
	"kimi":           Kimi,
	"kimicode":       Kimi,
	"kimicli":        Kimi,
	"kimicodecli":    Kimi,
	"opencode":       OpenCode,
	"omo":            OMO,
	"ohmyopencode":   OMO,
	"hermes":         Hermes,
	"hermesagent":    Hermes,
	"agy":            Agy,
	"antigravity":    Agy,
	"antigravitycli": Agy,
}

var separators = strings.NewReplacer("-", "", "_", "", " ", "")

// Of returns the canonical kind for an agent name, or "" when the name is not a known alias.
func Of(name string) string {
	return aliases[separators.Replace(strings.ToLower(strings.TrimSpace(name)))]
}
