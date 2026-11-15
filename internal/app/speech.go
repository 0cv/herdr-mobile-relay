package app

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"time"

	"github.com/0cv/herdr-mobile-relay/internal/speech"
	"github.com/0cv/herdr-mobile-relay/internal/transport"
)

type speechRequest struct {
	cancelled bool
	cancel    context.CancelFunc
}

// speakText synthesizes one sentence-sized fragment with the host's TTS
// engine and returns the WAV inline. The phone plays it as ordinary media,
// which keeps working with the screen off where the browser speech API dies.
func (s *Server) speakText(client *transport.ClientConn, requestID, speechRequestID, text, language string) {
	reply := s.reply(client, requestID, "speak_text", "")
	speakable := s.speakableLanguages()
	if len(speakable) == 0 {
		reply.fail("No speech engine is installed on this computer")
		return
	}
	if !slices.Contains(speakable, language) {
		reply.fail("This computer has no voice for that language")
		return
	}
	ctx, cancel := context.WithTimeout(client.Context(), 15*time.Second)
	requestKey := client.ID() + "\x00" + speechRequestID
	request := &speechRequest{cancel: cancel}
	s.speechMu.Lock()
	if s.speechRequests == nil {
		s.speechRequests = make(map[string]*speechRequest)
	}
	if previous := s.speechRequests[requestKey]; previous != nil {
		if previous.cancelled {
			request.cancelled = true
		} else if previous.cancel != nil {
			previous.cancel()
		}
	}
	s.speechRequests[requestKey] = request
	s.speechMu.Unlock()
	if request.cancelled {
		cancel()
	}
	defer func() {
		cancel()
		s.speechMu.Lock()
		if s.speechRequests[requestKey] == request {
			delete(s.speechRequests, requestKey)
		}
		s.speechMu.Unlock()
	}()
	wav, err := s.speechSynth(ctx, text, language)
	if err != nil {
		s.logger.Warn("speech synthesis failed", "error", err, "language", language)
		reply.fail("Speech synthesis failed on this computer")
		return
	}
	reply.complete(map[string]any{
		"format": "wav",
		"audio":  base64.StdEncoding.EncodeToString(wav),
	})
}

func (s *Server) cancelSpeech(clientID, speechRequestID string) {
	if clientID == "" || speechRequestID == "" {
		return
	}
	key := clientID + "\x00" + speechRequestID
	s.speechMu.Lock()
	defer s.speechMu.Unlock()
	if request := s.speechRequests[key]; request != nil {
		request.cancelled = true
		if request.cancel != nil {
			request.cancel()
		}
		return
	}
	if s.speechRequests == nil {
		s.speechRequests = make(map[string]*speechRequest)
	}
	if len(s.speechRequests) >= 128 {
		for requestKey, request := range s.speechRequests {
			if request.cancelled && request.cancel == nil {
				delete(s.speechRequests, requestKey)
			}
		}
		if len(s.speechRequests) >= 128 {
			return
		}
	}
	s.speechRequests[key] = &speechRequest{cancelled: true}
}

func (s *Server) speakableLanguages() []string {
	return s.rememberSpeechLanguages(s.speechStatus().Languages)
}

func (s *Server) rememberSpeechLanguages(languages []string) []string {
	s.speechMu.Lock()
	defer s.speechMu.Unlock()
	s.speechLanguages = append(s.speechLanguages[:0], languages...)
	return append([]string(nil), s.speechLanguages...)
}

// changeSpeechVoice downloads or deletes one cached voice, then tells every
// connected phone what this computer can speak now.
func (s *Server) changeSpeechVoice(client *transport.ClientConn, requestID, action, language string) {
	reply := s.reply(client, requestID, action, "")
	if !slices.Contains(speech.Offered, language) {
		reply.fail("That language is not one this app reads aloud")
		return
	}
	if action != "speech_voice_remove" && !s.speechStatus().ManagementSupported {
		reply.failWithData("Voice downloads are not supported on this computer", s.speechVoicePayload(nil))
		return
	}
	label := speech.LanguageLabel(language)
	var err error
	if action == "speech_voice_remove" {
		err = s.speechRemove(language)
	} else {
		ctx, cancel := context.WithTimeout(client.Context(), speech.InstallTimeout)
		defer cancel()
		err = s.speechInstall(ctx, language)
	}
	status := s.speechVoicePayload(nil)
	if err != nil {
		s.logger.Warn("speech voice change failed", "error", err, "language", language, "action", action)
		message := fmt.Sprintf("Downloading the %s voice failed on this computer", label)
		if action == "speech_voice_remove" {
			message = fmt.Sprintf("Removing the %s voice failed on this computer", label)
		}
		reply.failWithData(message, status)
		return
	}
	s.hub.Broadcast(s.speechVoicePayload(map[string]any{"type": "speech_voices"}))
	reply.complete(status)
}

// speechVoicePayload reads the cache and republishes what this computer can
// speak, so the answer and the broadcast can never disagree.
func (s *Server) speechVoicePayload(envelope map[string]any) map[string]any {
	status := s.speechStatus()
	s.speechMu.Lock()
	s.speechLanguages = append([]string(nil), status.Languages...)
	s.speechMu.Unlock()
	voices := make([]map[string]any, 0, len(status.Voices))
	for _, voice := range status.Voices {
		voices = append(voices, map[string]any{
			"language":  voice.Language,
			"name":      voice.Name,
			"installed": voice.Installed,
			"bytes":     voice.Bytes,
			"engine":    voice.Engine,
		})
	}
	payload := map[string]any{
		"cache_dir":            status.CacheDir,
		"engine_installed":     status.EngineInstalled,
		"management_supported": status.ManagementSupported,
		"languages":            status.Languages,
		"voices":               voices,
	}
	for key, value := range envelope {
		payload[key] = value
	}
	return payload
}
