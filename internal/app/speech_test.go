package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/protocol"
	"github.com/0cv/herdr-mobile-relay/internal/speech"
	"github.com/coder/websocket"
)

func TestSpeakTextSynthesizesOverTheWire(t *testing.T) {
	s := testServer()
	synthesized := ""
	spokenLanguage := ""
	availableLanguages := []string{"en", "fr"}
	s.speechStatus = func() speech.Catalog {
		return speech.Catalog{Languages: append([]string(nil), availableLanguages...)}
	}
	s.speechSynth = func(_ context.Context, text, language string) ([]byte, error) {
		synthesized = text
		spokenLanguage = language
		if strings.Contains(text, "fail") {
			return nil, errors.New("engine detail stays server-side")
		}
		return []byte("RIFFfakewav"), nil
	}
	useProductionHandler(t, s)
	conn := dialHub(t, s)
	request := func(text, language string) map[string]any {
		t.Helper()
		payload, err := json.Marshal(map[string]any{
			"type":       "speak_text",
			"protocol":   protocol.Version,
			"request_id": "req-1",
			"text":       text,
			"language":   language,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
			t.Fatalf("write speak request: %v", err)
		}
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read speak result: %v", err)
		}
		var result map[string]any
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}

	result := request("hello phone", "fr")
	data, _ := result["data"].(map[string]any)
	if result["ok"] != true || data["format"] != "wav" ||
		data["audio"] != base64.StdEncoding.EncodeToString([]byte("RIFFfakewav")) {
		t.Fatalf("speak result = %+v, want base64 wav payload", result)
	}
	if synthesized != "hello phone" || spokenLanguage != "fr" {
		t.Fatalf("synthesized %q in %q, want the requested text and language", synthesized, spokenLanguage)
	}

	// Engine details never reach the phone; the toast stays generic.
	failed := request("please fail", "en")
	if failed["ok"] != false || failed["error"] != "Speech synthesis failed on this computer" {
		t.Fatalf("failed result = %+v, want generic synthesis failure", failed)
	}

	// A language this host has no voice for is refused before synthesis.
	unsupported := request("hallo", "de")
	if unsupported["ok"] != false || unsupported["error"] != "This computer has no voice for that language" {
		t.Fatalf("unsupported language result = %+v", unsupported)
	}

	availableLanguages = nil
	missing := request("hello", "en")
	if missing["ok"] != false || missing["error"] != "No speech engine is installed on this computer" {
		t.Fatalf("missing engine result = %+v", missing)
	}
}

func TestCancelSpeechStopsRelaySynthesis(t *testing.T) {
	s := testServer()
	s.speechStatus = func() speech.Catalog {
		return speech.Catalog{Languages: []string{"en"}}
	}
	started := make(chan struct{})
	s.speechSynth = func(ctx context.Context, _, _ string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	useProductionHandler(t, s)
	conn := dialHub(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(
		`{"type":"speak_text","protocol":%d,"request_id":"speak-1","speech_request_id":"speech-1","text":"stop this","language":"en"}`, protocol.Version,
	))); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("speech synthesis did not start")
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(
		`{"type":"cancel_speech","protocol":%d,"speech_request_id":"speech-1"}`, protocol.Version,
	))); err != nil {
		t.Fatal(err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read cancelled speech result: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result["request_id"] != "speak-1" || result["ok"] != false {
		t.Fatalf("cancelled speech result = %#v", result)
	}
}

func TestSpeechVoiceManagementOverTheWire(t *testing.T) {
	s := testServer()
	installed := map[string]bool{"en": true}
	s.speechStatus = func() speech.Catalog {
		status := speech.Catalog{CacheDir: "/cache/speech", EngineInstalled: true, ManagementSupported: true}
		for _, language := range speech.Offered {
			engine := "espeak-ng"
			if installed[language] {
				engine = "piper"
				status.Languages = append(status.Languages, language)
			}
			status.Voices = append(status.Voices, speech.VoiceStatus{
				Language:  language,
				Name:      language + "-voice",
				Installed: installed[language],
				Bytes:     63 << 20,
				Engine:    engine,
			})
		}
		return status
	}
	requested := ""
	s.speechInstall = func(_ context.Context, language string) error {
		requested = language
		if language == "zh" {
			return errors.New("engine detail stays server-side")
		}
		installed[language] = true
		return nil
	}
	s.speechRemove = func(language string) error {
		delete(installed, language)
		return nil
	}
	useProductionHandler(t, s)
	conn := dialHub(t, s)
	exchange := func(request map[string]any) []map[string]any {
		t.Helper()
		payload, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
			t.Fatalf("write %v: %v", request["type"], err)
		}
		var messages []map[string]any
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				t.Fatalf("read %v result: %v", request["type"], err)
			}
			var message map[string]any
			if err := json.Unmarshal(data, &message); err != nil {
				t.Fatal(err)
			}
			messages = append(messages, message)
			if message["type"] == "command_result" {
				return messages
			}
		}
	}
	voiceState := func(message map[string]any) map[string]bool {
		t.Helper()
		payload, _ := message["data"].(map[string]any)
		if payload == nil {
			payload = message
		}
		voices, _ := payload["voices"].([]any)
		if len(voices) != len(speech.Offered) {
			t.Fatalf("payload lists %d voices, want %d", len(voices), len(speech.Offered))
		}
		state := map[string]bool{}
		for _, entry := range voices {
			voice, _ := entry.(map[string]any)
			language, _ := voice["language"].(string)
			state[language], _ = voice["installed"].(bool)
		}
		return state
	}

	if metadata, known := protocol.ClassifyAction("speech_voices_list"); !known || metadata.Class != protocol.ActionReadOnly {
		t.Error("listing voices must stay a read-only action")
	}
	for _, action := range []string{"speech_voice_install", "speech_voice_remove"} {
		if metadata, known := protocol.ClassifyAction(action); !known || metadata.Class != protocol.ActionMutating {
			t.Errorf("%s must be a mutating action so readers cannot run it", action)
		}
	}

	listed := exchange(map[string]any{"type": "speech_voices_list", "request_id": "list-1"})
	result := listed[len(listed)-1]
	data, _ := result["data"].(map[string]any)
	if result["ok"] != true || data["cache_dir"] != "/cache/speech" || data["engine_installed"] != true {
		t.Fatalf("list result = %+v", result)
	}
	if state := voiceState(result); !state["en"] || state["fr"] {
		t.Fatalf("listed voices = %+v, want English only", state)
	}

	// Installing answers the caller and tells every phone what the computer
	// can speak now, so a second device is never left with a stale list.
	messages := exchange(map[string]any{"type": "speech_voice_install", "request_id": "install-1", "language": "fr", "protocol": protocol.Version})
	if requested != "fr" {
		t.Fatalf("installed language = %q, want fr", requested)
	}
	broadcast := map[string]any{}
	for _, message := range messages {
		if message["type"] == "speech_voices" {
			broadcast = message
		}
	}
	if len(broadcast) == 0 {
		t.Fatalf("install sent no speech_voices broadcast: %+v", messages)
	}
	if state := voiceState(broadcast); !state["fr"] {
		t.Fatalf("broadcast voices = %+v, want French installed", state)
	}
	if languages, _ := broadcast["languages"].([]any); len(languages) != 2 {
		t.Fatalf("broadcast languages = %+v, want English and French", broadcast["languages"])
	}
	if s.speakableLanguages()[1] != "fr" {
		t.Fatalf("relay speakable languages = %v, want French included", s.speakableLanguages())
	}

	removed := exchange(map[string]any{"type": "speech_voice_remove", "request_id": "remove-1", "language": "fr", "protocol": protocol.Version})
	if state := voiceState(removed[len(removed)-1]); state["fr"] {
		t.Fatalf("remove result = %+v, want French gone", removed[len(removed)-1])
	}

	// A failed download keeps engine details server-side and still reports the
	// unchanged catalog.
	failed := exchange(map[string]any{"type": "speech_voice_install", "request_id": "install-2", "language": "zh", "protocol": protocol.Version})
	result = failed[len(failed)-1]
	if result["ok"] != false || result["error"] != "Downloading the Chinese voice failed on this computer" {
		t.Fatalf("failed install result = %+v", result)
	}
	if state := voiceState(result); state["zh"] {
		t.Fatalf("failed install reported Chinese as installed: %+v", result)
	}

	unsupported := exchange(map[string]any{"type": "speech_voice_install", "request_id": "install-3", "language": "ja", "protocol": protocol.Version})
	result = unsupported[len(unsupported)-1]
	if result["ok"] != false || result["error"] != "That language is not one this app reads aloud" {
		t.Fatalf("unsupported language result = %+v", result)
	}
}
