package localize

import (
	"errors"
	"strconv"
)

// ErrorCode is a stable, locale-independent application error identifier.
type ErrorCode string

const (
	ErrorInvalidRequest       ErrorCode = "invalid_request"
	ErrorUnknownAction        ErrorCode = "unknown_action"
	ErrorIncompatibleProtocol ErrorCode = "incompatible_protocol"
	ErrorAuthenticationNeeded ErrorCode = "authentication_required"
	ErrorPermissionDenied     ErrorCode = "permission_denied"
	ErrorReaderForbidden      ErrorCode = "reader_forbidden"
	ErrorDeviceRevoked        ErrorCode = "device_revoked"
	ErrorUnknownServerSession ErrorCode = "unknown_server_session"
	ErrorStaleTarget          ErrorCode = "stale_target"
	ErrorActionInProgress     ErrorCode = "action_in_progress"
	ErrorActionUnknown        ErrorCode = "action_dispatched_unknown"
	ErrorRateLimited          ErrorCode = "rate_limited"
	ErrorTooManyItems         ErrorCode = "too_many_items"
	ErrorUnexpected           ErrorCode = "unexpected"
)

const (
	MaxOperationBytes       = 64
	MaxProtocolVersionBytes = 32
	MaxRetryAfterSeconds    = 86_400
	MaxRenderedItemLimit    = 10_000
)

var (
	ErrUnknownErrorCode      = errors.New("unknown application error code")
	ErrInvalidErrorArguments = errors.New("invalid application error arguments")
)

type errorArgumentKind uint8

const (
	noErrorArguments errorArgumentKind = iota
	operationErrorArguments
	protocolErrorArguments
	retryErrorArguments
	limitErrorArguments
)

// ErrorArguments is sealed so callers can supply only catalog-defined argument
// shapes. In particular, there is no free-form content, path, or detail shape.
type ErrorArguments interface {
	errorArgumentKind() errorArgumentKind
	validate() bool
}

// NoErrorArgs is the argument shape for error codes without substitutions.
type NoErrorArgs struct{}

func (NoErrorArgs) errorArgumentKind() errorArgumentKind { return noErrorArguments }
func (NoErrorArgs) validate() bool                       { return true }

// OperationArgs contains one canonical action identifier. Construct it with
// NewOperationArgs so untrusted text cannot become localized prose.
type OperationArgs struct {
	operation string
}

func NewOperationArgs(operation string) (OperationArgs, error) {
	if !validOperation(operation) {
		return OperationArgs{}, ErrInvalidErrorArguments
	}
	return OperationArgs{operation: operation}, nil
}

func (a OperationArgs) Operation() string                  { return a.operation }
func (OperationArgs) errorArgumentKind() errorArgumentKind { return operationErrorArguments }
func (a OperationArgs) validate() bool                     { return validOperation(a.operation) }

// ProtocolVersionArgs contains canonical received and required protocol tokens.
type ProtocolVersionArgs struct {
	received string
	required string
}

func NewProtocolVersionArgs(received, required string) (ProtocolVersionArgs, error) {
	if !validProtocolVersion(received) || !validProtocolVersion(required) {
		return ProtocolVersionArgs{}, ErrInvalidErrorArguments
	}
	return ProtocolVersionArgs{received: received, required: required}, nil
}

func (a ProtocolVersionArgs) Received() string { return a.received }
func (a ProtocolVersionArgs) Required() string { return a.required }
func (ProtocolVersionArgs) errorArgumentKind() errorArgumentKind {
	return protocolErrorArguments
}
func (a ProtocolVersionArgs) validate() bool {
	return validProtocolVersion(a.received) && validProtocolVersion(a.required)
}

// RetryAfterArgs is a bounded retry delay in whole seconds.
type RetryAfterArgs struct {
	Seconds uint32
}

func (RetryAfterArgs) errorArgumentKind() errorArgumentKind { return retryErrorArguments }
func (a RetryAfterArgs) validate() bool {
	return a.Seconds > 0 && a.Seconds <= MaxRetryAfterSeconds
}

// ItemLimitArgs is a bounded, non-secret collection limit.
type ItemLimitArgs struct {
	Maximum uint32
}

func (ItemLimitArgs) errorArgumentKind() errorArgumentKind { return limitErrorArguments }
func (a ItemLimitArgs) validate() bool {
	return a.Maximum > 0 && a.Maximum <= MaxRenderedItemLimit
}

type errorTemplate struct {
	arguments errorArgumentKind
	render    func(ErrorArguments) string
}

var englishErrorCatalog = map[ErrorCode]errorTemplate{
	ErrorInvalidRequest:       fixedError("The request is invalid."),
	ErrorUnknownAction:        operationError("Unknown action “", "”."),
	ErrorIncompatibleProtocol: protocolError("Protocol version ", " is not compatible; version ", " is required."),
	ErrorAuthenticationNeeded: fixedError("Authentication is required."),
	ErrorPermissionDenied:     fixedError("This action is not permitted."),
	ErrorReaderForbidden:      fixedError("Reader devices cannot perform this action."),
	ErrorDeviceRevoked:        fixedError("This device has been revoked."),
	ErrorUnknownServerSession: fixedError("The selected server session is unavailable."),
	ErrorStaleTarget:          fixedError("The selected agent has changed. Refresh before trying again."),
	ErrorActionInProgress:     fixedError("Another action is already in progress."),
	ErrorActionUnknown:        fixedError("The action may have been dispatched. Inspect its result before continuing."),
	ErrorRateLimited:          retryError("Try again in ", " seconds."),
	ErrorTooManyItems:         limitError("This request is limited to ", " items."),
	ErrorUnexpected:           fixedError("Something went wrong."),
}

var chineseErrorCatalog = map[ErrorCode]errorTemplate{
	ErrorInvalidRequest:       fixedError("请求无效。"),
	ErrorUnknownAction:        operationError("未知操作“", "”。"),
	ErrorIncompatibleProtocol: protocolError("协议版本 ", " 不兼容；需要版本 ", "。"),
	ErrorAuthenticationNeeded: fixedError("需要进行身份验证。"),
	ErrorPermissionDenied:     fixedError("不允许执行此操作。"),
	ErrorReaderForbidden:      fixedError("只读设备无法执行此操作。"),
	ErrorDeviceRevoked:        fixedError("此设备已被撤销。"),
	ErrorUnknownServerSession: fixedError("所选服务器会话不可用。"),
	ErrorStaleTarget:          fixedError("所选智能体已发生变化，请刷新后重试。"),
	ErrorActionInProgress:     fixedError("另一项操作正在进行中。"),
	ErrorActionUnknown:        fixedError("操作可能已发送，请先检查结果再继续。"),
	ErrorRateLimited:          retryError("请在 ", " 秒后重试。"),
	ErrorTooManyItems:         limitError("此请求最多可包含 ", " 项。"),
	ErrorUnexpected:           fixedError("出现错误。"),
}

// RenderError renders app-owned prose for a stable code. An unsupported locale
// falls back to English. Unknown codes return the localized generic message and
// ErrUnknownErrorCode; invalid or mismatched arguments render no message.
func RenderError(locale Locale, code ErrorCode, arguments ErrorArguments) (string, error) {
	catalog := errorCatalog(locale)
	template, ok := catalog[code]
	if !ok {
		return catalog[ErrorUnexpected].render(NoErrorArgs{}), ErrUnknownErrorCode
	}
	if arguments == nil {
		arguments = NoErrorArgs{}
	}
	if arguments.errorArgumentKind() != template.arguments || !arguments.validate() {
		return "", ErrInvalidErrorArguments
	}
	return template.render(arguments), nil
}

func errorCatalog(locale Locale) map[ErrorCode]errorTemplate {
	if NormalizeLocale(string(locale)) == SimplifiedChinese {
		return chineseErrorCatalog
	}
	return englishErrorCatalog
}

func fixedError(message string) errorTemplate {
	return errorTemplate{
		arguments: noErrorArguments,
		render:    func(ErrorArguments) string { return message },
	}
}

func operationError(prefix, suffix string) errorTemplate {
	return errorTemplate{
		arguments: operationErrorArguments,
		render: func(arguments ErrorArguments) string {
			return prefix + arguments.(OperationArgs).operation + suffix
		},
	}
}

func protocolError(prefix, separator, suffix string) errorTemplate {
	return errorTemplate{
		arguments: protocolErrorArguments,
		render: func(arguments ErrorArguments) string {
			values := arguments.(ProtocolVersionArgs)
			return prefix + values.received + separator + values.required + suffix
		},
	}
}

func retryError(prefix, suffix string) errorTemplate {
	return errorTemplate{
		arguments: retryErrorArguments,
		render: func(arguments ErrorArguments) string {
			seconds := arguments.(RetryAfterArgs).Seconds
			return prefix + strconv.FormatUint(uint64(seconds), 10) + suffix
		},
	}
}

func limitError(prefix, suffix string) errorTemplate {
	return errorTemplate{
		arguments: limitErrorArguments,
		render: func(arguments ErrorArguments) string {
			maximum := arguments.(ItemLimitArgs).Maximum
			return prefix + strconv.FormatUint(uint64(maximum), 10) + suffix
		},
	}
}

func validOperation(value string) bool {
	if value == "" || len(value) > MaxOperationBytes || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for i := 1; i < len(value); i++ {
		character := value[i]
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '_' || character == '.' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func validProtocolVersion(value string) bool {
	if value == "" || len(value) > MaxProtocolVersionBytes {
		return false
	}
	for i := range len(value) {
		character := value[i]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-' || character == '+' {
			continue
		}
		return false
	}
	return true
}
