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

/**
 * Reads aloud with a TTS engine on the relay computer instead of the phone:
 * the audio arrives as ordinary media over the encrypted channel, which keeps
 * playing with the screen off where Android kills the browser speech API.
 */
export const RELAY_VOICE = 'relay';
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

// The relay voice needs no phone-side voice or speech API at all, so it
// keeps speech usable where the device offers neither.
function restingState(): LocalSpeechState {
  if (!get(localSpeechEnabled)) return 'off';
  if (localVoices.length || get(selectedLocalVoice) === RELAY_VOICE) return 'idle';
  return 'unavailable';
}

function refreshVoices(): void {
  if (!window.speechSynthesis) {
    localSpeechVoices.set([]);
    localSpeechState.set(restingState());
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
  localSpeechState.set(restingState());
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
    || uri === RELAY_VOICE
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
// the page on screen-off and the utterance queue dies with it. A quiet
// Web Audio tone keeps the tab audible - the same exemption music apps lean
// on - which also holds the Bluetooth audio link open. An oscillator rather
// than an audio element because the CSP has no media-src: a data: or blob:
// source is refused, and Web Audio needs no URL at all. MediaSession adds
// lock-screen and headset Stop controls.
let keepaliveContext: AudioContext | null = null;

function ensureKeepalive(onBlocked?: () => void): void {
  if (!window.AudioContext) return;
  if (!keepaliveContext) {
    const context = new AudioContext();
    const oscillator = context.createOscillator();
    const gain = context.createGain();
    oscillator.frequency.value = 50;
    // About -54 dBFS: inaudible in practice, comfortably above Chromium's
    // -72 dBFS silence threshold so the tab counts as playing audio.
    gain.gain.value = 0.002;
    oscillator.connect(gain).connect(context.destination);
    oscillator.start();
    keepaliveContext = context;
  }
  if (keepaliveContext.state !== 'running') {
    keepaliveContext.resume().catch(() => onBlocked?.());
  }
  mediaSessionPlaying();
}

function mediaSessionPlaying(): void {
  if (!('mediaSession' in navigator)) return;
  navigator.mediaSession.metadata = new MediaMetadata({ title: 'Reading response', artist: 'Herdr Mobile Relay' });
  navigator.mediaSession.playbackState = 'playing';
  for (const action of ['pause', 'stop'] as MediaSessionAction[]) {
    try {
      navigator.mediaSession.setActionHandler(action, () => stopLocalSpeech());
    } catch { /* action unsupported */ }
  }
}

// Relay-synthesized speech plays through a persistent media element: real
// media playback is what survives a locked screen, and reusing one element
// keeps the unlock earned by the arming tap.
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
 * Speak taps that fetch their text first must arm the keepalive before the
 * await: transient activation does not survive the round trip, and a play()
 * outside it is autoplay-blocked, losing the audible-tab exemption that
 * keeps speech alive with the screen off.
 */
export function armSpeechKeepalive(onIssue?: (message: string) => void): void {
  if (get(securityState).locked || !get(localSpeechEnabled)) return;
  const onBlocked = () => onIssue?.('The browser blocked background audio; reading may stop when the screen locks.');
  if (get(selectedLocalVoice) === RELAY_VOICE) {
    // A moment of silence unlocks the element for every later programmatic
    // play(); the fetched chunks arrive long after the activation window.
    setRelaySource(silentWAV());
    relayElement().play()?.catch(onBlocked);
    return;
  }
  ensureKeepalive(onBlocked);
}

export function releaseSpeechKeepalive(): void {
  if (get(localSpeechState) !== 'speaking') stopKeepalive();
}

function stopKeepalive(): void {
  void keepaliveContext?.suspend();
  relayAudio?.pause();
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
    void keepaliveContext?.close();
    keepaliveContext = null;
    relayAudio = null;
    if (relayObjectURL) URL.revokeObjectURL(relayObjectURL);
    relayObjectURL = '';
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
  // Reuses the stream a tap-time armSpeechKeepalive already started; a fresh
  // play() here would run outside the activation window and be blocked.
  ensureKeepalive(() => onIssue?.('The browser blocked background audio; reading may stop when the screen locks.'));
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

export type SpeechSender = (text: string) => Promise<{ data?: Record<string, unknown> }>;

/**
 * Reads text with the relay's TTS engine: sentence-sized fragments are
 * synthesized on the computer and played here as ordinary media, prefetching
 * the next fragment while the current one speaks.
 */
export function speakViaRelay(text: string, send: SpeechSender, onIssue?: (message: string) => void): boolean {
  if (get(securityState).locked || !get(localSpeechEnabled) || !text.trim()) return false;
  const generation = ++speakGeneration;
  window.speechSynthesis?.cancel();
  const chunks = speechChunks(speakableText(text) || text, 240);
  localSpeechState.set('speaking');
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
  let pending = send(chunks[0]);
  try {
    for (let index = 0; index < chunks.length; index++) {
      const result = await pending;
      if (generation !== speakGeneration) return;
      if (index + 1 < chunks.length) pending = send(chunks[index + 1]);
      const audio = String(result.data?.audio || '');
      if (!audio) throw new Error('The relay returned no audio.');
      const bytes = Uint8Array.from(atob(audio), (char) => char.charCodeAt(0));
      await playRelayBlob(new Blob([bytes], { type: 'audio/wav' }));
      if (generation !== speakGeneration) return;
    }
    localSpeechState.set('idle');
    stopKeepalive();
  } catch (error) {
    if (generation !== speakGeneration) return;
    speakGeneration++;
    localSpeechState.set('error');
    stopKeepalive();
    onIssue?.(error instanceof Error && error.message ? error.message : 'The relay could not read this aloud.');
  }
}

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

export function stopLocalSpeech(): void {
  speakGeneration++;
  window.speechSynthesis?.cancel();
  stopKeepalive();
  localSpeechState.set(restingState());
}
