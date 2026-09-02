package localize

import (
	"errors"
	"strings"
	"testing"
)

func TestErrorCatalogKeySetsMatch(t *testing.T) {
	if len(englishErrorCatalog) != len(chineseErrorCatalog) {
		t.Fatalf("catalog sizes differ: en=%d zh-CN=%d", len(englishErrorCatalog), len(chineseErrorCatalog))
	}
	for code := range englishErrorCatalog {
		if _, ok := chineseErrorCatalog[code]; !ok {
			t.Errorf("zh-CN catalog is missing %q", code)
		}
	}
	for code := range chineseErrorCatalog {
		if _, ok := englishErrorCatalog[code]; !ok {
			t.Errorf("English catalog is missing %q", code)
		}
	}
}

func TestEveryErrorRendersInBothLocales(t *testing.T) {
	for code, template := range englishErrorCatalog {
		arguments := validArgumentsFor(template.arguments)
		for _, locale := range SupportedLocales() {
			message, err := RenderError(locale, code, arguments)
			if err != nil {
				t.Errorf("RenderError(%q, %q) error = %v", locale, code, err)
			}
			if message == "" {
				t.Errorf("RenderError(%q, %q) returned an empty message", locale, code)
			}
		}
	}
}

func TestWaveZeroErrorsUseBoundedTypedArguments(t *testing.T) {
	operation, err := NewOperationArgs("pane.send_input")
	if err != nil {
		t.Fatalf("NewOperationArgs() error = %v", err)
	}
	message, err := RenderError(English, ErrorUnknownAction, operation)
	if err != nil {
		t.Fatalf("RenderError() error = %v", err)
	}
	if message != "Unknown action “pane.send_input”." {
		t.Fatalf("unknown action message = %q", message)
	}

	versions, err := NewProtocolVersionArgs("v1.4", "v2.0")
	if err != nil {
		t.Fatalf("NewProtocolVersionArgs() error = %v", err)
	}
	message, err = RenderError(SimplifiedChinese, ErrorIncompatibleProtocol, versions)
	if err != nil {
		t.Fatalf("RenderError() error = %v", err)
	}
	if message != "协议版本 v1.4 不兼容；需要版本 v2.0。" {
		t.Fatalf("protocol message = %q", message)
	}

	message, err = RenderError(English, ErrorInvalidRequest, nil)
	if err != nil || message != "The request is invalid." {
		t.Fatalf("no-argument render = %q, %v", message, err)
	}
}

func TestErrorArgumentValidationRejectsContentAndPaths(t *testing.T) {
	invalidOperations := []string{
		"",
		"PROMPT",
		"pane/send_input",
		"/home/user/private",
		"send secret text",
		strings.Repeat("a", MaxOperationBytes+1),
	}
	for _, value := range invalidOperations {
		if _, err := NewOperationArgs(value); !errors.Is(err, ErrInvalidErrorArguments) {
			t.Errorf("NewOperationArgs(%q) error = %v", value, err)
		}
	}

	invalidVersions := []string{
		"",
		"v2 required now",
		"../../private",
		"v2/token",
		strings.Repeat("v", MaxProtocolVersionBytes+1),
	}
	for _, value := range invalidVersions {
		if _, err := NewProtocolVersionArgs(value, "v2"); !errors.Is(err, ErrInvalidErrorArguments) {
			t.Errorf("NewProtocolVersionArgs(%q) error = %v", value, err)
		}
	}
}

func TestErrorArgumentKindsAndNumericBounds(t *testing.T) {
	tests := []struct {
		name      string
		code      ErrorCode
		arguments ErrorArguments
		wantError bool
	}{
		{name: "wrong shape", code: ErrorInvalidRequest, arguments: RetryAfterArgs{Seconds: 1}, wantError: true},
		{name: "empty operation", code: ErrorUnknownAction, arguments: OperationArgs{}, wantError: true},
		{name: "empty protocol versions", code: ErrorIncompatibleProtocol, arguments: ProtocolVersionArgs{}, wantError: true},
		{name: "retry zero", code: ErrorRateLimited, arguments: RetryAfterArgs{}, wantError: true},
		{name: "retry upper bound", code: ErrorRateLimited, arguments: RetryAfterArgs{Seconds: MaxRetryAfterSeconds}},
		{name: "retry too large", code: ErrorRateLimited, arguments: RetryAfterArgs{Seconds: MaxRetryAfterSeconds + 1}, wantError: true},
		{name: "limit zero", code: ErrorTooManyItems, arguments: ItemLimitArgs{}, wantError: true},
		{name: "limit upper bound", code: ErrorTooManyItems, arguments: ItemLimitArgs{Maximum: MaxRenderedItemLimit}},
		{name: "limit too large", code: ErrorTooManyItems, arguments: ItemLimitArgs{Maximum: MaxRenderedItemLimit + 1}, wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message, err := RenderError(English, test.code, test.arguments)
			if test.wantError {
				if !errors.Is(err, ErrInvalidErrorArguments) || message != "" {
					t.Fatalf("RenderError() = %q, %v", message, err)
				}
				return
			}
			if err != nil || message == "" {
				t.Fatalf("RenderError() = %q, %v", message, err)
			}
		})
	}
}

func TestUnknownErrorCodeUsesLocalizedGenericFallback(t *testing.T) {
	message, err := RenderError(SimplifiedChinese, ErrorCode("not_registered"), NoErrorArgs{})
	if !errors.Is(err, ErrUnknownErrorCode) {
		t.Fatalf("RenderError() error = %v", err)
	}
	if message != "出现错误。" {
		t.Fatalf("fallback message = %q", message)
	}

	english, err := RenderError(Locale("fr"), ErrorPermissionDenied, NoErrorArgs{})
	if err != nil || english != "This action is not permitted." {
		t.Fatalf("unknown locale fallback = %q, %v", english, err)
	}
}

func validArgumentsFor(kind errorArgumentKind) ErrorArguments {
	switch kind {
	case noErrorArguments:
		return NoErrorArgs{}
	case operationErrorArguments:
		arguments, _ := NewOperationArgs("agent.prompt")
		return arguments
	case protocolErrorArguments:
		arguments, _ := NewProtocolVersionArgs("v1", "v2")
		return arguments
	case retryErrorArguments:
		return RetryAfterArgs{Seconds: 30}
	case limitErrorArguments:
		return ItemLimitArgs{Maximum: 100}
	default:
		panic("unhandled error argument kind")
	}
}
