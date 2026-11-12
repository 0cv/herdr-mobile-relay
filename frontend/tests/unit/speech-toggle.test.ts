import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { get } from 'svelte/store';
import { initializeSpeech, setSpeechEnabled, setSpeechLanguage, speechState, toggleSpeech } from '$lib/speech';

beforeEach(() => {
  vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue(undefined);
  vi.spyOn(HTMLMediaElement.prototype, 'pause').mockImplementation(() => {});
  vi.stubGlobal('URL', class extends URL {
    static createObjectURL = () => 'blob:speech';
    static revokeObjectURL = vi.fn();
  });
  setSpeechEnabled(true);
  setSpeechLanguage('en');
});

afterEach(() => {
  initializeSpeech()();
  setSpeechEnabled(false);
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it('reports an unavailable voice before playing audio or fetching text', async () => {
  const source = vi.fn();
  const send = vi.fn();
  const issue = vi.fn();

  await toggleSpeech(source, ['fr'], send, issue);

  expect(source).not.toHaveBeenCalled();
  expect(send).not.toHaveBeenCalled();
  expect(HTMLMediaElement.prototype.play).not.toHaveBeenCalled();
  expect(issue).toHaveBeenCalledWith('This relay has no English voice; install a Piper voice for it on that computer.');
});

it('starts reading text immediately and stops on the next tap without fetching more text', async () => {
  const cancel = vi.fn();
  const send = vi.fn(() => Object.assign(new Promise<{ data?: Record<string, unknown> }>(() => {}), { cancel }));
  const issue = vi.fn();

  const reading = toggleSpeech('Agent response', ['en'], send, issue);
  expect(HTMLMediaElement.prototype.play).toHaveBeenCalledOnce();
  expect(send).toHaveBeenCalledWith('Agent response', 'en');
  expect(get(speechState)).toBe('speaking');
  await reading;

  const source = vi.fn();
  await toggleSpeech(source, [], send, issue);

  expect(source).not.toHaveBeenCalled();
  expect(send).toHaveBeenCalledOnce();
  expect(cancel).toHaveBeenCalledOnce();
  expect(get(speechState)).toBe('idle');
  expect(issue).not.toHaveBeenCalled();
});

it.each([
  ['', 'No completed agent response is available to read aloud.'],
  ['Could not read the conversation.', 'Could not read the conversation.'],
])('releases audio when fetched text is empty with failure %j', async (failure, message) => {
  let resolve!: (result: { text: string; failure: string }) => void;
  const response = new Promise<{ text: string; failure: string }>((done) => { resolve = done; });
  const source = vi.fn(() => {
    expect(HTMLMediaElement.prototype.play).toHaveBeenCalledOnce();
    return response;
  });
  const send = vi.fn();
  const issue = vi.fn();

  const reading = toggleSpeech(source, ['en'], send, issue);
  expect(source).toHaveBeenCalledOnce();
  expect(send).not.toHaveBeenCalled();
  resolve({ text: ' ', failure });
  await reading;

  expect(send).not.toHaveBeenCalled();
  expect(HTMLMediaElement.prototype.pause).toHaveBeenCalled();
  expect(issue).toHaveBeenCalledWith(message);
});
