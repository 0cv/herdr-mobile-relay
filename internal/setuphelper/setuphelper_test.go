package setuphelper

import (
	"strings"
	"testing"
)

func TestSetupFragment(t *testing.T) {
	fragment := SetupFragment("a+b&c", "My Host", "wss://example.test/ws?a=1")
	for _, expected := range []string{"setup=a%2Bb%26c", "label=My+Host", "relay=wss%3A%2F%2Fexample.test%2Fws%3Fa%3D1"} {
		if !strings.Contains(fragment, expected) {
			t.Fatalf("fragment %q does not contain %q", fragment, expected)
		}
	}
}

func TestNormalizeOrigin(t *testing.T) {
	got, err := NormalizeOrigin("Example.COM/", false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://example.com" {
		t.Fatalf("origin = %q", got)
	}
	if _, err := NormalizeOrigin("http://example.com", true); err == nil {
		t.Fatal("non-loopback HTTP accepted")
	}
	if got, err := NormalizeOrigin("http://127.0.0.1:8375", true); err != nil || got != "http://127.0.0.1:8375" {
		t.Fatalf("loopback = %q, %v", got, err)
	}
}

func TestTerminalQR(t *testing.T) {
	rendered, err := TerminalQR("https://example.test/#setup=secret", 120)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.ContainsAny(rendered, "█▀▄") {
		t.Fatalf("QR output is empty: %q", rendered)
	}
	if _, err := TerminalQR("https://example.test", 5); err == nil {
		t.Fatal("narrow terminal accepted")
	}
}
