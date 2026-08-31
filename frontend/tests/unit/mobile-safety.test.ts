import { afterEach, describe, expect, it, vi } from 'vitest';
import { get } from 'svelte/store';
import {
  initializeLocalSpeech,
  localSpeechVoices,
  selectedLocalVoice,
  setLocalSpeechEnabled,
  setSelectedLocalVoice,
  speakLocal,
} from '$lib/local-speech';
import { terminalWakeLock } from '$lib/preferences';
import { targetRefForAgent, targetRefMatchesAgent } from '$lib/resource-id';
import { securityState } from '$lib/security';
import type { Agent } from '$lib/types';
import { mountTerminalWakeLock, wakeLockState } from '$lib/wake-lock';

function exactAgent(overrides: Partial<Agent> = {}): Agent {
  return {
    relay_id: 'relay-1',
    relay_label: 'Laptop',
    pane_id: 'relay-1::pane-1',
    raw_pane_id: 'pane-1',
    server_session_id: 'primary',
    terminal_id: 'terminal-1',
    generation: 4,
    agent_session_id: 'agent-session-1',
    status: 'working',
    ...overrides,
  };
}

afterEach(() => {
  terminalWakeLock.set(false);
  setLocalSpeechEnabled(false);
  setSelectedLocalVoice('');
  securityState.update((state) => ({ ...state, locked: false }));
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('exact route identity', () => {
  it('rejects pane reuse when any authoritative target field changes', () => {
    const agent = exactAgent();
    const target = targetRefForAgent(agent);
    expect(target).not.toBeNull();
    expect(targetRefMatchesAgent(target!, agent)).toBe(true);
    expect(targetRefMatchesAgent(target!, exactAgent({ terminal_id: 'terminal-2' }))).toBe(false);
    expect(targetRefMatchesAgent(target!, exactAgent({ generation: 5 }))).toBe(false);
    expect(targetRefMatchesAgent(target!, exactAgent({ server_session_id: 'named' }))).toBe(false);
    expect(targetRefMatchesAgent(target!, exactAgent({ agent_session_id: 'agent-session-2' }))).toBe(false);
  });
});

describe('local speech', () => {
  it('lists and speaks through explicitly selected local voices only', () => {
    const remote = {
      voiceURI: 'remote',
      name: 'Remote',
      lang: 'en-US',
      localService: false,
      default: false,
    } as SpeechSynthesisVoice;
    const local = {
      voiceURI: 'local',
      name: 'Local',
      lang: 'en-US',
      localService: true,
      default: true,
    } as SpeechSynthesisVoice;
    const speak = vi.fn();
    const cancel = vi.fn();
    vi.stubGlobal('speechSynthesis', {
      getVoices: () => [remote, local],
      speak,
      cancel,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    });
    class FakeUtterance {
      voice: SpeechSynthesisVoice | null = null;
      lang = '';
      onstart: ((event: SpeechSynthesisEvent) => void) | null = null;
      onend: ((event: SpeechSynthesisEvent) => void) | null = null;
      onerror: ((event: SpeechSynthesisErrorEvent) => void) | null = null;

      constructor(public text: string) {}
    }
    vi.stubGlobal('SpeechSynthesisUtterance', FakeUtterance);

    const stop = initializeLocalSpeech();
    setLocalSpeechEnabled(true);
    expect(get(localSpeechVoices).map((voice) => voice.uri)).toEqual(['local']);
    setSelectedLocalVoice('remote');
    expect(get(selectedLocalVoice)).toBe('');
    expect(speakLocal('private response')).toBe(false);

    setSelectedLocalVoice('local');
    expect(speakLocal('private response')).toBe(true);
    const utterance = speak.mock.calls[0][0] as FakeUtterance;
    expect(utterance.text).toBe('private response');
    expect(utterance.voice).toBe(local);
    stop();
  });

  it('collapses Android voice variants that share one voiceURI', () => {
    // Google TTS on Android lists variants of a voice under a single
    // voiceURI. The settings voice list is keyed by URI, and the repeat
    // crashed the mounting settings view on a real phone (each_key_duplicate
    // on `Bosnian Bosnia & Herzegovina`), wedging every control in the app.
    const variant = (name: string): SpeechSynthesisVoice => ({
      voiceURI: 'Bosnian Bosnia & Herzegovina',
      name,
      lang: 'bs-BA',
      localService: true,
      default: false,
    } as SpeechSynthesisVoice);
    vi.stubGlobal('speechSynthesis', {
      getVoices: () => [variant('Bosnian I'), variant('Bosnian II')],
      speak: vi.fn(),
      cancel: vi.fn(),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    });

    const stop = initializeLocalSpeech();
    const voices = get(localSpeechVoices);
    expect(voices).toHaveLength(1);
    expect(voices[0]).toMatchObject({ uri: 'Bosnian Bosnia & Herzegovina', name: 'Bosnian I' });
    stop();
  });
});

describe('terminal wake lock', () => {
  it('acquires only when opted in and visible, then releases while hidden', async () => {
    let visibility: DocumentVisibilityState = 'visible';
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => visibility,
    });
    const release = vi.fn(async () => {});
    const request = vi.fn(async () => ({
      released: false,
      release,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      dispatchEvent: vi.fn(),
      onrelease: null,
    }));
    Object.defineProperty(navigator, 'wakeLock', {
      configurable: true,
      value: { request },
    });

    const stop = mountTerminalWakeLock();
    expect(request).not.toHaveBeenCalled();
    terminalWakeLock.set(true);
    await vi.waitFor(() => expect(get(wakeLockState)).toBe('active'));
    expect(request).toHaveBeenCalledTimes(1);

    visibility = 'hidden';
    document.dispatchEvent(new Event('visibilitychange'));
    await vi.waitFor(() => expect(get(wakeLockState)).toBe('released'));

    stop();
  });
});
