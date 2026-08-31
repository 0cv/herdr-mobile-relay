package localize

import "strings"

// Locale is a locale supported by the relay-owned message catalog.
type Locale string

const (
	English           Locale = "en"
	SimplifiedChinese Locale = "zh-CN"
)

var supportedLocales = [...]Locale{English, SimplifiedChinese}

// SupportedLocales returns the complete set of locales with catalogs.
func SupportedLocales() []Locale {
	locales := make([]Locale, len(supportedLocales))
	copy(locales, supportedLocales[:])
	return locales
}

// NormalizeLocale converts a locale tag to a supported locale. Unsupported and
// malformed tags deliberately fall back to English.
func NormalizeLocale(value string) Locale {
	tag := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(value), "_", "-"))
	switch {
	case tag == "zh-cn" || strings.HasPrefix(tag, "zh-cn-"):
		return SimplifiedChinese
	case tag == "en" || strings.HasPrefix(tag, "en-"):
		return English
	default:
		return English
	}
}
