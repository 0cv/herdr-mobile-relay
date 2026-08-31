import { get, writable } from 'svelte/store';

import { speakableText } from './markdown';
import { securityState } from './security';
const ENABLED_KEY = 'herdr_local_speech_enabled';
const VOICE_KEY = 'herdr_local_speech_voice';


/**
 * The default once speech is enabled: pick the local voice matching the
 * phone's language instead of making the user choose between regional
 * variants of it. Explicit selection remains available as an override, and
 * only voices the browser marks local are ever candidates.
 */
export const AUTO_VOICE = 'auto';
export interface LocalSpeechVoice {
  uri: string;
  name: string;
  language: string;
}

export type LocalSpeechState = 'off' | 'idle' | 'speaking' | 'error' | 'unavailable';

export const localSpeechEnabled = writable(localStorage.getItem(ENABLED_KEY) === 'true');
export const selectedLocalVoice = writable(localStorage.getItem(VOICE_KEY) || '');
export const localSpeechVoices = writable<LocalSpeechVoice[]>([]);
export const localSpeechState = writable<LocalSpeechState>('off');

let localVoices: SpeechSynthesisVoice[] = [];

function refreshVoices(): void {
  if (!window.speechSynthesis) {
    localSpeechVoices.set([]);
    localSpeechState.set(get(localSpeechEnabled) ? 'unavailable' : 'off');
    return;
  }
  // Android's system TTS reports variants of one voice under a single
  // voiceURI. The app selects and speaks by URI, so the duplicates are
  // indistinguishable here - and the voice list renders keyed by URI, where
  // a repeat crashes the mounting settings view. First occurrence wins,
  // matching how speakLocal resolves the stored selection.
  const seenURIs = new Set<string>();
  localVoices = window.speechSynthesis.getVoices().filter((voice) => {
    if (!voice.localService || seenURIs.has(voice.voiceURI)) return false;
    seenURIs.add(voice.voiceURI);
    return true;
  });
  localSpeechVoices.set(localVoices.map((voice) => ({
    uri: voice.voiceURI,
    name: voice.name,
    language: voice.lang,
  })));
  localSpeechState.set(get(localSpeechEnabled) ? localVoices.length ? 'idle' : 'unavailable' : 'off');
}

export function setLocalSpeechEnabled(enabled: boolean): void {
  localStorage.setItem(ENABLED_KEY, String(enabled));
  localSpeechEnabled.set(enabled);
  if (!enabled) {
    window.speechSynthesis?.cancel();
    stopKeepalive();
  }
  if (enabled && !get(selectedLocalVoice)) setSelectedLocalVoice(AUTO_VOICE);
  refreshVoices();
}

export function setSelectedLocalVoice(uri: string): void {
  const valid = uri === ''
    || uri === AUTO_VOICE
    || localVoices.some((voice) => voice.localService && voice.voiceURI === uri);
  if (!valid) return;
  if (uri) localStorage.setItem(VOICE_KEY, uri);
  else localStorage.removeItem(VOICE_KEY);
  selectedLocalVoice.set(uri);
}

function resolveVoice(selected: string): SpeechSynthesisVoice | undefined {
  if (selected !== AUTO_VOICE) {
    return localVoices.find((voice) => voice.localService && voice.voiceURI === selected);
  }
  const language = String(navigator.language || 'en');
  const prefix = language.split('-')[0].toLowerCase();
  return localVoices.find((voice) => voice.lang.toLowerCase() === language.toLowerCase())
    ?? localVoices.find((voice) => voice.lang.toLowerCase().startsWith(`${prefix}-`) || voice.lang.toLowerCase() === prefix)
    ?? localVoices.find((voice) => voice.lang.toLowerCase().startsWith('en'))
    ?? localVoices[0];
}

/**
 * Android's TTS rejects utterances past a few thousand characters, and each
 * boundary is a point where a frozen tab loses the queue. Sentence-sized
 * chunks keep every utterance well under the limit.
 */
export function speechChunks(text: string, limit = 1500): string[] {
  const chunks: string[] = [];
  let current = '';
  for (const sentence of text.split(/(?<=[.!?:;\n])\s+/u)) {
    for (let piece = sentence; ;) {
      if (current && current.length + piece.length + 1 > limit) {
        chunks.push(current);
        current = '';
      }
      if (piece.length <= limit) {
        current = current ? `${current} ${piece}` : piece;
        break;
      }
      const cut = piece.lastIndexOf(' ', limit);
      chunks.push(piece.slice(0, cut > 0 ? cut : limit));
      piece = piece.slice(cut > 0 ? cut + 1 : limit);
    }
  }
  if (current) chunks.push(current);
  return chunks;
}

// Speech renders in the system TTS service, not the tab, so Chrome freezes
// the page on screen-off and the utterance queue dies with it. A looping
// near-silent tone keeps the tab audible - the same exemption music apps
// lean on - which also holds the Bluetooth audio link open. MediaSession
// adds lock-screen and headset Stop controls.
let keepalive: HTMLAudioElement | null = null;

function quietToneURI(): string {
  const samples = 8000;
  const data = new Uint8Array(44 + samples * 2);
  const view = new DataView(data.buffer);
  const write = (offset: number, value: string) => {
    for (let i = 0; i < value.length; i++) data[offset + i] = value.charCodeAt(i);
  };
  write(0, 'RIFF'); view.setUint32(4, 36 + samples * 2, true); write(8, 'WAVEfmt ');
  view.setUint32(16, 16, true); view.setUint16(20, 1, true); view.setUint16(22, 1, true);
  view.setUint32(24, 8000, true); view.setUint32(28, 16000, true);
  view.setUint16(32, 2, true); view.setUint16(34, 16, true);
  write(36, 'data'); view.setUint32(40, samples * 2, true);
  for (let i = 0; i < samples; i++) {
    view.setInt16(44 + i * 2, Math.round(Math.sin((i * 2 * Math.PI * 50) / 8000) * 48), true);
  }
  let binary = '';
  for (const byte of data) binary += String.fromCharCode(byte);
  return `data:audio/wav;base64,${btoa(binary)}`;
}

function startKeepalive(onBlocked?: () => void): void {
  keepalive?.pause();
  keepalive = new Audio(quietToneURI());
  keepalive.loop = true;
  keepalive.play()?.catch(() => onBlocked?.());
  if ('mediaSession' in navigator) {
    navigator.mediaSession.metadata = new MediaMetadata({ title: 'Reading response', artist: 'Herdr Mobile Relay' });
    navigator.mediaSession.playbackState = 'playing';
    for (const action of ['pause', 'stop'] as MediaSessionAction[]) {
      try {
        navigator.mediaSession.setActionHandler(action, () => stopLocalSpeech());
      } catch { /* action unsupported */ }
    }
  }
}

function stopKeepalive(): void {
  keepalive?.pause();
  if ('mediaSession' in navigator) navigator.mediaSession.playbackState = 'none';
}

// Chrome on Android is known to leave the synthesis queue paused after the
// renderer thaws; resuming on return is harmless everywhere else.
function resumeOnReturn(): void {
  if (!document.hidden && get(localSpeechState) === 'speaking') window.speechSynthesis?.resume();
}

export function initializeLocalSpeech(): () => void {
  refreshVoices();
  window.speechSynthesis?.addEventListener('voiceschanged', refreshVoices);
  document.addEventListener('visibilitychange', resumeOnReturn);
  return () => {
    window.speechSynthesis?.removeEventListener('voiceschanged', refreshVoices);
    document.removeEventListener('visibilitychange', resumeOnReturn);
    window.speechSynthesis?.cancel();
    stopKeepalive();
  };
}

// Cancelling fires interrupted end/error events on every queued utterance;
// the generation guard keeps them from clobbering the replacing speech.
let speakGeneration = 0;

export function speakLocal(text: string, onIssue?: (message: string) => void): boolean {
  if (get(securityState).locked || !get(localSpeechEnabled) || !window.speechSynthesis || !text.trim()) return false;
  const voice = resolveVoice(get(selectedLocalVoice));
  if (!voice) {
    localSpeechState.set(localVoices.length ? 'error' : 'unavailable');
    onIssue?.(localVoices.length
      ? 'Choose a local voice in Settings before reading responses aloud.'
      : 'No local voice is available on this device.');
    return false;
  }
  const generation = ++speakGeneration;
  window.speechSynthesis.cancel();
  // The keepalive must start inside the tap's activation window: play() from
  // a later TTS callback is autoplay-blocked, silently losing the audible-tab
  // exemption that keeps speech alive with the screen off.
  startKeepalive(() => onIssue?.('The browser blocked background audio; reading may stop when the screen locks.'));
  const chunks = speechChunks(speakableText(text) || text);
  chunks.forEach((chunk, index) => {
    const utterance = new SpeechSynthesisUtterance(chunk);
    utterance.voice = voice;
    utterance.lang = voice.lang;
    if (index === 0) {
      utterance.onstart = () => {
        if (generation !== speakGeneration) return;
        localSpeechState.set('speaking');
      };
    }
    if (index === chunks.length - 1) {
      utterance.onend = () => {
        if (generation !== speakGeneration) return;
        localSpeechState.set('idle');
        stopKeepalive();
      };
    }
    utterance.onerror = () => {
      if (generation !== speakGeneration) return;
      speakGeneration++;
      localSpeechState.set('error');
      stopKeepalive();
      onIssue?.('The selected voice could not speak on this device.');
    };
    window.speechSynthesis.speak(utterance);
  });
  return true;
}

export function stopLocalSpeech(): void {
  speakGeneration++;
  window.speechSynthesis?.cancel();
  stopKeepalive();
  localSpeechState.set(get(localSpeechEnabled) ? localVoices.length ? 'idle' : 'unavailable' : 'off');
}
