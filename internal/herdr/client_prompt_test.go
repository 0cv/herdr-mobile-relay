package herdr

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Un agente SIN nombre hace que `herdr agent prompt` falle con
// `agent_not_ready` (el nombre es opt-in: un pane abierto a mano o reabierto
// tras actualizar Herdr no lo tiene). El prompt debe entregarse igual, por la
// superficie del pane, en vez de dejarlo sin enviar.
func TestPromptFallsBackToPaneRunForUnnamedAgent(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "herdr")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"agent\" ] && [ \"$2\" = \"prompt\" ]; then\n" +
		"  printf '%s' '{\"error\":{\"code\":\"agent_not_ready\",\"message\":\"agent pane-1 is not an active named agent\"}}' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"printf '%s\\n' \"$@\" > \"$HERDR_TEST_ARGS\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake Herdr: %v", err)
	}
	t.Setenv("HERDR_TEST_ARGS", argsPath)

	client := NewClient(bin, filepath.Join(dir, "herdr.sock"))
	if err := client.Prompt(context.Background(), "pane-1", "hola"); err != nil {
		t.Fatalf("Prompt() error = %v; el fallback debería entregarlo", err)
	}
	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("el fallback no invocó Herdr: %v", err)
	}
	got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	want := []string{"pane", "run", "pane-1", "hola"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Herdr arguments = %#v, want %#v", got, want)
	}
}

// Un fallo distinto de `agent_not_ready` NO se reintenta por la superficie del
// pane: se devuelve tal cual (no queremos enviar prompts duplicados).
func TestPromptDoesNotFallBackForOtherErrors(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "herdr")
	script := "#!/bin/sh\n" +
		"printf '%s' '{\"error\":{\"code\":\"server_not_running\",\"message\":\"no server\"}}' >&2\n" +
		"printf '%s\\n' \"$@\" > \"$HERDR_TEST_ARGS\"\n" +
		"exit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake Herdr: %v", err)
	}
	t.Setenv("HERDR_TEST_ARGS", argsPath)

	client := NewClient(bin, filepath.Join(dir, "herdr.sock"))
	if err := client.Prompt(context.Background(), "pane-1", "hola"); err == nil {
		t.Fatal("Prompt() debería propagar el error")
	}
	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read fake Herdr arguments: %v", err)
	}
	got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	want := []string{"agent", "prompt", "pane-1", "hola"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("no debería haber fallback; args = %#v, want %#v", got, want)
	}
}
