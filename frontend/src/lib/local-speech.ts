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
  if (!enabled) window.speechSynthesis?.cancel();
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

export function initializeLocalSpeech(): () => void {
  refreshVoices();
  window.speechSynthesis?.addEventListener('voiceschanged', refreshVoices);
  return () => {
    window.speechSynthesis?.removeEventListener('voiceschanged', refreshVoices);
    window.speechSynthesis?.cancel();
  };
}

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
  window.speechSynthesis.cancel();
  const utterance = new SpeechSynthesisUtterance(speakableText(text) || text);
  utterance.voice = voice;
  utterance.lang = voice.lang;
  utterance.onstart = () => localSpeechState.set('speaking');
  utterance.onend = () => localSpeechState.set('idle');
  utterance.onerror = () => {
    localSpeechState.set('error');
    onIssue?.('The selected voice could not speak on this device.');
  };
  window.speechSynthesis.speak(utterance);
  return true;
}

export function stopLocalSpeech(): void {
  window.speechSynthesis?.cancel();
  localSpeechState.set(get(localSpeechEnabled) ? localVoices.length ? 'idle' : 'unavailable' : 'off');
}
