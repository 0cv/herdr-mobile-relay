import { afterEach, describe, expect, it, vi } from 'vitest';
import { get } from 'svelte/store';
import { reportAppAnomaly } from '$lib/crash-banner';
import {
  adoptRelaySpeech,
  armSpeechKeepalive,
  initializeSpeech,
  releaseSpeechKeepalive,
  setSpeechEnabled,
  setSpeechLanguage,
  speakViaRelay,
  speechChunks,
  speechEnabled,
  speechLanguage,
  speechState,
  stopSpeech,
} from '$lib/speech';
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
  setSpeechEnabled(false);
  setSpeechLanguage('en');
  securityState.update((state) => ({ ...state, locked: false }));
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('crash banner recovery', () => {
  it('reports a later independent failure after dismissal', () => {
    reportAppAnomaly('first independent test failure');
    const first = document.querySelector<HTMLElement>('[role="alert"]');
    expect(first?.textContent).toContain('first independent test failure');

    first?.click();
    expect(document.querySelector('[role="alert"]')).toBeNull();

    reportAppAnomaly('second independent test failure');
    const second = document.querySelector<HTMLElement>('[role="alert"]');
    expect(second?.textContent).toContain('second independent test failure');
    second?.click();
  });
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

describe('relay speech', () => {
  it('offers only the languages the app supports and remembers the choice', () => {
    setSpeechEnabled(true);
    expect(get(speechState)).toBe('idle');
    expect(get(speechLanguage)).toBe('en');

    setSpeechLanguage('zh');
    expect(get(speechLanguage)).toBe('zh');
    expect(localStorage.getItem('herdr_speech_language')).toBe('zh');

    // A language with no relay voice behind it is never selectable.
    setSpeechLanguage('ja');
    expect(get(speechLanguage)).toBe('zh');

    setSpeechEnabled(false);
    expect(get(speechState)).toBe('off');
  });

  it('reads aloud by default once a relay reports a voice, then obeys the setting', () => {
    localStorage.removeItem('herdr_speech_enabled');
    localStorage.removeItem('herdr_speech_language');
    adoptRelaySpeech(['en']);
    expect(get(speechEnabled)).toBe(true);
    expect(get(speechLanguage)).toBe('en');

    // Turning it off is a decision, so no later relay may undo it.
    setSpeechEnabled(false);
    adoptRelaySpeech(['en', 'fr']);
    expect(get(speechEnabled)).toBe(false);

    // A relay with no voice at all leaves a fresh phone untouched.
    localStorage.removeItem('herdr_speech_enabled');
    adoptRelaySpeech([]);
    expect(get(speechEnabled)).toBe(false);
    expect(localStorage.getItem('herdr_speech_enabled')).toBeNull();

    // The first language is one the relay can actually speak: English is only
    // downloaded by default, so a phone set to German still hears something.
    localStorage.removeItem('herdr_speech_language');
    speechLanguage.set('de');
    adoptRelaySpeech(['en']);
    expect(get(speechLanguage)).toBe('en');

    localStorage.removeItem('herdr_speech_language');
    speechLanguage.set('de');
    adoptRelaySpeech(['fr', 'zh']);
    expect(get(speechLanguage)).toBe('fr');
  });

  it('splits long responses at sentence boundaries, including Chinese', () => {
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
    // Chinese sentences carry no spaces, so their punctuation has to split.
    expect(speechChunks('中继已确认。每一项更改。', 12)).toEqual(['中继已确认。', '每一项更改。']);
  });

  it('streams relay-synthesized audio through one unlocked media element', async () => {
    const elements: FakeMediaElement[] = [];
    class FakeMediaElement {
      src = '';
      paused = true;
      plays = 0;
      onended: (() => void) | null = null;
      onpause: (() => void) | null = null;
      onerror: (() => void) | null = null;

      constructor() {
        elements.push(this);
      }

      play() {
        this.paused = false;
        this.plays++;
        return Promise.resolve();
      }

      pause() {
        this.paused = true;
        this.onpause?.();
      }
    }
    vi.stubGlobal('Audio', FakeMediaElement);
    let urls = 0;
    vi.stubGlobal('URL', class extends URL {
      static createObjectURL = () => `blob:fake-${++urls}`;
      static revokeObjectURL = vi.fn();
    });

    const stop = initializeSpeech();
    setSpeechEnabled(true);
    setSpeechLanguage('fr');

    // The tap unlocks the persistent element with a silent moment before any
    // network round trip; later programmatic plays reuse that unlock.
    armSpeechKeepalive();
    expect(elements).toHaveLength(1);
    const element = elements[0];
    expect(element.plays).toBe(1);

    const sent: { text: string; language: string }[] = [];
    const cancellations: Array<() => void> = [];
    const send = vi.fn((text: string, language: string) => {
      sent.push({ text, language });
      const cancel = vi.fn();
      cancellations.push(cancel);
      return Object.assign(
        Promise.resolve({ data: { audio: btoa(`RIFF-${sent.length}`) } }),
        { cancel },
      );
    });
    const sentence = 'The relay confirmed every change landed as expected.';
    expect(speakViaRelay(Array.from({ length: 12 }, () => sentence).join(' '), send)).toBe(true);
    expect(get(speechState)).toBe('speaking');

    await vi.waitFor(() => expect(element.plays).toBe(2));
    // The next fragment is prefetched while the first one is still playing,
    // and every fragment carries the language the relay must speak.
    expect(sent.length).toBe(2);
    expect(sent.every((fragment) => fragment.language === 'fr')).toBe(true);
    element.onended?.();
    await vi.waitFor(() => expect(element.plays).toBe(3));
    expect(elements).toHaveLength(1);

    // Stopping mid-fragment pauses the element and ends the run.
    stopSpeech();
    expect(cancellations.at(-1)).toHaveBeenCalledOnce();
    expect(element.paused).toBe(true);
    expect(get(speechState)).toBe('idle');
    element.onended?.();
    await Promise.resolve();
    expect(get(speechState)).toBe('idle');
    stop();
  });

  it('reports a relay that answers without audio and releases an abandoned tap', async () => {
    const elements: FakeMediaElement[] = [];
    class FakeMediaElement {
      src = '';
      paused = true;
      onended: (() => void) | null = null;
      onpause: (() => void) | null = null;
      onerror: (() => void) | null = null;

      constructor() {
        elements.push(this);
      }

      play() {
        this.paused = false;
        return Promise.resolve();
      }

      pause() {
        this.paused = true;
        this.onpause?.();
      }
    }
    vi.stubGlobal('Audio', FakeMediaElement);
    vi.stubGlobal('URL', class extends URL {
      static createObjectURL = () => 'blob:fake';
      static revokeObjectURL = vi.fn();
    });

    const stop = initializeSpeech();
    setSpeechEnabled(true);

    // A tap that fetches its text first arms playback inside the activation
    // window; abandoning it releases the element again.
    armSpeechKeepalive();
    expect(elements[0].paused).toBe(false);
    releaseSpeechKeepalive();
    expect(elements[0].paused).toBe(true);

    const issues: string[] = [];
    expect(speakViaRelay('response', () => Promise.resolve({ data: {} }), (message) => issues.push(message))).toBe(true);
    await vi.waitFor(() => expect(issues).toHaveLength(1));
    expect(issues[0]).toContain('no audio');
    expect(get(speechState)).toBe('error');
    stop();
  });

  it('refuses to speak while the app is locked', () => {
    setSpeechEnabled(true);
    securityState.update((state) => ({ ...state, locked: true }));
    expect(speakViaRelay('response', () => Promise.resolve({ data: {} }))).toBe(false);
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
