<script lang="ts">
  import { decodeQR } from 'qr/decode.js';
  import AppDialog from '$components/ui/AppDialog.svelte';
  import { parseScannedSetupLink } from '$lib/setup-scan';
  import { relayStore } from '$lib/store';

  let { open = $bindable(false) }: { open?: boolean } = $props();
  let video = $state<HTMLVideoElement>();
  let ready = $state(false);
  let cameraError = $state('');
  let refusal = $state('');
  const startError = 'The camera could not be started. Close other apps that use it, then open the scanner again.';

  function cameraErrorMessage(error: unknown): string {
    switch (error && typeof error === 'object' && 'name' in error ? error.name : '') {
      case 'NotAllowedError':
      case 'SecurityError':
        return 'Camera access was denied. Allow the camera for this app in your browser or system settings, then open the scanner again.';
      case 'NotFoundError':
      case 'OverconstrainedError':
        return 'No camera was found on this device.';
      default:
        return startError;
    }
  }

  $effect(() => {
    if (!open || !video) return;
    const preview = video;
    let stopped = false;
    let tracks: MediaStreamTrack[] = [];
    let timer: ReturnType<typeof setTimeout> | undefined;
    let lastText: string | undefined;

    function stop() {
      stopped = true;
      clearTimeout(timer);
      for (const track of tracks) {
        track.removeEventListener('ended', trackEnded);
        track.stop();
      }
      tracks = [];
      preview.srcObject = null;
    }

    function trackEnded() {
      cameraError = startError;
      stop();
    }

    function close() {
      stop();
      open = false;
    }

    function visibilityChanged() {
      if (document.visibilityState === 'hidden') close();
    }

    function readFrame(context: CanvasRenderingContext2D): string | undefined {
      const { videoWidth, videoHeight, readyState } = preview;
      const side = Math.min(videoWidth, videoHeight);
      if (readyState < HTMLMediaElement.HAVE_CURRENT_DATA || !side) return;
      const size = Math.min(side, 720);
      const canvas = context.canvas;
      if (canvas.width !== size || canvas.height !== size) canvas.width = canvas.height = size;
      try {
        context.drawImage(preview, (videoWidth - side) / 2, (videoHeight - side) / 2, side, side, 0, 0, size, size);
        return decodeQR(context.getImageData(0, 0, size, size));
      } catch {
        return;
      }
    }

    function scan(context: CanvasRenderingContext2D) {
      const text = readFrame(context);
      if (text !== undefined && text !== lastText) {
        lastText = text;
        const link = parseScannedSetupLink(text, location.protocol, Date.now());
        const result = typeof link === 'string' ? link : relayStore.importSetupLink(link, true, false);
        if (result === true) {
          close();
          return;
        }
        refusal = result === 'not-setup' ? 'This is not a Herdr setup code.'
          : result === 'expired' ? 'This invitation has expired. Create a new one and scan it.'
            : 'This code could not be used. Print or create a new one and scan it.';
      }
      timer = setTimeout(() => scan(context), 200);
    }

    async function start() {
      if (!navigator.mediaDevices?.getUserMedia) {
        cameraError = 'This browser cannot use the camera here.';
        return;
      }
      try {
        const stream = await navigator.mediaDevices.getUserMedia({
          audio: false,
          video: { facingMode: { ideal: 'environment' }, width: { ideal: 1280 }, height: { ideal: 720 } },
        });
        tracks = stream.getTracks();
        if (stopped) {
          stop();
          return;
        }
        if (tracks.some(track => track.readyState === 'ended')) {
          trackEnded();
          return;
        }
        for (const track of tracks) track.addEventListener('ended', trackEnded);
        preview.srcObject = stream;
        await preview.play();
        if (stopped) return;
        const context = document.createElement('canvas').getContext('2d', { willReadFrequently: true });
        if (!context) throw new Error();
        ready = true;
        timer = setTimeout(() => scan(context), 200);
      } catch (error) {
        if (stopped) return;
        cameraError = cameraErrorMessage(error);
        stop();
      }
    }

    if (document.visibilityState === 'hidden') {
      close();
      return;
    }
    document.addEventListener('visibilitychange', visibilityChanged);
    window.addEventListener('pagehide', close);
    void start();
    return () => {
      document.removeEventListener('visibilitychange', visibilityChanged);
      window.removeEventListener('pagehide', close);
      stop();
    };
  });
</script>

<AppDialog id="setup-scanner" bind:open title="Scan Setup Code" description="Add a computer using its setup QR code.">
  <video bind:this={video} hidden={Boolean(cameraError)} playsinline muted autoplay aria-label="Camera preview" style="width:100%;aspect-ratio:1;object-fit:cover;background:black"></video>
  <p class="hint" role="status">
    {#if !cameraError}{ready ? 'Point the camera at the QR code shown on your computer.' : 'Starting camera…'}{/if}
  </p>
  <p class="error" role="alert">
    {#if cameraError}{cameraError}<br />You can still open the setup link, or add the relay in Settings.{:else}{refusal}{/if}
  </p>
  <button type="button" class="button button-secondary" onclick={() => { open = false; }}>Cancel</button>
</AppDialog>
