import { afterEach, describe, expect, it, vi } from 'vitest';
import { get } from 'svelte/store';
import {
  armSpeechKeepalive,
  initializeLocalSpeech,
  localSpeechState,
  localSpeechVoices,
  releaseSpeechKeepalive,
  selectedLocalVoice,
  setLocalSpeechEnabled,
  setSelectedLocalVoice,
  speakLocal,
  speechChunks,
  stopLocalSpeech,
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
    // Enabling defaults to Automatic, and a remote voice is never selectable.
    expect(get(selectedLocalVoice)).toBe('auto');
    setSelectedLocalVoice('remote');
    expect(get(selectedLocalVoice)).toBe('auto');

    // Automatic resolves to the LOCAL voice even though the remote one is
    // the browser default, and formatting is stripped before speaking.
    expect(speakLocal('private `response`')).toBe(true);
    const automatic = speak.mock.calls[0][0] as FakeUtterance;
    expect(automatic.text).toBe('private response');
    expect(automatic.voice).toBe(local);

    setSelectedLocalVoice('local');
    expect(speakLocal('private response')).toBe(true);
    const utterance = speak.mock.calls[1][0] as FakeUtterance;
    expect(utterance.voice).toBe(local);
    stop();
  });

  it('splits long responses at sentence boundaries under the TTS limit', () => {
    const sentence = 'This is one of the sentences the agent wrote.';
    const long = Array.from({ length: 60 }, () => sentence).join(' ');
    const chunks = speechChunks(long);
    expect(chunks.length).toBeGreaterThan(1);
    for (const chunk of chunks) {
      expect(chunk.length).toBeLessThanOrEqual(1500);
      expect(chunk.endsWith('.')).toBe(true);
    }
    expect(chunks.join(' ')).toBe(long);
    // A single overlong token still hard-cuts rather than erroring.
    expect(speechChunks('x'.repeat(3200)).every((chunk) => chunk.length <= 1500)).toBe(true);
    expect(speechChunks('short text')).toEqual(['short text']);
  });

  it('queues every chunk and holds a keepalive stream while speaking', () => {
    const local = {
      voiceURI: 'local',
      name: 'Local',
      lang: 'en-US',
      localService: true,
      default: true,
    } as SpeechSynthesisVoice;
    const speak = vi.fn();
    vi.stubGlobal('speechSynthesis', {
      getVoices: () => [local],
      speak,
      cancel: vi.fn(),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    });
    class FakeUtterance {
      voice: SpeechSynthesisVoice | null = null;
      lang = '';
      onstart: (() => void) | null = null;
      onend: (() => void) | null = null;
      onerror: (() => void) | null = null;

      constructor(public text: string) {}
    }
    vi.stubGlobal('SpeechSynthesisUtterance', FakeUtterance);
    const contexts: FakeAudioContext[] = [];
    class FakeAudioContext {
      state = 'running';
      resumes = 0;
      oscillator: { frequency: { value: number }; started: boolean } | null = null;
      gainValue = 0;
      destination = {};

      constructor() {
        contexts.push(this);
      }

      createOscillator() {
        this.oscillator = { frequency: { value: 0 }, started: false };
        return {
          frequency: this.oscillator.frequency,
          connect: (node: unknown) => node,
          start: () => { this.oscillator!.started = true; },
        };
      }

      createGain() {
        return {
          gain: {
            get value() { return 0; },
            set value(v: number) { contexts.at(-1)!.gainValue = v; },
          },
          connect: (node: unknown) => node,
        };
      }

      resume() {
        this.state = 'running';
        this.resumes++;
        return Promise.resolve();
      }

      suspend() {
        this.state = 'suspended';
        return Promise.resolve();
      }

      close() {
        this.state = 'closed';
        return Promise.resolve();
      }
    }
    vi.stubGlobal('AudioContext', FakeAudioContext);

    const stop = initializeLocalSpeech();
    setLocalSpeechEnabled(true);
    const sentence = 'The relay confirmed every change landed as expected.';
    expect(speakLocal(Array.from({ length: 60 }, () => sentence).join(' '))).toBe(true);
    const utterances = speak.mock.calls.map((call) => call[0] as FakeUtterance);
    expect(utterances.length).toBeGreaterThan(1);

    // The keepalive starts inside the tap's activation window and needs no
    // URL: the CSP has no media-src, so element-based audio is refused. The
    // whole queue is enqueued up front so a frozen tab cannot starve it.
    expect(contexts).toHaveLength(1);
    expect(contexts[0].oscillator?.started).toBe(true);
    expect(contexts[0].oscillator?.frequency.value).toBe(50);
    expect(contexts[0].gainValue).toBeGreaterThan(0);
    expect(contexts[0].gainValue).toBeLessThan(0.01);
    expect(contexts[0].state).toBe('running');
    expect(utterances.slice(0, -1).every((utterance) => utterance.onend === null)).toBe(true);
    utterances[0].onstart?.();
    expect(get(localSpeechState)).toBe('speaking');

    utterances[utterances.length - 1].onend?.();
    expect(get(localSpeechState)).toBe('idle');
    expect(contexts[0].state).toBe('suspended');

    // Stopping mid-speech releases the stream too.
    expect(speakLocal(sentence)).toBe(true);
    expect(contexts[0].state).toBe('running');
    stopLocalSpeech();
    expect(contexts[0].state).toBe('suspended');

    // A tap that must fetch its text first arms the stream inside the
    // activation window; speakLocal then reuses it without a fresh resume,
    // and an abandoned arm releases unless speech is already running.
    armSpeechKeepalive();
    expect(contexts).toHaveLength(1);
    expect(contexts[0].state).toBe('running');
    const resumesBefore = contexts[0].resumes;
    expect(speakLocal(sentence)).toBe(true);
    expect(contexts[0].resumes).toBe(resumesBefore);
    localSpeechState.set('speaking');
    releaseSpeechKeepalive();
    expect(contexts[0].state).toBe('running');
    localSpeechState.set('idle');
    releaseSpeechKeepalive();
    expect(contexts[0].state).toBe('suspended');
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

  it('reports why a speak tap produced no audio', () => {
    const local = {
      voiceURI: 'local',
      name: 'Local',
      lang: 'en-US',
      localService: true,
      default: true,
    } as SpeechSynthesisVoice;
    const speak = vi.fn();
    vi.stubGlobal('speechSynthesis', {
      getVoices: () => [local],
      speak,
      cancel: vi.fn(),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    });
    class FakeUtterance {
      voice: SpeechSynthesisVoice | null = null;
      lang = '';
      onstart: (() => void) | null = null;
      onend: (() => void) | null = null;
      onerror: (() => void) | null = null;

      constructor(public text: string) {}
    }
    vi.stubGlobal('SpeechSynthesisUtterance', FakeUtterance);

    const stop = initializeLocalSpeech();
    setLocalSpeechEnabled(true);

    // Selection explicitly cleared: the tap must explain itself instead of
    // silently doing nothing.
    setSelectedLocalVoice('');
    const issues: string[] = [];
    expect(speakLocal('response', (message) => issues.push(message))).toBe(false);
    expect(issues).toEqual(['Choose a local voice in Settings before reading responses aloud.']);

    // A device-side TTS failure after a successful start reports too.
    setSelectedLocalVoice('local');
    expect(speakLocal('response', (message) => issues.push(message))).toBe(true);
    (speak.mock.calls[0][0] as FakeUtterance).onerror?.();
    expect(issues).toHaveLength(2);
    expect(issues[1]).toContain('could not speak');
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
