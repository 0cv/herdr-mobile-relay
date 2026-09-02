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

func TestPackedQR(t *testing.T) {
	link := "https://phone.example.test/#setup=" + strings.Repeat("Q", 43) + "&invite=invitation-abcdef123456&label=cv"
	size, packed, err := PackedQR(link)
	if err != nil {
		t.Fatal(err)
	}
	if size < 21 || (size-17)%4 != 0 {
		t.Fatalf("size = %d, want a QR version size", size)
	}
	if len(packed) != (size*size+7)/8 {
		t.Fatalf("packed %d bytes for a %dx%d bitmap", len(packed), size, size)
	}
	dark := func(row, column int) bool {
		index := row*size + column
		return packed[index/8]&(1<<(7-index%8)) != 0
	}
	// The three finder patterns tell a camera where the code is, and the
	// bitmap carries no quiet zone: the phone adds that when it draws.
	for _, corner := range [][2]int{{0, 0}, {0, size - 7}, {size - 7, 0}} {
		if !dark(corner[0], corner[1]) || dark(corner[0]+1, corner[1]+1) || !dark(corner[0]+3, corner[1]+3) {
			t.Fatalf("no finder pattern at %v", corner)
		}
	}
	if _, _, err := PackedQR("  "); err == nil {
		t.Fatal("blank value accepted")
	}
	if _, _, err := PackedQR(strings.Repeat("x", MaxQRBytes+1)); err == nil {
		t.Fatal("oversized value accepted")
	}
}
