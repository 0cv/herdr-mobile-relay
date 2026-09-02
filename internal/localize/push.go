package localize

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// PushCategory identifies a fixed app-owned notification template.
type PushCategory string

const (
	PushTest        PushCategory = "test"
	PushBlocked     PushCategory = "blocked"
	PushQuestion    PushCategory = "question"
	PushCompletion  PushCategory = "completion"
	PushUpdate      PushCategory = "update"
	PushTrustChange PushCategory = "trust_change"
)

const (
	MaxRelayLabelRunes   = 80
	MaxProjectLabelRunes = 120
	MaxAgentLabelRunes   = 80
)

var (
	ErrUnknownPushCategory = errors.New("unknown push category")
	ErrInvalidPushLabels   = errors.New("invalid push labels")
)

// PushLabels are the only caller-owned strings accepted by push templates.
// They are rendered verbatim after independent validation; terminal output,
// paths, secrets, and arbitrary message content have no template slot.
type PushLabels struct {
	Relay   string
	Project string
	Agent   string
}

// PushMessage is safe app-owned notification prose. Labels remain unchanged.
type PushMessage struct {
	Title string
	Body  string
}

type pushTemplate struct {
	validate func(PushLabels) bool
	render   func(PushLabels) PushMessage
}

var englishPushCatalog = map[PushCategory]pushTemplate{
	PushTest: {
		validate: relayLabels,
		render: func(labels PushLabels) PushMessage {
			return PushMessage{
				Title: "Test notification",
				Body:  "Notifications from " + labels.Relay + " are working.",
			}
		},
	},
	PushBlocked: {
		validate: agentProjectLabels,
		render: func(labels PushLabels) PushMessage {
			return PushMessage{
				Title: labels.Agent + " is blocked",
				Body:  "Open " + labels.Project + " on " + labels.Relay + " to review it.",
			}
		},
	},
	PushQuestion: {
		validate: agentProjectLabels,
		render: func(labels PushLabels) PushMessage {
			return PushMessage{
				Title: labels.Agent + " has a question",
				Body:  "Open " + labels.Project + " on " + labels.Relay + " to respond.",
			}
		},
	},
	PushCompletion: {
		validate: agentProjectLabels,
		render: func(labels PushLabels) PushMessage {
			return PushMessage{
				Title: labels.Agent + " completed its work",
				Body:  labels.Project + " on " + labels.Relay + " is ready to review.",
			}
		},
	},
	PushUpdate: {
		validate: relayLabels,
		render: func(labels PushLabels) PushMessage {
			return PushMessage{
				Title: "Herdr update available",
				Body:  "An update is available for " + labels.Relay + ".",
			}
		},
	},
	PushTrustChange: {
		validate: relayLabels,
		render: func(labels PushLabels) PushMessage {
			return PushMessage{
				Title: "Device trust changed",
				Body:  "Review trusted devices for " + labels.Relay + ".",
			}
		},
	},
}

var chinesePushCatalog = map[PushCategory]pushTemplate{
	PushTest: {
		validate: relayLabels,
		render: func(labels PushLabels) PushMessage {
			return PushMessage{
				Title: "测试通知",
				Body:  "来自 " + labels.Relay + " 的通知已正常工作。",
			}
		},
	},
	PushBlocked: {
		validate: agentProjectLabels,
		render: func(labels PushLabels) PushMessage {
			return PushMessage{
				Title: labels.Agent + " 已被阻塞",
				Body:  "打开 " + labels.Relay + " 上的 " + labels.Project + " 进行查看。",
			}
		},
	},
	PushQuestion: {
		validate: agentProjectLabels,
		render: func(labels PushLabels) PushMessage {
			return PushMessage{
				Title: labels.Agent + " 有一个问题",
				Body:  "打开 " + labels.Relay + " 上的 " + labels.Project + " 进行回复。",
			}
		},
	},
	PushCompletion: {
		validate: agentProjectLabels,
		render: func(labels PushLabels) PushMessage {
			return PushMessage{
				Title: labels.Agent + " 已完成工作",
				Body:  labels.Relay + " 上的 " + labels.Project + " 已可供查看。",
			}
		},
	},
	PushUpdate: {
		validate: relayLabels,
		render: func(labels PushLabels) PushMessage {
			return PushMessage{
				Title: "Herdr 有可用更新",
				Body:  labels.Relay + " 有可用更新。",
			}
		},
	},
	PushTrustChange: {
		validate: relayLabels,
		render: func(labels PushLabels) PushMessage {
			return PushMessage{
				Title: "设备信任状态已更改",
				Body:  "请查看 " + labels.Relay + " 的受信任设备。",
			}
		},
	},
}

// RenderPush selects a fixed template and validates each label before rendering.
// Unsupported locales fall back to English.
func RenderPush(locale Locale, category PushCategory, labels PushLabels) (PushMessage, error) {
	catalog := pushCatalog(locale)
	template, ok := catalog[category]
	if !ok {
		return PushMessage{}, ErrUnknownPushCategory
	}
	if !template.validate(labels) {
		return PushMessage{}, ErrInvalidPushLabels
	}
	return template.render(labels), nil
}

func pushCatalog(locale Locale) map[PushCategory]pushTemplate {
	if NormalizeLocale(string(locale)) == SimplifiedChinese {
		return chinesePushCatalog
	}
	return englishPushCatalog
}

func relayLabels(labels PushLabels) bool {
	return labels.Project == "" && labels.Agent == "" && validLabel(labels.Relay, MaxRelayLabelRunes)
}

func agentProjectLabels(labels PushLabels) bool {
	return validLabel(labels.Relay, MaxRelayLabelRunes) &&
		validLabel(labels.Project, MaxProjectLabelRunes) &&
		validLabel(labels.Agent, MaxAgentLabelRunes)
}

func validLabel(value string, maximumRunes int) bool {
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > maximumRunes {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}
