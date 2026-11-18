package localize

import "testing"

func TestNormalizeLocale(t *testing.T) {
	tests := []struct {
		input string
		want  Locale
	}{
		{input: "en", want: English},
		{input: "EN-us", want: English},
		{input: " zh_CN ", want: SimplifiedChinese},
		{input: "zh-cn-u-nu-hanidec", want: SimplifiedChinese},
		{input: "", want: English},
		{input: "zh", want: English},
		{input: "zh-TW", want: English},
		{input: "fr-FR", want: English},
		{input: "not a locale", want: English},
	}

	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			if got := NormalizeLocale(test.input); got != test.want {
				t.Fatalf("NormalizeLocale(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}
