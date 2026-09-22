package herdr

import (
	"context"
	"errors"
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

// Si `agent prompt` se rechaza por falta de nombre Y el fallback por la
// superficie del pane TAMBIÉN falla, el error NO puede seguir clasificándose
// como "refused": el registro pudo entregarse antes de agotar el plazo o perder
// la conexión, así que un reintento seguro duplicaría el prompt. Debe quedar
// como despacho desconocido, conservando el detalle de ambos fallos.
func TestPromptFallbackFailureIsNotRefused(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "herdr")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" >> \"$HERDR_TEST_ARGS\"\n" +
		"if [ \"$1\" = \"agent\" ] && [ \"$2\" = \"prompt\" ]; then\n" +
		"  printf '%s' '{\"error\":{\"code\":\"agent_not_ready\",\"message\":\"agent pane-1 is not an active named agent\"}}' >&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"printf '%s' 'connection reset before Herdr acknowledged the prompt' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake Herdr: %v", err)
	}
	t.Setenv("HERDR_TEST_ARGS", argsPath)

	client := NewClient(bin, filepath.Join(dir, "herdr.sock"))
	err := client.Prompt(context.Background(), "pane-1", "hola")
	if err == nil {
		t.Fatal("Prompt() debería propagar el fallo del fallback")
	}
	if IsRefused(err) {
		t.Fatalf("IsRefused() = true para %v; el fallback pudo entregar el prompt", err)
	}
	if !errors.Is(err, ErrDispatchedUnknown) {
		t.Fatalf("err = %v, want ErrDispatchedUnknown", err)
	}
	if errors.Is(err, ErrNotStarted) {
		t.Fatalf("err = %v, want not ErrNotStarted (el reintento no es seguro)", err)
	}
	if message := err.Error(); !strings.Contains(message, "agent_not_ready") ||
		!strings.Contains(message, "connection reset") {
		t.Fatalf("error = %q, want both the original refusal and the fallback failure", message)
	}
	data, readErr := os.ReadFile(argsPath)
	if readErr != nil {
		t.Fatalf("read fake Herdr arguments: %v", readErr)
	}
	got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	want := []string{"agent", "prompt", "pane-1", "hola", "pane", "run", "pane-1", "hola"}
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
