package localize

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestPushCatalogKeySetsMatch(t *testing.T) {
	if len(englishPushCatalog) != len(chinesePushCatalog) {
		t.Fatalf("catalog sizes differ: en=%d zh-CN=%d", len(englishPushCatalog), len(chinesePushCatalog))
	}
	for category := range englishPushCatalog {
		if _, ok := chinesePushCatalog[category]; !ok {
			t.Errorf("zh-CN catalog is missing %q", category)
		}
	}
	for category := range chinesePushCatalog {
		if _, ok := englishPushCatalog[category]; !ok {
			t.Errorf("English catalog is missing %q", category)
		}
	}
}

func TestEveryPushCategoryInBothLocales(t *testing.T) {
	relayOnly := PushLabels{Relay: "relay-one"}
	agentProject := PushLabels{Relay: "relay-one", Project: "mobile-app", Agent: "Claude"}
	tests := []struct {
		name     string
		locale   Locale
		category PushCategory
		labels   PushLabels
		want     PushMessage
	}{
		{name: "English test", locale: English, category: PushTest, labels: relayOnly, want: PushMessage{Title: "Test notification", Body: "Notifications from relay-one are working."}},
		{name: "English blocked", locale: English, category: PushBlocked, labels: agentProject, want: PushMessage{Title: "Claude is blocked", Body: "Open mobile-app on relay-one to review it."}},
		{name: "English question", locale: English, category: PushQuestion, labels: agentProject, want: PushMessage{Title: "Claude has a question", Body: "Open mobile-app on relay-one to respond."}},
		{name: "English completion", locale: English, category: PushCompletion, labels: agentProject, want: PushMessage{Title: "Claude completed its work", Body: "mobile-app on relay-one is ready to review."}},
		{name: "English update", locale: English, category: PushUpdate, labels: relayOnly, want: PushMessage{Title: "Herdr update available", Body: "An update is available for relay-one."}},
		{name: "English trust change", locale: English, category: PushTrustChange, labels: relayOnly, want: PushMessage{Title: "Device trust changed", Body: "Review trusted devices for relay-one."}},
		{name: "Chinese test", locale: SimplifiedChinese, category: PushTest, labels: relayOnly, want: PushMessage{Title: "测试通知", Body: "来自 relay-one 的通知已正常工作。"}},
		{name: "Chinese blocked", locale: SimplifiedChinese, category: PushBlocked, labels: agentProject, want: PushMessage{Title: "Claude 已被阻塞", Body: "打开 relay-one 上的 mobile-app 进行查看。"}},
		{name: "Chinese question", locale: SimplifiedChinese, category: PushQuestion, labels: agentProject, want: PushMessage{Title: "Claude 有一个问题", Body: "打开 relay-one 上的 mobile-app 进行回复。"}},
		{name: "Chinese completion", locale: SimplifiedChinese, category: PushCompletion, labels: agentProject, want: PushMessage{Title: "Claude 已完成工作", Body: "relay-one 上的 mobile-app 已可供查看。"}},
		{name: "Chinese update", locale: SimplifiedChinese, category: PushUpdate, labels: relayOnly, want: PushMessage{Title: "Herdr 有可用更新", Body: "relay-one 有可用更新。"}},
		{name: "Chinese trust change", locale: SimplifiedChinese, category: PushTrustChange, labels: relayOnly, want: PushMessage{Title: "设备信任状态已更改", Body: "请查看 relay-one 的受信任设备。"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message, err := RenderPush(test.locale, test.category, test.labels)
			if err != nil {
				t.Fatalf("RenderPush() error = %v", err)
			}
			if message != test.want {
				t.Fatalf("RenderPush() = %#v, want %#v", message, test.want)
			}
		})
	}
}

func TestPushUnknownLocaleFallsBackToEnglish(t *testing.T) {
	labels := PushLabels{Relay: "relay-one"}
	fallback, err := RenderPush(Locale("fr-FR"), PushTest, labels)
	if err != nil {
		t.Fatalf("RenderPush() error = %v", err)
	}
	english, err := RenderPush(English, PushTest, labels)
	if err != nil {
		t.Fatalf("RenderPush() English error = %v", err)
	}
	if fallback != english {
		t.Fatalf("unknown locale result = %#v, want %#v", fallback, english)
	}
}

func TestPushLabelsRemainVerbatim(t *testing.T) {
	labels := PushLabels{
		Relay:   " relay & one ",
		Project: " project <alpha> ",
		Agent:   " Agent %s ",
	}
	message, err := RenderPush(SimplifiedChinese, PushQuestion, labels)
	if err != nil {
		t.Fatalf("RenderPush() error = %v", err)
	}
	for _, label := range []string{labels.Relay, labels.Project, labels.Agent} {
		if !strings.Contains(message.Title+message.Body, label) {
			t.Errorf("rendered message changed or omitted label %q: %#v", label, message)
		}
	}
}

func TestPushLabelsAreSeparatelyBounded(t *testing.T) {
	valid := PushLabels{
		Relay:   strings.Repeat("中", MaxRelayLabelRunes),
		Project: strings.Repeat("项", MaxProjectLabelRunes),
		Agent:   strings.Repeat("智", MaxAgentLabelRunes),
	}
	if _, err := RenderPush(English, PushBlocked, valid); err != nil {
		t.Fatalf("maximum-size labels rejected: %v", err)
	}

	tests := []struct {
		name   string
		labels PushLabels
	}{
		{name: "relay", labels: PushLabels{Relay: strings.Repeat("r", MaxRelayLabelRunes+1), Project: "project", Agent: "agent"}},
		{name: "project", labels: PushLabels{Relay: "relay", Project: strings.Repeat("p", MaxProjectLabelRunes+1), Agent: "agent"}},
		{name: "agent", labels: PushLabels{Relay: "relay", Project: "project", Agent: strings.Repeat("a", MaxAgentLabelRunes+1)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := RenderPush(English, PushBlocked, test.labels); !errors.Is(err, ErrInvalidPushLabels) {
				t.Fatalf("RenderPush() error = %v", err)
			}
		})
	}
}

func TestPushTemplatesRejectMissingExtraAndControlLabels(t *testing.T) {
	tests := []struct {
		name     string
		category PushCategory
		labels   PushLabels
	}{
		{name: "missing relay", category: PushTest, labels: PushLabels{}},
		{name: "extra project has no test slot", category: PushTest, labels: PushLabels{Relay: "relay", Project: "secret"}},
		{name: "missing project", category: PushBlocked, labels: PushLabels{Relay: "relay", Agent: "agent"}},
		{name: "missing agent", category: PushQuestion, labels: PushLabels{Relay: "relay", Project: "project"}},
		{name: "newline", category: PushCompletion, labels: PushLabels{Relay: "relay", Project: "project\nprivate", Agent: "agent"}},
		{name: "invalid UTF-8", category: PushUpdate, labels: PushLabels{Relay: string([]byte{0xff})}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := RenderPush(English, test.category, test.labels); !errors.Is(err, ErrInvalidPushLabels) {
				t.Fatalf("RenderPush() error = %v", err)
			}
		})
	}

	if _, err := RenderPush(English, PushCategory("terminal_content"), PushLabels{Relay: "relay"}); !errors.Is(err, ErrUnknownPushCategory) {
		t.Fatalf("unknown category error = %v", err)
	}
}

func TestPushTemplateSurfaceHasLabelsOnly(t *testing.T) {
	typeOfLabels := reflect.TypeOf(PushLabels{})
	want := []string{"Relay", "Project", "Agent"}
	if typeOfLabels.NumField() != len(want) {
		t.Fatalf("PushLabels has %d fields, want %d", typeOfLabels.NumField(), len(want))
	}
	for index, fieldName := range want {
		if got := typeOfLabels.Field(index).Name; got != fieldName {
			t.Fatalf("PushLabels field %d = %q, want %q", index, got, fieldName)
		}
	}
}
