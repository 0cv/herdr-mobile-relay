import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import ScanSetupCode from '$components/ScanSetupCode.svelte';
import SetupScanner from '$components/SetupScanner.svelte';
import { relayStore } from '$lib/store';
import * as updates from '$lib/updates';
import { directSetupLink, qrPixels } from '../support/setup-qr';

const scannerProps = { open: true };
const denied = 'Camera access was denied. Allow the camera for this app in your browser or system settings, then open the scanner again.';
const missing = 'No camera was found on this device.';
const unavailable = 'The camera could not be started. Close other apps that use it, then open the scanner again.';
const fallback = 'You can still open the setup link, or add the relay in Settings.';

function stubCamera(getUserMedia?: () => Promise<MediaStream>) {
  vi.stubGlobal('navigator', { mediaDevices: getUserMedia ? { getUserMedia } : undefined });
}

function cameraStream() {
  const tracks = [new EventTarget(), new EventTarget()].map(track => Object.assign(track, { stop: vi.fn() }));
  const stream = { getTracks: () => tracks } as unknown as MediaStream;
  vi.spyOn(HTMLMediaElement.prototype, 'play').mockResolvedValue(undefined);
  vi.spyOn(HTMLMediaElement.prototype, 'readyState', 'get').mockReturnValue(HTMLMediaElement.HAVE_CURRENT_DATA);
  vi.spyOn(HTMLVideoElement.prototype, 'videoWidth', 'get').mockReturnValue(1280);
  vi.spyOn(HTMLVideoElement.prototype, 'videoHeight', 'get').mockReturnValue(720);
  return { tracks, stream };
}

function stubFrames(fixture?: string) {
  const context = {
    canvas: document.createElement('canvas'),
    drawImage: vi.fn(),
    getImageData: vi.fn().mockReturnValue(fixture ? qrPixels(fixture) : undefined),
  };
  vi.spyOn(HTMLCanvasElement.prototype, 'getContext').mockReturnValue(context as unknown as CanvasRenderingContext2D);
  return context;
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe('setup scanner dialog', () => {
  it('shows a titled dialog with a Cancel button and explains unavailable camera access', async () => {
    stubCamera();
    render(SetupScanner, scannerProps);
    const dialog = await screen.findByRole('dialog', { name: 'Scan Setup Code' });
    expect(within(dialog).getByRole('button', { name: 'Cancel' })).toBeEnabled();
    expect(within(dialog).getByRole('alert')).toHaveTextContent(`This browser cannot use the camera here.${fallback}`);
  });

  it.each([
    ['NotAllowedError', denied],
    ['SecurityError', denied],
    ['NotFoundError', missing],
    ['OverconstrainedError', missing],
    ['NotReadableError', unavailable],
    ['AbortError', unavailable],
  ])('explains %s', async (name, message) => {
    stubCamera(vi.fn().mockRejectedValue(new DOMException('', name)));
    render(SetupScanner, scannerProps);
    await vi.waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent(`${message}${fallback}`));
  });
});

describe('setup scanner trigger', () => {
  it('does not appear in an iPhone browser tab', () => {
    vi.stubGlobal('navigator', { standalone: false, userAgent: 'Mozilla/5.0 (iPhone)', maxTouchPoints: 5 });
    render(ScanSetupCode);
    expect(screen.queryByRole('button', { name: 'Scan QR Code' })).not.toBeInTheDocument();
  });

  it('shows a load failure and retries on the next tap', async () => {
    stubCamera();
    const load = vi.spyOn(updates, 'importOrReload').mockRejectedValueOnce(new Error('offline'));
    const toast = vi.spyOn(relayStore, 'showToast').mockImplementation(() => {});
    render(ScanSetupCode);
    const button = screen.getByRole('button', { name: 'Scan QR Code' });
    expect(load).not.toHaveBeenCalled();
    await fireEvent.click(button);
    expect(toast).toHaveBeenCalledExactlyOnceWith('The scanner could not be loaded. Check the connection and try again.', true);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    await fireEvent.click(button);
    expect(await screen.findByRole('dialog', { name: 'Scan Setup Code' })).toBeVisible();
    expect(load).toHaveBeenCalledTimes(2);
  });
});

describe('camera cleanup', () => {
  it.each(['unmount', 'Cancel', 'Escape', 'backdrop', 'hidden', 'pagehide', 'ended'])(
    'stops every track and releases the video on %s', async (exit) => {
      const { stream, tracks } = cameraStream();
      const acquire = vi.fn().mockResolvedValue(stream);
      stubCamera(acquire);
      const frames = stubFrames();
      const { container, unmount } = render(SetupScanner, scannerProps);
      const video = container.querySelector('video')!;
      const dialog = screen.getByRole('dialog');
      await vi.waitFor(() => expect(video.srcObject).toBe(stream));
      vi.useFakeTimers();
      const visibility = vi.spyOn(document, 'visibilityState', 'get').mockReturnValue('hidden');
      switch (exit) {
        case 'unmount': unmount(); break;
        case 'Cancel': await fireEvent.click(within(dialog).getByRole('button', { name: 'Cancel' })); break;
        case 'Escape': await fireEvent(dialog, new Event('cancel')); break;
        case 'backdrop': await fireEvent.click(dialog); break;
        case 'hidden': await fireEvent(document, new Event('visibilitychange')); break;
        case 'pagehide': await fireEvent(window, new Event('pagehide')); break;
        case 'ended': tracks[0].dispatchEvent(new Event('ended')); await tick(); break;
      }
      for (const track of tracks) expect(track.stop).toHaveBeenCalledTimes(1);
      expect(video.srcObject).toBeNull();
      if (exit === 'ended') {
        expect(screen.getByRole('alert')).toHaveTextContent(`${unavailable}${fallback}`);
      } else {
        expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      }
      visibility.mockReturnValue('visible');
      await fireEvent(document, new Event('visibilitychange'));
      await vi.advanceTimersByTimeAsync(400);
      expect(acquire).toHaveBeenCalledTimes(1);
      expect(frames.getImageData).not.toHaveBeenCalled();
    },
  );

  it('stops a stream granted after unmount without attaching it', async () => {
    const { stream, tracks } = cameraStream();
    let grant!: (stream: MediaStream) => void;
    const pending = new Promise<MediaStream>(resolve => { grant = resolve; });
    const acquire = vi.fn().mockReturnValue(pending);
    stubCamera(acquire);
    const { container, unmount } = render(SetupScanner, scannerProps);
    const video = container.querySelector('video')!;
    expect(acquire).toHaveBeenCalledTimes(1);
    unmount();
    grant(stream);
    await vi.waitFor(() => {
      for (const track of tracks) expect(track.stop).toHaveBeenCalledTimes(1);
    });
    expect(video.srcObject).toBeNull();
    expect(video.play).not.toHaveBeenCalled();
  });
});

describe('reading setup codes', () => {
  function startScanner(fixture: string, accepted = true) {
    vi.useFakeTimers();
    const { stream, tracks } = cameraStream();
    stubCamera(vi.fn().mockResolvedValue(stream));
    const frames = stubFrames(fixture);
    const importSetupLink = vi.spyOn(relayStore, 'importSetupLink').mockReturnValue(accepted);
    const { container } = render(SetupScanner, scannerProps);
    return { frames, importSetupLink, tracks, video: container.querySelector('video')! };
  }

  it('imports the decoded link once and closes', async () => {
    const { importSetupLink, tracks, video } = startScanner('setup-qr-direct');
    await vi.advanceTimersByTimeAsync(600);
    const { hash, protocol, host } = new URL(directSetupLink);
    expect(importSetupLink).toHaveBeenCalledExactlyOnceWith({ hash, protocol, host }, true, false);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    for (const track of tracks) expect(track.stop).toHaveBeenCalledTimes(1);
    expect(video.srcObject).toBeNull();
  });

  it('refuses the same unrelated code only once and keeps scanning', async () => {
    const { frames, importSetupLink } = startScanner('other-qr');
    await vi.advanceTimersByTimeAsync(200);
    const alert = screen.getByRole('alert');
    expect(alert).toHaveTextContent('This is not a Herdr setup code.');
    const changes = vi.fn();
    const observer = new MutationObserver(changes);
    observer.observe(alert, { subtree: true, childList: true, characterData: true });
    try {
      await vi.advanceTimersByTimeAsync(600);
      expect(changes).not.toHaveBeenCalled();
      expect(frames.getImageData).toHaveBeenCalledTimes(4);
      expect(importSetupLink).not.toHaveBeenCalled();
      expect(screen.getByRole('dialog')).toBeVisible();
    } finally {
      observer.disconnect();
    }
  });

  it('refuses an expired invitation without calling the store', async () => {
    const { importSetupLink } = startScanner('setup-qr-invitation');
    vi.setSystemTime(4_102_444_800_000);
    await vi.advanceTimersByTimeAsync(200);
    expect(screen.getByRole('alert')).toHaveTextContent('This invitation has expired. Create a new one and scan it.');
    expect(importSetupLink).not.toHaveBeenCalled();
    expect(screen.getByRole('dialog')).toBeVisible();
  });

  it('keeps reading when the setup link could not be imported', async () => {
    const { frames, importSetupLink } = startScanner('setup-qr-direct', false);
    await vi.advanceTimersByTimeAsync(600);
    expect(screen.getByRole('alert')).toHaveTextContent('This code could not be used. Print or create a new one and scan it.');
    expect(importSetupLink).toHaveBeenCalledTimes(1);
    expect(frames.getImageData).toHaveBeenCalledTimes(3);
    expect(screen.getByRole('dialog')).toBeVisible();
  });
});
