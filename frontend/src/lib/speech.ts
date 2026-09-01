import { get, writable } from 'svelte/store';

import { speakableText } from './markdown';
import { securityState } from './security';

const ENABLED_KEY = 'herdr_speech_enabled';
const LANGUAGE_KEY = 'herdr_speech_language';

/**
 * The languages a relay can read responses in, in the order the settings
 * dropdown lists them. Every fragment is synthesized on the computer and
 * played here as ordinary media, which keeps reading with the screen off
 * where the browser's own speech API stops.
 */
export const SPEECH_LANGUAGES = [
  { code: 'en', label: 'English' },
  { code: 'fr', label: 'French' },
  { code: 'de', label: 'German' },
  { code: 'es', label: 'Spanish' },
  { code: 'zh', label: 'Chinese' },
] as const;

export type SpeechState = 'off' | 'idle' | 'speaking' | 'error';

export function speechLanguageLabel(code: string): string {
  return SPEECH_LANGUAGES.find((language) => language.code === code)?.label || code;
}

export function isSpeechLanguage(code: unknown): code is string {
  return typeof code === 'string' && SPEECH_LANGUAGES.some((language) => language.code === code);
}

function storedLanguage(): string {
  const stored = localStorage.getItem(LANGUAGE_KEY) || '';
  if (isSpeechLanguage(stored)) return stored;
  const phone = String(navigator.language || 'en').split('-')[0].toLowerCase();
  return isSpeechLanguage(phone) ? phone : 'en';
}

export const speechEnabled = writable(localStorage.getItem(ENABLED_KEY) === 'true');
export const speechLanguage = writable(storedLanguage());
export const speechState = writable<SpeechState>(get(speechEnabled) ? 'idle' : 'off');

export function setSpeechEnabled(enabled: boolean): void {
  localStorage.setItem(ENABLED_KEY, String(enabled));
  speechEnabled.set(enabled);
  if (!enabled) stopSpeech();
  else speechState.set('idle');
}

export function setSpeechLanguage(code: string): void {
  if (!isSpeechLanguage(code)) return;
  localStorage.setItem(LANGUAGE_KEY, code);
  speechLanguage.set(code);
  stopSpeech();
}

/**
 * Fragments keep each round trip short, so reading starts almost immediately
 * and one lost fragment costs a sentence rather than the whole response.
 * Chinese sentences end without a space, so their punctuation splits too.
 */
export function speechChunks(text: string, limit = 1500): string[] {
  const chunks: string[] = [];
  let current = '';
  for (const sentence of text.split(/(?<=[.!?:;\n])\s+|(?<=[。！？；：])/u)) {
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

// Relay audio plays through a persistent media element: real media playback
// is what survives a locked screen, and reusing one element keeps the unlock
// earned by the arming tap. MediaSession adds lock-screen and headset Stop
// controls.
let relayAudio: HTMLAudioElement | null = null;
let relayObjectURL = '';

function relayElement(): HTMLAudioElement {
  if (!relayAudio) relayAudio = new Audio();
  return relayAudio;
}

function setRelaySource(blob: Blob): void {
  if (relayObjectURL) URL.revokeObjectURL(relayObjectURL);
  relayObjectURL = URL.createObjectURL(blob);
  relayElement().src = relayObjectURL;
}

function mediaSessionPlaying(): void {
  if (!('mediaSession' in navigator)) return;
  navigator.mediaSession.metadata = new MediaMetadata({ title: 'Reading response', artist: 'Herdr Mobile Relay' });
  navigator.mediaSession.playbackState = 'playing';
  for (const action of ['pause', 'stop'] as MediaSessionAction[]) {
    try {
      navigator.mediaSession.setActionHandler(action, () => stopSpeech());
    } catch { /* action unsupported */ }
  }
}

function silentWAV(): Blob {
  const samples = 400;
  const data = new Uint8Array(44 + samples * 2);
  const view = new DataView(data.buffer);
  const ascii = (offset: number, value: string) => {
    for (let i = 0; i < value.length; i++) data[offset + i] = value.charCodeAt(i);
  };
  ascii(0, 'RIFF'); view.setUint32(4, 36 + samples * 2, true); ascii(8, 'WAVEfmt ');
  view.setUint32(16, 16, true); view.setUint16(20, 1, true); view.setUint16(22, 1, true);
  view.setUint32(24, 8000, true); view.setUint32(28, 16000, true);
  view.setUint16(32, 2, true); view.setUint16(34, 16, true);
  ascii(36, 'data'); view.setUint32(40, samples * 2, true);
  return new Blob([data], { type: 'audio/wav' });
}

/**
 * Speak taps must arm the element before any await: transient activation does
 * not survive the round trip to the relay, and a play() outside it is
 * autoplay-blocked, losing the audible-tab exemption that keeps speech alive
 * with the screen off.
 */
export function armSpeechKeepalive(onIssue?: (message: string) => void): void {
  if (get(securityState).locked || !get(speechEnabled)) return;
  // A moment of silence unlocks the persistent element for every later
  // programmatic play; the fetched fragments arrive long after the tap.
  setRelaySource(silentWAV());
  relayElement().play()?.catch(() => {
    onIssue?.('The browser blocked background audio; reading may stop when the screen locks.');
  });
}

export function releaseSpeechKeepalive(): void {
  if (get(speechState) !== 'speaking') stopPlayback();
}

function stopPlayback(): void {
  relayAudio?.pause();
  if ('mediaSession' in navigator) navigator.mediaSession.playbackState = 'none';
}

export function initializeSpeech(): () => void {
  return () => {
    stopSpeech();
    relayAudio = null;
    if (relayObjectURL) URL.revokeObjectURL(relayObjectURL);
    relayObjectURL = '';
  };
}

// Stopping pauses the element, which resolves the fragment currently playing;
// the generation guard keeps that resolution from continuing the old run.
let speakGeneration = 0;

export type SpeechSender = (text: string, language: string) => Promise<{ data?: Record<string, unknown> }>;

/**
 * Reads text with the relay's speech engine: fragments are synthesized on the
 * computer and played here as ordinary media, prefetching the next fragment
 * while the current one speaks.
 */
export function speakViaRelay(text: string, send: SpeechSender, onIssue?: (message: string) => void): boolean {
  if (get(securityState).locked || !get(speechEnabled) || !text.trim()) return false;
  const generation = ++speakGeneration;
  const chunks = speechChunks(speakableText(text) || text, 240);
  speechState.set('speaking');
  mediaSessionPlaying();
  void playRelayChunks(chunks, send, generation, onIssue);
  return true;
}

async function playRelayChunks(
  chunks: string[],
  send: SpeechSender,
  generation: number,
  onIssue?: (message: string) => void,
): Promise<void> {
  const language = get(speechLanguage);
  let pending = send(chunks[0], language);
  try {
    for (let index = 0; index < chunks.length; index++) {
      const result = await pending;
      if (generation !== speakGeneration) return;
      if (index + 1 < chunks.length) pending = send(chunks[index + 1], language);
      const audio = String(result.data?.audio || '');
      if (!audio) throw new Error('The relay returned no audio.');
      const bytes = Uint8Array.from(atob(audio), (char) => char.charCodeAt(0));
      await playRelayBlob(new Blob([bytes], { type: 'audio/wav' }));
      if (generation !== speakGeneration) return;
    }
    speechState.set('idle');
    stopPlayback();
  } catch (error) {
    if (generation !== speakGeneration) return;
    speakGeneration++;
    speechState.set('error');
    stopPlayback();
    onIssue?.(error instanceof Error && error.message ? error.message : 'The relay could not read this aloud.');
  }
}

// The bundle targets es2022 for older phones, so this stays on the executor
// form rather than Promise.withResolvers.
function playRelayBlob(blob: Blob): Promise<void> {
  return new Promise((resolve, reject) => {
    const element = relayElement();
    setRelaySource(blob);
    // A stop pauses the element instead of ending it; resolving on pause too
    // lets the loop observe the bumped generation and exit.
    element.onended = () => resolve();
    element.onpause = () => resolve();
    element.onerror = () => reject(new Error('This phone could not play the relay audio.'));
    element.play()?.catch(reject);
  });
}

export function stopSpeech(): void {
  speakGeneration++;
  stopPlayback();
  speechState.set(get(speechEnabled) ? 'idle' : 'off');
}
