/**
 * Synthetic, in-page relay used by the resume browser fixtures and the hosted
 * resume benchmark. It answers the app's real E2EE handshake with WebCrypto,
 * speaks the gateway rendezvous and chunk framing, and can emulate a direct
 * WebRTC DataChannel, so the shipped bundle runs its unmodified transports.
 *
 * Nothing here reaches a network: WebSocket and RTCPeerConnection are replaced
 * before the app loads. Hidden durations are emulated as a frozen page (only
 * the wall clock moves), half-open sockets as silent drops, and an outage as
 * connection attempts that stall until restoration and then complete on a
 * TCP-style SYN retransmission schedule. These are deterministic scripted
 * conditions, not a model of any real radio, VPN or carrier network.
 *
 * `resumeFixtureInit` runs inside the page through `page.addInitScript`, so it
 * must stay self-contained: no imports or outer variables.
 */

/**
 * @typedef {'cloudflare' | 'gateway' | 'tailscale-managed' | 'tailscale-byo' | 'tailscale-cli' | null} FixtureIngress
 * @typedef {{
 *   slot: number;
 *   id: string;
 *   label: string;
 *   transport: 'wss' | 'hybrid';
 *   url: string;
 *   gatewayUrl: string;
 *   gatewayRelayId: string;
 *   rendezvousKey: string;
 *   ingress: FixtureIngress;
 *   credentialId: string;
 *   secret: string;
 *   revoked: boolean;
 * }} FixtureRelay
 * @typedef {{
 *   relays: FixtureRelay[];
 *   direct?: boolean;
 *   forceRelay?: boolean;
 *   seed?: number;
 *   latencyMs?: [number, number];
 *   lock?: { delayMs: number; cancel: boolean } | null;
 *   blackholeAtStart?: boolean;
 *   iosTab?: boolean;
 *   connectionEvents?: boolean;
 *   faults?: FixtureFaults;
 *   directWindowMs?: number;
 * }} FixtureConfig
 *   `connectionEvents` gives the page a `navigator.connection` event target
 *   (WebKit has none), so `networkChange()` can report a network handoff.
 *   `directWindowMs` opens a direct-upgrade observation window of that
 *   length at every wake (see `directTimeline`).
 * @typedef {{
 *   workspaceOnly?: boolean;
 *   stale?: boolean;
 *   abandonFirst?: boolean;
 *   burst?: boolean;
 *   holdPaintUntilDirect?: boolean;
 *   holdPaint?: boolean;
 *   delayRefreshMs?: number;
 *   directRefuse?: boolean;
 *   directDelayMs?: number;
 * }} FixtureFaults
 *   Conditions applied after the first wake: `workspaceOnly` answers a
 *   refresh without an agents snapshot, `stale` marks the post-wake
 *   inventory stale, and `abandonFirst` sends the first post-wake snapshot on
 *   a session the relay is abandoning (closed shortly after); none of them
 *   may count as a fresh resume. `burst` sends two fresh agents snapshots
 *   back to back, so the second replaces the first before it paints; that
 *   must still count. `holdPaintUntilDirect` is a test-only widening of the
 *   render-to-paint window: the endpoint's paint check for a qualifying card
 *   waits until a direct WebRTC session has been selected (at most 10 s),
 *   so a direct promotion deterministically lands between render and paint.
 *   `holdPaint` holds post-wake completion checks until `releasePaint()`,
 *   so presentation can change deterministically between scheduling and paint.
 *   `delayRefreshMs` answers each post-wake refresh that much later (keep it
 *   under the app's 2 s probe timeout). `directRefuse` closes every post-wake
 *   direct offer (a failed upgrade); `directDelayMs` connects post-wake
 *   direct attempts that much later (a delayed upgrade).
 */

/**
 * One credential secret per slot; every byte of it is a privacy marker.
 *
 * @param {number} slot
 */
function fixtureSecret(slot) {
  return `Zz9SeCrEtCrEdEnTiAlMaRkEr${String(slot).padStart(2, '0')}${'A'.repeat(16)}`;
}

/**
 * Builds a fixture relay. The hostnames deliberately look like other ingress
 * kinds so tests prove the label comes from the authenticated descriptor.
 *
 * @param {number} slot
 * @param {'wss' | 'hybrid'} transport
 * @param {{ ingress?: FixtureIngress; host?: string; revoked?: boolean }} [options]
 * @returns {FixtureRelay}
 */
export function fixtureRelay(slot, transport, options = {}) {
  const host = options.host || `relay-${slot}-private-host.fixture.invalid`;
  return {
    slot,
    id: `fixture-relay-${slot}`,
    label: `Fixture Laptop ${slot} Private`,
    transport,
    url: transport === 'wss' ? `wss://${host}/ws` : '',
    gatewayUrl: transport === 'hybrid' ? `wss://gateway-${slot}-private.fixture.invalid` : '',
    gatewayRelayId: `GwRelayIdMarker${String(slot).padStart(2, '0')}xxxxx`.slice(0, 22),
    rendezvousKey: `RendezvousKeyMarker${String(slot).padStart(2, '0')}${'B'.repeat(22)}`,
    ingress: options.ingress === undefined ? (transport === 'hybrid' ? 'gateway' : 'cloudflare') : options.ingress,
    credentialId: `fixture-credential-${slot}`,
    secret: fixtureSecret(slot),
    revoked: options.revoked === true,
  };
}

/**
 * Text that must never appear in resume-timing UI, exports or browser logs:
 * relay names and addresses, credentials, identifiers and agent content.
 *
 * @param {FixtureRelay[]} relays
 * @returns {string[]}
 */
export function resumeSensitiveMarkers(relays) {
  const markers = [
    'fixture-host-marker',
    '/home/fixture-private',
    'fixture-device',
    'fixture-session',
    'fixture-terminal',
    'fixture-w',
    'private.fixture.invalid',
    'private-host',
    'Zz9SeCrEt',
    'fixture-credential',
    'GwRelayIdMarker',
    'RendezvousKeyMarker',
    'Fixture Laptop',
    'relay-deferred-private.example',
  ];
  for (const relay of relays) {
    // Agent content: the per-epoch inventory text this relay renders.
    markers.push(`resume-${relay.slot}-`, relay.id, relay.label, relay.secret, relay.credentialId);
    if (relay.url) markers.push(new URL(relay.url).hostname);
    if (relay.gatewayUrl) markers.push(new URL(relay.gatewayUrl).hostname);
  }
  return [...new Set(markers)];
}

/**
 * Installs the synthetic relay in the page. Exposes `window.__resumeFixture`
 * for the harness.
 *
 * @param {FixtureConfig} config
 */
export function resumeFixtureInit(config) {
  const SESSION_KEY = 'herdr_resume_fixture';
  const encoder = new TextEncoder();
  const decoder = new TextDecoder();
  const latencyRange = config.latencyMs || [8, 30];
  // The stable bootstrap document only redirects to the build entry; it must
  // not consume the state an emulated discard leaves for the app document.
  const appDocument = location.pathname.startsWith('/builds/');
  /** @type {{ epoch: number; dateOffset: number; discarded: boolean; wakeAbsolute: number | null }} */
  let persisted = { epoch: 1, dateOffset: 0, discarded: false, wakeAbsolute: null };
  try {
    const saved = JSON.parse(sessionStorage.getItem(SESSION_KEY) || 'null');
    if (saved && typeof saved === 'object') persisted = { ...persisted, ...saved };
  } catch {
    // First load in this tab.
  }

  let seed = ((config.seed || 1) + persisted.epoch * 7919) >>> 0;
  function random() {
    seed = (seed + 0x6d2b79f5) >>> 0;
    let value = seed;
    value = Math.imul(value ^ (value >>> 15), value | 1);
    value ^= value + Math.imul(value ^ (value >>> 7), value | 61);
    return ((value ^ (value >>> 14)) >>> 0) / 4294967296;
  }
  function latency() {
    return latencyRange[0] + Math.floor(random() * (latencyRange[1] - latencyRange[0] + 1));
  }

  const state = {
    epoch: persisted.epoch,
    dateOffset: persisted.dateOffset,
    blackhole: config.blackholeAtStart === true,
    /** @type {any[]} */
    pending: [],
    /** @type {any[]} */
    sockets: [],
    /** @type {any[]} */
    channels: [],
    /** @type {FixtureRelay | null} */
    directRelay: null,
    lock: config.lock || null,
    dials: 0,
    hiddenDials: 0,
    handshakes: 0,
    directHandshakes: 0,
    refreshes: 0,
    bytes: 0,
    hiddenBytes: 0,
    wakeAt: 0,
    snapshotSeq: 0,
    /**
     * Relay-side truth for every agents snapshot marker: which wake epoch it
     * belongs to, whether its inventory was ready and not stale, and the
     * session that carried it.
     *
     * @type {Map<string, { slot: number; epoch: number; authoritative: boolean; session: RelaySession }>}
     */
    snapshots: new Map(),
    faults: { ...(config.faults || {}) },
    /**
     * Sessions in the order the app started receiving application messages
     * on them. The app's hybrid transport promotes a direct path on its first
     * application message and falls back to the gateway when direct dies, so
     * the latest still-live session here is the path the app is using.
     *
     * @type {RelaySession[]}
     */
    selections: [],
    /**
     * Relay-side direct-upgrade events: peer connection created, offer
     * received, offer refused, answer sent, DataChannel open, direct session
     * authenticated, and direct session first used (promoted).
     *
     * @type {Array<{ kind: string; at: number; epoch: number }>}
     */
    directEvents: [],
  };
  let visibility = 'visible';
  /**
   * The direct-upgrade observation window of the latest wake: the session
   * each relay was using at the wake, relays that selected another session
   * since, and, once the window has closed, whether the wake's direct session
   * was still the live path at its end. It is decided at that moment, never
   * from whatever is current when the harness reads it later.
   *
   * @type {{ epoch: number; wakeAt: number; closed: boolean; start: Map<number, any>; switched: Set<number>; kept: Map<number, boolean> } | null}
   */
  let directWindow = null;
  /** @param {string} kind */
  function noteDirect(kind) {
    state.directEvents.push({ kind, at: performance.now(), epoch: state.epoch });
  }
  /** @type {Set<() => void>} */
  const selectionWaiters = new Set();
  /** @param {RelaySession} session */
  function select(session) {
    state.selections = state.selections.filter((entry) => entry !== session && entry.active());
    state.selections.push(session);
    if (session.path === 'webrtc') noteDirect('promoted');
    const slot = session.relay.slot;
    if (directWindow && !directWindow.closed && directWindow.start.get(slot) !== session) directWindow.switched.add(slot);
    for (const waiter of [...selectionWaiters]) waiter();
  }
  /** Opens the current wake's direct-upgrade window, closing it on its own timer. */
  function openDirectWindow() {
    const windowMs = Number(config.directWindowMs) || 0;
    if (!(windowMs > 0)) return;
    /** @type {NonNullable<typeof directWindow>} */
    const current = {
      epoch: state.epoch,
      wakeAt: state.wakeAt,
      closed: false,
      start: new Map(config.relays.map((relay) => /** @type {[number, any]} */ ([relay.slot, currentSession(relay.slot)]))),
      switched: new Set(),
      kept: new Map(),
    };
    directWindow = current;
    setTimeout(() => {
      if (directWindow !== current) return;
      for (const [slot, session] of current.start) {
        current.kept.set(slot, Boolean(session && session.path === 'webrtc' && !current.switched.has(slot)
          && currentSession(slot) === session));
      }
      current.closed = true;
    }, Math.max(0, current.wakeAt + windowMs - performance.now()));
  }
  /**
   * The session currently carrying the app's application traffic for a
   * relay: the most recently selected one that is still the live path.
   *
   * @param {number} slot
   * @returns {RelaySession | null}
   */
  function currentSession(slot) {
    for (let index = state.selections.length - 1; index >= 0; index -= 1) {
      const session = state.selections[index];
      if (session.relay.slot === slot && session.active()) return session;
    }
    return null;
  }
  if (config.connectionEvents && !(/** @type {any} */ (navigator).connection instanceof EventTarget)) {
    Object.defineProperty(navigator, 'connection', { configurable: true, value: new EventTarget() });
  }

  /** @param {boolean} discarded @param {number | null} [wakeAbsolute] */
  function persist(discarded, wakeAbsolute = null) {
    try {
      sessionStorage.setItem(SESSION_KEY, JSON.stringify({
        epoch: state.epoch,
        dateOffset: state.dateOffset,
        discarded,
        wakeAbsolute,
      }));
    } catch {
      // The harness only needs persistence across an emulated discard.
    }
  }
  if (appDocument && persisted.discarded) {
    Object.defineProperty(document, 'wasDiscarded', { configurable: true, value: true });
    // The wake of a discarded page is the reload itself, before the bootstrap
    // redirect: express it on this document's monotonic clock (it is negative).
    if (typeof persisted.wakeAbsolute === 'number') {
      state.wakeAt = persisted.wakeAbsolute - performance.timeOrigin;
      openDirectWindow();
    }
  }
  if (appDocument) persist(false);

  const realDateNow = Date.now.bind(Date);
  Date.now = () => realDateNow() + state.dateOffset;
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibility });
  Object.defineProperty(document, 'hidden', { configurable: true, get: () => visibility === 'hidden' });

  if (config.iosTab) {
    Object.defineProperty(navigator, 'standalone', { configurable: true, value: false });
    Object.defineProperty(navigator, 'userAgent', {
      configurable: true,
      value: 'Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1',
    });
  }

  if (!localStorage.getItem('herdr_relays') && config.relays.length) {
    localStorage.setItem('herdr_relays', JSON.stringify(config.relays.map((relay) => (relay.transport === 'hybrid'
      ? {
        id: relay.id,
        label: relay.label,
        url: '',
        token: '',
        transport: 'hybrid',
        gatewayUrl: relay.gatewayUrl,
        gatewayUrls: [relay.gatewayUrl],
        gatewayRelayId: relay.gatewayRelayId,
        rendezvousKey: relay.rendezvousKey,
        paired: true,
      }
      : { id: relay.id, label: relay.label, url: relay.url, token: '', paired: true }))));
    /** @type {Record<string, unknown>} */
    const credentials = {};
    for (const relay of config.relays) {
      credentials[relay.id] = {
        kind: 'credential',
        id: relay.credentialId,
        version: 1,
        secret: relay.secret,
        deviceId: 'fixture-device',
        role: 'controller',
        locale: 'en',
        issuedAt: realDateNow(),
      };
    }
    localStorage.setItem('herdr_device_auth_v1', JSON.stringify({ version: 1, relays: credentials }));
    if (config.forceRelay) localStorage.setItem('herdr_force_relay', '1');
    if (config.lock) {
      localStorage.setItem('herdr_require_device_unlock', 'true');
      localStorage.setItem('herdr_device_unlock_credential', 'AQID');
    }
  }

  if (config.lock) {
    if (!('PublicKeyCredential' in window)) {
      Object.defineProperty(window, 'PublicKeyCredential', { configurable: true, value: class FixtureCredential {} });
    }
    Object.defineProperty(navigator, 'credentials', {
      configurable: true,
      value: {
        create: async () => null,
        get: () => new Promise((resolve, reject) => {
          const lock = state.lock || { delayMs: 0, cancel: false };
          setTimeout(() => {
            if (lock.cancel) reject(new DOMException('Verification cancelled by the fixture', 'NotAllowedError'));
            else resolve({ id: 'fixture-assertion' });
          }, lock.delayMs);
        }),
      },
    });
  }

  /** @param {Uint8Array} bytes */
  function encode64(bytes) {
    let binary = '';
    for (const byte of bytes) binary += String.fromCharCode(byte);
    return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  }
  /** @param {string} text */
  function decode64(text) {
    const normalized = text.replace(/-/g, '+').replace(/_/g, '/');
    const raw = atob(normalized + '='.repeat((4 - (normalized.length % 4)) % 4));
    return Uint8Array.from(raw, (character) => character.charCodeAt(0));
  }
  /** @param {Uint8Array[]} parts */
  function concat(...parts) {
    const out = new Uint8Array(parts.reduce((total, part) => total + part.length, 0));
    let offset = 0;
    for (const part of parts) {
      out.set(part, offset);
      offset += part.length;
    }
    return out;
  }
  /** @param {number} sequence */
  function frameNonce(sequence) {
    const nonce = new Uint8Array(12);
    new DataView(nonce.buffer).setBigUint64(4, BigInt(sequence), false);
    return nonce;
  }
  /** @param {string} direction @param {number} sequence */
  function frameAad(direction, sequence) {
    const prefix = encoder.encode(`herdr-e2ee-v2 ${direction}`);
    const aad = new Uint8Array(prefix.length + 9);
    aad.set(prefix);
    new DataView(aad.buffer).setBigUint64(prefix.length + 1, BigInt(sequence), false);
    return aad;
  }
  /** @param {Uint8Array} bytes */
  function sealed(bytes) {
    return bytes.length >= 2 && bytes[0] === 2 && bytes[1] === 0;
  }
  /** @param {Uint8Array} logical @param {number} maximum */
  function chunkFrame(logical, maximum) {
    /** @type {Uint8Array[]} */
    const chunks = [];
    let offset = 0;
    let start = true;
    do {
      const header = start ? 6 : 2;
      const take = Math.min(maximum - header, logical.length - offset);
      const end = offset + take >= logical.length;
      const piece = new Uint8Array(header + take);
      piece[0] = 1;
      piece[1] = (start ? 1 : 0) | (end ? 2 : 0);
      if (start) new DataView(piece.buffer).setUint32(2, logical.length, false);
      piece.set(logical.subarray(offset, offset + take), header);
      chunks.push(piece);
      offset += take;
      start = false;
    } while (offset < logical.length);
    return chunks;
  }
  class Reassembly {
    constructor() {
      /** @type {Uint8Array | null} */
      this.buffer = null;
      this.received = 0;
    }
    /** @param {Uint8Array} piece */
    push(piece) {
      const flags = piece[1];
      let body;
      if (flags & 1) {
        this.buffer = new Uint8Array(new DataView(piece.buffer, piece.byteOffset).getUint32(2, false));
        this.received = 0;
        body = piece.subarray(6);
      } else {
        body = piece.subarray(2);
      }
      if (!this.buffer) return null;
      this.buffer.set(body, this.received);
      this.received += body.length;
      if (!(flags & 2)) return null;
      const logical = this.buffer;
      this.buffer = null;
      return logical;
    }
  }

  /**
   * The agent name a snapshot carries. Each snapshot gets its own sequence
   * number, so a later authoritative snapshot changes the rendered card even
   * when an earlier, rejected one belonged to the same epoch.
   *
   * @param {number} slot @param {number} epoch @param {number} seq
   */
  function markerFor(slot, epoch, seq) {
    return `resume-${slot}-${epoch}-${seq}-ok`;
  }
  const MARKER_PATTERN = /resume-(\d+)-(\d+)-(\d+)-ok/;
  /** @param {FixtureRelay} relay @param {string} marker */
  function agentsFor(relay, marker) {
    const workspaceId = `fixture-w${relay.slot}`;
    return [{
      pane_id: `${workspaceId}:p1`,
      workspace_id: workspaceId,
      tab_id: `${workspaceId}:t1`,
      tab_number: 1,
      tab_label: marker,
      // Compact cards show cwd rather than project. Put the snapshot marker
      // first so the actual inventory text (not merely aria-label) identifies
      // this wake even on phone-width, ellipsized cards. Keep the path canary.
      cwd: `/${marker}/home/fixture-private/project`,
      status: 'working',
      project: marker,
      agent: 'codex',
      server_session_id: 'fixture-session',
      terminal_id: `fixture-terminal-${relay.slot}`,
      generation: 1,
      agent_session_id: '',
    }];
  }
  /**
   * Workspace labels never carry an agent marker: a workspace refresh alone
   * must not look like fresh agents.
   *
   * @param {FixtureRelay} relay
   */
  function workspacesFor(relay) {
    const workspaceId = `fixture-w${relay.slot}`;
    return [{
      workspace_id: workspaceId,
      number: relay.slot,
      label: `fixture-ws-${relay.slot}-${state.epoch}`,
      pane_count: 1,
      tab_count: 1,
      cwd: '/home/fixture-private/project',
    }];
  }

  /**
   * The relay side of one encrypted session on one path. It authenticates
   * with the stored device credential exactly as the real relay would.
   */
  class RelaySession {
    /**
     * @param {FixtureRelay} relay
     * @param {'websocket' | 'gateway' | 'webrtc'} path
     * @param {(frame: string | Uint8Array, delivered?: () => void) => void} emit
     * @param {() => void} refuse
     * @param {() => boolean} isOpen
     * @param {() => void} terminate
     */
    constructor(relay, path, emit, refuse, isOpen, terminate) {
      this.relay = relay;
      this.path = path;
      this.emit = emit;
      this.refuse = refuse;
      this.isOpen = isOpen;
      this.terminate = terminate;
      this.abandoned = false;
      /** Whether the first application message has been handed to the app. */
      this.chosen = false;
      /** @type {CryptoKey | null} */
      this.sendKey = null;
      /** @type {CryptoKey | null} */
      this.receiveKey = null;
      this.sendSequence = 0;
      this.receiveSequence = 0;
      this.ready = false;
      this.closed = false;
      /** @type {Promise<void>} */
      this.queue = Promise.resolve();
    }

    /** @param {string | Uint8Array} frame */
    receive(frame) {
      this.queue = this.queue.then(() => this.handle(frame)).catch(() => {
        this.closed = true;
      });
    }

    /** The authenticated path is still the live one: open and not being abandoned. */
    active() {
      return this.ready && !this.closed && !this.abandoned && this.isOpen();
    }

    /** @param {string | Uint8Array} frame */
    async handle(frame) {
      if (this.closed) return;
      if (!this.sendKey) {
        await this.answerHello(JSON.parse(String(frame)));
        return;
      }
      const message = JSON.parse(await this.open(frame));
      if (!this.ready) {
        if (message.type !== 'e2ee_client_finish') throw new Error('unexpected client finish');
        await this.send({
          type: 'e2ee_server_finish',
          version: 2,
          device_id: 'fixture-device',
          credential_id: this.relay.credentialId,
          credential_version: 1,
          role: 'controller',
          locale: 'en',
        });
        this.ready = true;
        if (this.path === 'webrtc') {
          state.directHandshakes += 1;
          noteDirect('authenticated');
        } else state.handshakes += 1;
        await this.snapshot(true);
        return;
      }
      if (message.type === 'refresh_agents') {
        state.refreshes += 1;
        const hold = state.epoch > 1 ? Number(state.faults.delayRefreshMs) || 0 : 0;
        if (hold > 0) await new Promise((resolve) => setTimeout(resolve, hold));
        await this.snapshot(false);
        return;
      }
      if (message.type === 'webrtc_offer' && this.path === 'gateway' && config.direct) {
        noteDirect('offer');
        if (state.epoch > 1 && state.faults.directRefuse) {
          noteDirect('refused');
          await this.send({ type: 'webrtc_closed', request_id: message.request_id, reason: 'refused by the fixture' });
          return;
        }
        state.directRelay = this.relay;
        await this.send({ type: 'webrtc_answer', request_id: message.request_id, sdp: 'v=0 fixture-answer' });
        noteDirect('answer');
      }
    }

    /** @param {Record<string, any>} hello */
    async answerHello(hello) {
      if (hello.type !== 'e2ee_client_hello') throw new Error('unexpected client hello');
      if (this.relay.revoked || hello.auth_id !== this.relay.credentialId) {
        this.closed = true;
        this.refuse();
        return;
      }
      const authKey = await crypto.subtle.importKey(
        'raw', decode64(this.relay.secret), { name: 'HMAC', hash: 'SHA-256' }, false, ['sign'],
      );
      const pair = /** @type {CryptoKeyPair} */ (await crypto.subtle.generateKey(
        { name: 'ECDH', namedCurve: 'P-256' }, true, ['deriveBits'],
      ));
      const serverNonce = crypto.getRandomValues(new Uint8Array(32));
      const serverPublic = new Uint8Array(await crypto.subtle.exportKey('raw', pair.publicKey));
      const clientPublic = decode64(String(hello.public_key));
      const binding = encoder.encode(`herdr-e2ee-v2 auth\0${hello.auth_kind}\0${hello.auth_id}\0${hello.auth_version}\0`);
      const transcript = concat(binding, decode64(String(hello.nonce)), clientPublic, serverNonce, serverPublic);
      /** @param {string} label */
      const tag = async (label) => new Uint8Array(await crypto.subtle.sign(
        'HMAC', authKey, concat(encoder.encode(label), transcript),
      ));
      const proof = await tag('herdr-e2ee-v2 server\0');
      const salt = await tag('herdr-e2ee-v2 key\0');
      const imported = await crypto.subtle.importKey('raw', clientPublic, { name: 'ECDH', namedCurve: 'P-256' }, false, []);
      const shared = new Uint8Array(await crypto.subtle.deriveBits({ name: 'ECDH', public: imported }, pair.privateKey, 256));
      const material = await crypto.subtle.importKey('raw', shared, 'HKDF', false, ['deriveKey']);
      /** @param {string} info @param {KeyUsage} usage */
      const derive = (info, usage) => crypto.subtle.deriveKey(
        { name: 'HKDF', hash: 'SHA-256', salt, info: encoder.encode(info) },
        material,
        { name: 'AES-GCM', length: 256 },
        false,
        [usage],
      );
      this.sendKey = await derive('herdr-e2ee-v2 s2c', 'encrypt');
      this.receiveKey = await derive('herdr-e2ee-v2 c2s', 'decrypt');
      this.emit(JSON.stringify({
        type: 'e2ee_server_hello',
        version: 2,
        nonce: encode64(serverNonce),
        public_key: encode64(serverPublic),
        proof: encode64(proof),
      }));
    }

    /** @param {string | Uint8Array} frame */
    async open(frame) {
      let sequence;
      let ciphertext;
      if (typeof frame === 'string') {
        const parsed = JSON.parse(frame);
        sequence = Number(parsed.sequence);
        ciphertext = decode64(String(parsed.ciphertext));
      } else {
        sequence = Number(new DataView(frame.buffer, frame.byteOffset).getBigUint64(2, false));
        ciphertext = frame.slice(10);
      }
      if (sequence !== this.receiveSequence || !this.receiveKey) throw new Error('unexpected sequence');
      this.receiveSequence += 1;
      const plaintext = await crypto.subtle.decrypt(
        { name: 'AES-GCM', iv: frameNonce(sequence), additionalData: frameAad('c2s', sequence), tagLength: 128 },
        this.receiveKey,
        ciphertext,
      );
      return decoder.decode(plaintext);
    }

    /** @param {Record<string, unknown>} message */
    async send(message) {
      if (this.closed || !this.sendKey) return;
      const sequence = this.sendSequence;
      this.sendSequence += 1;
      // The first application message after the handshake is when the app
      // starts using this path (a direct path is promoted on it).
      /** @type {(() => void) | undefined} */
      let delivered;
      if (this.ready && !this.chosen) {
        this.chosen = true;
        delivered = () => select(this);
      }
      const ciphertext = new Uint8Array(await crypto.subtle.encrypt(
        { name: 'AES-GCM', iv: frameNonce(sequence), additionalData: frameAad('s2c', sequence), tagLength: 128 },
        this.sendKey,
        encoder.encode(JSON.stringify(message)),
      ));
      if (this.path === 'websocket') {
        this.emit(JSON.stringify({ type: 'e2ee', version: 2, sequence, ciphertext: encode64(ciphertext) }), delivered);
        return;
      }
      const frame = new Uint8Array(10 + ciphertext.length);
      frame[0] = 2;
      frame[1] = 0;
      new DataView(frame.buffer).setBigUint64(2, BigInt(sequence), false);
      frame.set(ciphertext, 10);
      this.emit(frame, delivered);
    }

    /** @param {boolean} initial */
    async snapshot(initial) {
      const afterWake = state.epoch > 1;
      const faults = afterWake ? state.faults : {};
      const stale = faults.stale === true;
      const inventory = stale
        ? { state: 'error', stale: true, error_code: 'fixture_stale', last_success_at: Date.now() - 60_000 }
        : { state: 'ready', stale: false, last_success_at: Date.now() };
      const seq = ++state.snapshotSeq;
      const marker = markerFor(this.relay.slot, state.epoch, seq);
      const withAgents = initial || faults.workspaceOnly !== true;
      if (withAgents) {
        state.snapshots.set(marker, { slot: this.relay.slot, epoch: state.epoch, authoritative: !stale, session: this });
      }
      const abandon = faults.abandonFirst === true;
      if (abandon) {
        state.faults.abandonFirst = false;
        // The relay is retiring this path: its last snapshot is still
        // delivered and may even paint, but not on the path the app will use.
        this.abandoned = true;
      }
      const agents = agentsFor(this.relay, marker);
      const workspaces = workspacesFor(this.relay);
      if (initial) {
        /** @type {Record<string, unknown>} */
        const pushConfig = {
          type: 'push_config',
          vapid_public_key: '',
          host: 'fixture-host-marker',
          home: '/home/fixture-private',
          protocol: 3,
          version: 'fixture',
          release_version: '0.0.0',
          revision: 'fixture',
          capabilities: [],
          agent_profiles: [],
          inventory,
        };
        if (this.relay.ingress) pushConfig.ingress = this.relay.ingress;
        await this.send(pushConfig);
        await this.send({ type: 'agents', agents });
        await this.send({ type: 'workspaces', workspaces });
        await this.send({ type: 'activity_history', activities: [] });
        await this.send({ type: 'inventory_status', ...inventory });
        return;
      }
      await this.send({ type: 'inventory_status', ...inventory });
      if (withAgents) await this.send({ type: 'agents', agents });
      if (withAgents && faults.burst === true) {
        const next = markerFor(this.relay.slot, state.epoch, ++state.snapshotSeq);
        state.snapshots.set(next, { slot: this.relay.slot, epoch: state.epoch, authoritative: !stale, session: this });
        await this.send({ type: 'agents', agents: agentsFor(this.relay, next) });
      }
      await this.send({ type: 'workspaces', workspaces });
      if (abandon) this.terminate();
    }
  }

  /** @param {number} size */
  function count(size) {
    state.bytes += size;
    if (visibility === 'hidden') state.hiddenBytes += size;
  }

  class FixtureSocket {
    /** @param {string | URL} url @param {string | string[]} [protocols] */
    constructor(url, protocols) {
      this.url = String(url);
      this.protocol = '';
      this.extensions = '';
      this.readyState = 0;
      this.bufferedAmount = 0;
      this.binaryType = 'blob';
      /** @type {((event: any) => void) | null} */
      this.onopen = null;
      /** @type {((event: any) => void) | null} */
      this.onmessage = null;
      /** @type {((event: any) => void) | null} */
      this.onclose = null;
      /** @type {((event: any) => void) | null} */
      this.onerror = null;
      this.dead = false;
      this.createdAt = performance.now();
      this.delay = latency();
      /** @type {RelaySession | null} */
      this.session = null;
      this.reassembly = new Reassembly();
      this.gateway = this.url.endsWith('/connect');
      this.relay = config.relays.find((relay) => (this.gateway
        ? `${relay.gatewayUrl}/connect` === this.url
        : relay.url === this.url)) || null;
      if (!this.gateway) this.protocol = Array.isArray(protocols) ? String(protocols[0] || '') : String(protocols || '');
      state.dials += 1;
      if (visibility === 'hidden') state.hiddenDials += 1;
      state.sockets.push(this);
      if (!this.relay) {
        setTimeout(() => this.fail(), 0);
        return;
      }
      if (state.blackhole) {
        state.pending.push(this);
        return;
      }
      setTimeout(() => this.accept(), this.delay);
    }

    accept() {
      if (this.readyState !== 0 || !this.relay) return;
      this.readyState = 1;
      this.onopen?.(new Event('open'));
      const relay = this.relay;
      if (this.gateway) {
        this.text(JSON.stringify({ type: 'gateway_hello', proto: 1, nonce: encode64(crypto.getRandomValues(new Uint8Array(32))) }));
        return;
      }
      this.session = new RelaySession(
        relay,
        'websocket',
        (frame, delivered) => this.text(String(frame), delivered),
        () => this.serverClose(4401, ''),
        () => this.isOpen(),
        () => setTimeout(() => this.serverClose(1000, ''), 300),
      );
    }

    isOpen() {
      return this.readyState === 1 && !this.dead;
    }

    /** @param {string | ArrayBuffer | Uint8Array} data */
    send(data) {
      if (this.readyState !== 1 || this.dead) return;
      count(typeof data === 'string' ? data.length : data.byteLength);
      const copy = typeof data === 'string' ? data : new Uint8Array(data instanceof Uint8Array ? data : new Uint8Array(data)).slice();
      setTimeout(() => this.fromClient(copy), Math.floor(this.delay / 2));
    }

    /** @param {string | Uint8Array} data */
    fromClient(data) {
      if (this.dead || this.readyState !== 1 || !this.relay) return;
      if (!this.gateway) {
        this.session?.receive(String(data));
        return;
      }
      if (typeof data === 'string') {
        const message = JSON.parse(data);
        if (message.type !== 'connect' || this.session) return;
        this.text(JSON.stringify({ type: 'ready' }));
        this.session = new RelaySession(
          this.relay,
          'gateway',
          (frame, delivered) => this.binary(frame, delivered),
          () => this.serverClose(1000, 'device_unauthorized'),
          () => this.isOpen(),
          () => setTimeout(() => this.serverClose(1000, ''), 300),
        );
        return;
      }
      const logical = this.reassembly.push(data);
      if (logical) this.session?.receive(sealed(logical) ? logical : decoder.decode(logical));
    }

    /** @param {string} payload @param {() => void} [delivered] */
    text(payload, delivered) {
      count(payload.length);
      setTimeout(() => {
        if (this.dead || this.readyState !== 1) return;
        delivered?.();
        this.onmessage?.({ data: payload });
      }, Math.floor(this.delay / 2));
    }

    /** @param {string | Uint8Array} frame @param {() => void} [delivered] */
    binary(frame, delivered) {
      const bytes = typeof frame === 'string' ? encoder.encode(frame) : frame;
      const pieces = chunkFrame(bytes, 262_144);
      pieces.forEach((piece, index) => {
        count(piece.byteLength);
        setTimeout(() => {
          if (this.dead || this.readyState !== 1) return;
          if (index === pieces.length - 1) delivered?.();
          this.onmessage?.({ data: piece.buffer });
        }, Math.floor(this.delay / 2));
      });
    }

    /** @param {number} code @param {string} reason */
    serverClose(code, reason) {
      if (this.readyState === 3) return;
      this.readyState = 3;
      setTimeout(() => this.onclose?.({ code, reason, wasClean: true }), Math.floor(this.delay / 2));
    }

    /** @param {number} [code] */
    close(code) {
      if (this.readyState === 3) return;
      this.readyState = 3;
      setTimeout(() => this.onclose?.({ code: code ?? 1000, reason: '', wasClean: true }), 0);
    }

    fail() {
      if (this.readyState === 3) return;
      this.readyState = 3;
      this.onerror?.(new Event('error'));
      this.onclose?.({ code: 1006, reason: '', wasClean: false });
    }
  }
  Object.assign(FixtureSocket, { CONNECTING: 0, OPEN: 1, CLOSING: 2, CLOSED: 3 });
  Object.defineProperty(window, 'WebSocket', { configurable: true, writable: true, value: FixtureSocket });

  class FixtureDataChannel {
    /** @param {FixturePeerConnection} peer */
    constructor(peer) {
      this.peer = peer;
      this.readyState = 'connecting';
      this.bufferedAmount = 0;
      this.bufferedAmountLowThreshold = 0;
      this.binaryType = 'blob';
      /** @type {((event: any) => void) | null} */
      this.onopen = null;
      /** @type {((event: any) => void) | null} */
      this.onclose = null;
      /** @type {((event: any) => void) | null} */
      this.onerror = null;
      /** @type {((event: any) => void) | null} */
      this.onmessage = null;
      /** @type {((event: any) => void) | null} */
      this.onbufferedamountlow = null;
      this.dead = false;
      this.reassembly = new Reassembly();
      /** @type {RelaySession | null} */
      this.session = null;
      state.channels.push(this);
    }

    /** @param {FixtureRelay} relay */
    start(relay) {
      if (this.readyState !== 'connecting') return;
      this.readyState = 'open';
      this.session = new RelaySession(
        relay,
        'webrtc',
        (frame, delivered) => this.deliver(frame, delivered),
        () => this.peer.close(),
        () => this.readyState === 'open' && !this.dead && this.peer.connectionState !== 'closed',
        () => setTimeout(() => {
          this.readyState = 'closed';
          this.onclose?.(new Event('close'));
        }, 300),
      );
      noteDirect('open');
      this.onopen?.(new Event('open'));
    }

    /** @param {Uint8Array | ArrayBuffer} data */
    send(data) {
      if (this.readyState !== 'open' || this.dead) return;
      const bytes = new Uint8Array(data instanceof Uint8Array ? data : new Uint8Array(data)).slice();
      count(bytes.byteLength);
      setTimeout(() => {
        if (this.dead || this.readyState !== 'open') return;
        const logical = this.reassembly.push(bytes);
        if (logical) this.session?.receive(sealed(logical) ? logical : decoder.decode(logical));
      }, Math.floor(this.peer.delay / 2));
    }

    /** @param {string | Uint8Array} frame @param {() => void} [delivered] */
    deliver(frame, delivered) {
      const bytes = typeof frame === 'string' ? encoder.encode(frame) : frame;
      const pieces = chunkFrame(bytes, 16_384);
      pieces.forEach((piece, index) => {
        count(piece.byteLength);
        setTimeout(() => {
          if (this.dead || this.readyState !== 'open') return;
          if (index === pieces.length - 1) delivered?.();
          this.onmessage?.({ data: piece.buffer });
        }, Math.floor(this.peer.delay / 2));
      });
    }

    close() {
      this.readyState = 'closed';
    }
  }

  class FixturePeerConnection {
    constructor() {
      this.connectionState = 'new';
      /** @type {RTCSessionDescriptionInit | null} */
      this.localDescription = null;
      /** @type {RTCSessionDescriptionInit | null} */
      this.remoteDescription = null;
      /** @type {((event: any) => void) | null} */
      this.onicecandidate = null;
      /** @type {((event: any) => void) | null} */
      this.onconnectionstatechange = null;
      /** @type {FixtureDataChannel[]} */
      this.channels = [];
      this.delay = latency();
      noteDirect('peer');
    }

    createDataChannel() {
      const channel = new FixtureDataChannel(this);
      this.channels.push(channel);
      return channel;
    }

    async createOffer() {
      return { type: 'offer', sdp: 'v=0 fixture-offer' };
    }

    /** @param {RTCSessionDescriptionInit} description */
    async setLocalDescription(description) {
      this.localDescription = description;
    }

    /** @param {RTCSessionDescriptionInit} description */
    async setRemoteDescription(description) {
      this.remoteDescription = description;
      const relay = state.directRelay;
      const extra = state.epoch > 1 ? Number(state.faults.directDelayMs) || 0 : 0;
      setTimeout(() => {
        if (this.connectionState === 'closed' || !relay) return;
        this.connectionState = 'connected';
        this.onconnectionstatechange?.(new Event('connectionstatechange'));
        for (const channel of this.channels) channel.start(relay);
      }, this.delay + extra);
    }

    async addIceCandidate() {}

    restartIce() {}

    close() {
      this.connectionState = 'closed';
      for (const channel of this.channels) channel.close();
    }
  }
  if (config.direct) {
    Object.defineProperty(window, 'RTCPeerConnection', { configurable: true, writable: true, value: FixturePeerConnection });
  }

  /**
   * First paint of each slot's fresh agents for each epoch, recorded from the
   * page itself so a render that beats the harness's own call still counts
   * from the moment it happened. -1 marks a render whose frame is pending.
   *
   * @type {Map<string, number>}
   */
  const rendered = new Map();
  /**
   * Every agent card marker the scan judged, once per marker and verdict, with
   * only numbers and fixed reason codes.
   *
   * @type {Array<{ slot: number; epoch: number; seq: number; path: string; verdict: string }>}
   */
  const renderLog = [];
  const logged = new Set();
  /** @param {string} marker @param {string} verdict */
  function log(marker, verdict) {
    const key = `${marker}:${verdict}`;
    const match = MARKER_PATTERN.exec(marker);
    if (logged.has(key) || !match) return;
    logged.add(key);
    renderLog.push({
      slot: Number(match[1]),
      epoch: Number(match[2]),
      seq: Number(match[3]),
      path: state.snapshots.get(marker)?.session.path ?? '',
      verdict,
    });
  }
  /** @type {Set<() => void>} */
  const waiters = new Set();
  /** @param {number} slot @param {number} epoch */
  const renderKey = (slot, epoch) => `${slot}:${epoch}`;
  /** Agent markers currently rendered as the accessible name of an agent card. */
  function renderedAgentMarkers() {
    /** @type {Map<string, Element>} */
    const markers = new Map();
    for (const card of document.querySelectorAll('article.agent-card')) {
      const name = card.querySelector('.agent-open')?.getAttribute('aria-label') || '';
      const match = MARKER_PATTERN.exec(name);
      if (match) markers.set(match[0], card);
    }
    return markers;
  }
  /**
   * Why a rendered agent card does not show fresh agents, or null when it
   * does: its snapshot must be ready and non-stale, its card not marked
   * stale, and the session that carried it still the live authenticated path
   * and the one the app is currently using (a gateway draining after a
   * direct promotion is open but no longer current).
   *
   * @param {{ slot: number; authoritative: boolean; session: RelaySession }} record
   * @param {Element} card @param {string} marker
   */
  function rejection(record, card, marker) {
    if (!record.authoritative) return 'not-authoritative';
    if (card.classList.contains('stale')) return 'stale-card';
    if (!record.session.active()) return 'inactive-path';
    if (currentSession(record.slot) !== record.session) return 'not-current-path';
    if (!presented(card, marker)) return 'not-presented';
    return null;
  }
  /** @param {string} color */
  function transparentColor(color) {
    if (color === 'transparent') return true;
    // Computed colors can be legacy rgba(), modern rgb(), or color()/oklch().
    const alpha = color.includes('/') ? color.slice(color.lastIndexOf('/') + 1, -1)
      : color.startsWith('rgba(') ? color.slice(color.lastIndexOf(',') + 1, -1) : null;
    return alpha !== null && Number.parseFloat(alpha) === 0;
  }
  /** @param {CSSStyleDeclaration} style */
  function unpainted(style) {
    if (style.display === 'none' || style.visibility !== 'visible'
      || Number(style.opacity) === 0 || style.contentVisibility === 'hidden') return true;
    // Compositor effects are not a DOM visibility oracle. Conservatively
    // refuse filtered/blended inventory rather than infer its painted pixels.
    if (style.filter !== 'none' || style.mixBlendMode !== 'normal') return true;
    return [style.maskImage, style.getPropertyValue('-webkit-mask-image')]
      .some((mask) => Boolean(mask) && mask !== 'none');
  }
  /** @param {Element} element */
  function paintTreeVisible(element) {
    for (let node = /** @type {Element | null} */ (element); node; node = node.parentElement) {
      const style = getComputedStyle(node);
      if (style.display === 'none' || style.contentVisibility === 'hidden' || style.opacity === '0') return false;
    }
    return true;
  }
  /** @param {DOMRect} a @param {DOMRect} b @param {number} [spread] */
  function overlaps(a, b, spread = 0) {
    return a.left - spread < b.right && a.right + spread > b.left
      && a.top - spread < b.bottom && a.bottom + spread > b.top;
  }
  // Diagnostic codes are fixed vocabulary, never DOM labels/styles or URLs.
  let compositionFailure = 'none';
  /** @param {string} reason */
  function refuseComposition(reason) {
    compositionFailure = reason;
    return false;
  }
  /**
   * Only the shipped, in-flow disclosure glyph is supported generated content.
   * Its host must be disjoint from inventory, with no positioning, overflow
   * paint or compositing effects that could move/extend its ink over the text.
   * All other generated boxes are unknown, even if they ignore pointer events.
   *
   * @param {Element} element @param {string} pseudo
   * @param {CSSStyleDeclaration} style @param {DOMRect[]} rects
   */
  function supportedDisclosure(element, pseudo, style, rects) {
    const shape = {
      host: pseudo === '::before' && element.matches('.workspace-card > summary'),
      content: style.content === '"›"', position: style.position === 'static', index: style.zIndex === 'auto',
      pointer: style.pointerEvents !== 'none', flex: style.flexGrow === '0' && style.flexShrink === '0' && style.flexBasis === 'auto',
      effect: style.filter === 'none' && style.mixBlendMode === 'normal',
      background: style.backgroundImage === 'none' && transparentColor(style.backgroundColor),
      shadow: style.boxShadow === 'none' && style.textShadow === 'none', outline: style.outlineStyle === 'none',
      clip: style.clipPath === 'none', mask: [style.maskImage, style.getPropertyValue('-webkit-mask-image')]
        .every((mask) => !mask || mask === 'none'),
    };
    const failed = Object.entries(shape).find(([, supported]) => !supported);
    if (failed) return refuseComposition(`generated:${failed[0]}`);
    const font = Number.parseFloat(style.fontSize);
    if (!(font > 0 && font <= 32)) return refuseComposition('generated:font');
    for (const value of [style.width, style.height]) {
      if (value !== 'auto' && !(Number.parseFloat(value) >= 0 && Number.parseFloat(value) <= 32)) return refuseComposition('generated:size');
    }
    for (const value of [style.marginTop, style.marginRight, style.marginBottom, style.marginLeft,
      style.paddingTop, style.paddingRight, style.paddingBottom, style.paddingLeft,
      style.borderTopWidth, style.borderRightWidth, style.borderBottomWidth, style.borderLeftWidth]) {
      if (value !== '0px') return refuseComposition('generated:spacing');
    }
    if (![style.getPropertyValue('translate'), style.getPropertyValue('rotate'), style.getPropertyValue('scale')]
      .every((value) => !value || value === 'none')) return refuseComposition('generated:individual-transform');
    // The shipped arrow rotates 0/90 degrees about its center, never translates.
    if (!['none', 'matrix(1, 0, 0, 1, 0, 0)', 'matrix(0, 1, -1, 0, 0, 0)'].includes(style.transform)) return refuseComposition('generated:rotation');
    const host = getComputedStyle(element);
    const bounds = element.getBoundingClientRect();
    if (host.display !== 'flex' || host.alignItems !== 'center' || bounds.height < font * 2
      || Number.parseFloat(host.paddingLeft) < font / 2 || Number.parseFloat(host.paddingRight) < font / 2
      || rects.some((rect) => overlaps(bounds, rect))) return refuseComposition('generated:host-bounds');
    return true;
  }
  /** @param {Element} element @param {CSSStyleDeclaration} style @param {DOMRect[]} rects */
  function supportedShadow(element, style, rects) {
    if (style.boxShadow === 'none') return true;
    // The shipped status-dot ring is bounded, not a viewport-sized shadow.
    if (!element.matches('.agent-identity > .status-dot')
      || !/^rgba?\([^)]*\) 0px 0px 0px 2px$/.test(style.boxShadow)) return false;
    for (let node = /** @type {Element | null} */ (element); node; node = node.parentElement) {
      if (getComputedStyle(node).transform !== 'none') return false;
    }
    const bounds = element.getBoundingClientRect();
    return rects.every((rect) => !overlaps(bounds, rect, 2));
  }
  /** @param {Element} element @param {CSSStyleDeclaration} style @param {DOMRect[]} rects */
  function supportedPointerTransparentBox(element, style, rects) {
    // An empty ordinary leaf (e.g. the shipped nav-update badge) paints only
    // its bounded background/border. No text/replaced content, descendants,
    // border-image outset or overflow effects can extend ink from this box.
    if (element.namespaceURI !== 'http://www.w3.org/1999/xhtml'
      || !['span', 'div'].includes(element.localName) || element.children.length || element.textContent?.trim()
      || style.boxShadow !== 'none' || style.textShadow !== 'none' || style.outlineStyle !== 'none'
      || style.filter !== 'none' || style.mixBlendMode !== 'normal' || style.borderImageSource !== 'none') return false;
    const bounds = element.getBoundingClientRect();
    return rects.every((rect) => !overlaps(bounds, rect));
  }
  /**
   * Pointer hit-testing intentionally ignores pointer-transparent paint and
   * shadows. It can only be an additional check, never our occlusion proof.
   * Reject unsupported compositions independently, without changing pointer
   * styles, inserting probes or altering the page being measured. Conservative
   * refusals remain attempted non-completions, not harness exclusions.
   *
   * @param {DOMRect[]} rects
   */
  function supportedComposition(rects) {
    compositionFailure = 'none';
    if (document.querySelector('dialog[open]') || (CSS.supports('selector(:popover-open)')
      && document.querySelector(':popover-open'))) return refuseComposition('composition:top-layer');
    for (const element of document.querySelectorAll('*')) {
      if (!paintTreeVisible(element)) continue;
      const style = getComputedStyle(element);
      if (element.shadowRoot || element.localName.includes('-') || ['iframe', 'object', 'embed'].includes(element.localName)) return refuseComposition('composition:boundary');
      if (style.visibility === 'visible') {
        if (style.pointerEvents === 'none' && !supportedPointerTransparentBox(element, style, rects)) return refuseComposition('composition:pointer-transparent');
        if (style.textShadow !== 'none' || !supportedShadow(element, style, rects)) return refuseComposition('composition:shadow');
        if (style.outlineStyle !== 'none') return refuseComposition('composition:outline');
        if (style.mixBlendMode !== 'normal') return refuseComposition('composition:blend');
        // Known button-only color effects cannot extend ink beyond their box;
        // any inventory ancestor filter is independently refused by unpainted().
        if (!['none', 'brightness(1.08)', 'grayscale(0.35)'].includes(style.filter)) return refuseComposition('composition:filter');
      }
      for (const pseudo of ['::before', '::after']) {
        const generated = getComputedStyle(element, pseudo);
        if (['none', 'normal', ''].includes(generated.content) || generated.display === 'none'
          || generated.visibility !== 'visible' || generated.opacity === '0') continue;
        if (!supportedDisclosure(element, pseudo, generated, rects)) return false;
      }
    }
    return true;
  }
  /**
   * A range of the actual snapshot-identifying text must paint. A visible
   * article/background/button/logo alone cannot stand in for its inventory.
   * Range geometry scopes clipping and hit-testing to the marker's glyphs;
   * styles are checked from the text parent through the card to the root.
   *
   * @param {Text} text @param {string} marker
   */
  function textPresented(text, marker) {
    const parent = text.parentElement;
    if (!parent) return false;
    const style = getComputedStyle(parent);
    if (!(Number.parseFloat(style.fontSize) > 0) || transparentColor(style.color)
      || transparentColor(style.getPropertyValue('-webkit-text-fill-color'))) return false;
    const start = text.data.indexOf(marker);
    if (start < 0) return false;
    const range = document.createRange();
    range.setStart(text, start);
    range.setEnd(text, start + marker.length);
    const rects = [...range.getClientRects()];
    if (!rects.length || !supportedComposition(rects)) return false;
    for (const rect of rects) {
      if (!(rect.width > 0 && rect.height > 0)) return false;
      let left = Math.max(0, rect.left);
      let top = Math.max(0, rect.top);
      let right = Math.min(innerWidth, rect.right);
      let bottom = Math.min(innerHeight, rect.bottom);
      for (let element = /** @type {Element | null} */ (parent); element; element = element.parentElement) {
        const computed = getComputedStyle(element);
        if (unpainted(computed)) return false;
        const bounds = element.getBoundingClientRect();
        if (['hidden', 'clip', 'scroll', 'auto'].includes(computed.overflowX)) {
          left = Math.max(left, bounds.left);
          right = Math.min(right, bounds.right);
        }
        if (['hidden', 'clip', 'scroll', 'auto'].includes(computed.overflowY)) {
          top = Math.max(top, bounds.top);
          bottom = Math.min(bottom, bounds.bottom);
        }
      }
      // The complete identifying marker must fit, not just an ellipsis or
      // clipped prefix. One CSS pixel permits fractional text-box rounding.
      if (!(right > left && bottom > top) || left > rect.left + 1 || right < rect.right - 1
        || top > rect.top + 1 || bottom < rect.bottom - 1) return false;
      let exposed = false;
      for (const x of [0.5, 0.25, 0.75]) {
        for (const y of [0.5, 0.25, 0.75]) {
          const hit = document.elementFromPoint(left + (right - left) * x, top + (bottom - top) * y);
          if (hit && parent.contains(hit)) exposed = true;
        }
      }
      if (!exposed) return false;
    }
    return true;
  }
  /** @param {Element} card @param {string} marker */
  function presented(card, marker) {
    if (!card.isConnected || document.visibilityState !== 'visible' || document.hidden) return false;
    const inventory = card.querySelector('.agent-open');
    if (!inventory) return false;
    const text = document.createTreeWalker(inventory, NodeFilter.SHOW_TEXT);
    for (let node = text.nextNode(); node; node = text.nextNode()) {
      if (textPresented(/** @type {Text} */ (node), marker)) return true;
    }
    return false;
  }
  /** @type {Set<() => void>} */
  const heldPaints = new Set();
  /** @param {number} slot */
  function holdForDirect(slot) {
    return state.epoch > 1 && state.faults.holdPaintUntilDirect === true
      && currentSession(slot)?.path !== 'webrtc';
  }
  /**
   * Runs the paint check at the next animation frame, or, under the
   * `holdPaintUntilDirect` fault, at the moment a direct session is selected
   * (at most 10 s later), before the app has applied anything it carries.
   *
   * @param {number} slot @param {() => void} check
   */
  function atPaint(slot, check) {
    requestAnimationFrame(() => {
      if (state.epoch > 1 && state.faults.holdPaint) {
        heldPaints.add(check);
        return;
      }
      if (!holdForDirect(slot)) {
        check();
        return;
      }
      const release = () => {
        if (holdForDirect(slot)) return;
        selectionWaiters.delete(release);
        clearTimeout(timer);
        check();
      };
      const timer = setTimeout(() => {
        selectionWaiters.delete(release);
        requestAnimationFrame(check);
      }, 10_000);
      selectionWaiters.add(release);
    });
  }
  /**
   * The success endpoint. An agent card (not a workspace label) must name the
   * current epoch's agents from a snapshot the relay sent with ready,
   * non-stale inventory, on a session that is still the live authenticated
   * path, with no unlock dialog covering it, and its frame must paint.
   */
  function scan() {
    // Inventory behind the unlock dialog is not presented to the user.
    if (!document.body || document.getElementById('unlock-dialog')) return;
    const epoch = state.epoch;
    for (const [marker, card] of renderedAgentMarkers()) {
      const record = state.snapshots.get(marker);
      if (!record || record.epoch !== epoch) continue;
      const key = renderKey(record.slot, epoch);
      if (rendered.has(key)) continue;
      const reason = rejection(record, card, marker);
      if (reason) {
        log(marker, reason);
        continue;
      }
      rendered.set(key, -1);
      atPaint(record.slot, () => paint(record.slot, epoch, key));
    }
  }
  /**
   * The frame scheduled by a qualifying render. It re-reads what this frame
   * actually paints for the relay: a newer fresh snapshot that replaced the
   * first one before the paint counts here, while a card that lost its live
   * or current path, became stale or is covered by the unlock dialog does not.
   *
   * @param {number} slot @param {number} epoch @param {string} key
   */
  function paint(slot, epoch, key) {
    if (epoch !== state.epoch) {
      rendered.delete(key);
      return;
    }
    const covered = Boolean(document.getElementById('unlock-dialog'));
    let painted = null;
    /** @type {[string, string] | null} */
    let retired = null;
    for (const [marker, card] of renderedAgentMarkers()) {
      const record = state.snapshots.get(marker);
      if (!record || record.slot !== slot || record.epoch !== epoch) continue;
      const reason = covered ? 'covered' : rejection(record, card, marker);
      if (!reason) {
        painted = marker;
        break;
      }
      retired ??= [marker, reason];
    }
    if (!painted) {
      rendered.delete(key);
      if (retired) log(retired[0], `retired-before-paint:${retired[1]}`);
      // Whatever replaced it gets its own chance at the next frame.
      scan();
      return;
    }
    rendered.set(key, performance.now());
    log(painted, 'counted');
    for (const waiter of [...waiters]) waiter();
  }
  new MutationObserver(scan).observe(document, { subtree: true, childList: true, characterData: true, attributes: true });
  // Reveals retry from observable presentation changes, not a frame loop.
  // Permanently hidden cards therefore schedule no repeated paint checks.
  document.addEventListener('visibilitychange', scan);
  window.addEventListener('resize', scan);
  window.addEventListener('scroll', scan, true);
  document.addEventListener('transitionend', scan, true);
  document.addEventListener('animationend', scan, true);
  // A change of the current path can qualify a card that is already shown.
  selectionWaiters.add(() => queueMicrotask(scan));

  /** @param {number[]} slots @param {number} timeoutMs */
  function awaitFresh(slots, timeoutMs) {
    const keys = slots.map((slot) => renderKey(slot, state.epoch));
    const done = () => keys.every((key) => (rendered.get(key) ?? -1) >= 0);
    const renderedAt = () => Math.max(...keys.map((key) => rendered.get(key) ?? 0));
    return new Promise((resolve) => {
      if (done()) {
        resolve({ renderedAt: renderedAt() });
        return;
      }
      /** @type {ReturnType<typeof setTimeout> | undefined} */
      let timer;
      const waiter = () => {
        if (!done()) return;
        waiters.delete(waiter);
        clearTimeout(timer);
        resolve({ renderedAt: renderedAt() });
      };
      timer = setTimeout(() => {
        waiters.delete(waiter);
        resolve({ timedOut: true });
      }, Math.max(0, timeoutMs));
      waiters.add(waiter);
      scan();
    });
  }

  /**
   * Time to fresh render for the current epoch, from its wake (or from
   * navigation start after an emulated discard), bounded by the deadline.
   *
   * @param {number[]} slots
   * @param {number} deadlineMs
   */
  function measure(slots, deadlineMs) {
    const wakeAt = state.wakeAt;
    return awaitFresh(slots, wakeAt + deadlineMs - performance.now())
      .then((result) => ({ ...result, wakeAt }));
  }

  /** @param {number} slot */
  function hasRendered(slot) {
    for (const key of rendered.keys()) if (key.startsWith(`${slot}:`)) return true;
    return false;
  }

  const api = {
    get epoch() { return state.epoch; },
    get wakeAt() { return state.wakeAt; },
    hide() {
      visibility = 'hidden';
      document.dispatchEvent(new Event('visibilitychange'));
    },
    /** @param {{ pageshow?: boolean; online?: boolean; change?: boolean }} [options] */
    show(options = {}) {
      state.epoch += 1;
      persist(false);
      visibility = 'visible';
      state.wakeAt = performance.now();
      openDirectWindow();
      if (options.pageshow) window.dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true }));
      document.dispatchEvent(new Event('visibilitychange'));
      window.dispatchEvent(new Event('focus'));
      if (options.online) window.dispatchEvent(new Event('online'));
      return state.wakeAt;
    },
    /** A frozen page: only the wall clock advances. @param {number} ms */
    freeze(ms) {
      state.dateOffset += ms;
      persist(false);
    },
    /** Every open socket and DataChannel goes silently half-open. */
    killConnections() {
      for (const socket of state.sockets) if (socket.readyState === 1) socket.dead = true;
      for (const channel of state.channels) channel.dead = true;
    },
    blackhole() {
      state.blackhole = true;
    },
    /** @param {boolean} [online] */
    restore(online) {
      state.blackhole = false;
      const restoredAt = performance.now();
      for (const socket of state.pending.splice(0)) {
        const age = restoredAt - socket.createdAt;
        const retry = [1_000, 3_000, 7_000, 15_000, 31_000].find((at) => at >= age);
        if (retry === undefined) {
          socket.fail();
          continue;
        }
        setTimeout(() => socket.accept(), retry - age + socket.delay);
      }
      if (online) window.dispatchEvent(new Event('online'));
      return restoredAt;
    },
    /**
     * Arms the next load as a discarded page with a new inventory epoch, and
     * records the reload start as its wake on the shared absolute clock.
     */
    prepareDiscard() {
      state.epoch += 1;
      persist(true, performance.timeOrigin + performance.now());
      return state.epoch;
    },
    /** True once the app document (not the bootstrap redirect) is running. */
    ready() {
      return appDocument && Boolean(document.getElementById('app'));
    },
    /** @param {{ delayMs: number; cancel: boolean }} next */
    setUnlock(next) {
      state.lock = next;
    },
    /**
     * The app keeps its socket but reports a network handoff (Wi-Fi to
     * cellular): a new measured epoch starts at the change event.
     */
    networkChange() {
      state.epoch += 1;
      persist(false);
      state.wakeAt = performance.now();
      openDirectWindow();
      /** @type {any} */ (navigator).connection?.dispatchEvent(new Event('change'));
      return state.wakeAt;
    },
    /** @param {number} slot */
    currentPath(slot) {
      return currentSession(slot)?.path ?? null;
    },
    /**
     * The raw direct-upgrade events of the current epoch since its wake
     * (peer connection created, offer, refusal, answer, DataChannel open,
     * direct authentication, promotion), as ms offsets from the wake, and the
     * state of the wake's observation window: whether it has closed and, if
     * so, whether the relay's direct session from the wake was still its live
     * path at the end. Fixed names and numbers only; the runner bounds them
     * to the window (`directUpgradeRecord`). The fixture's direct path serves
     * a single relay, so events are not split by slot.
     *
     * @param {number} slot
     */
    directTimeline(slot) {
      const current = directWindow && directWindow.epoch === state.epoch ? directWindow : null;
      return {
        events: state.directEvents
          .filter((event) => event.epoch === state.epoch && event.at >= state.wakeAt)
          .map((event) => ({ kind: event.kind, at_ms: event.at - state.wakeAt })),
        window_closed: Boolean(current?.closed),
        kept_direct: current?.closed ? current.kept.get(slot) === true : null,
      };
    },
    presentationFailure() { return compositionFailure; },
    pendingPaints() { return heldPaints.size; },
    releasePaint() {
      state.faults.holdPaint = false;
      for (const check of heldPaints) requestAnimationFrame(check);
      heldPaints.clear();
    },
    awaitFresh,
    measure,
    hasRendered,
    renderLog() {
      return renderLog.map((entry) => ({ ...entry }));
    },
    stats() {
      return {
        dials: state.dials,
        hiddenDials: state.hiddenDials,
        handshakes: state.handshakes,
        directHandshakes: state.directHandshakes,
        refreshes: state.refreshes,
        bytes: state.bytes,
        hiddenBytes: state.hiddenBytes,
      };
    },
  };
  Object.defineProperty(window, '__resumeFixture', { configurable: true, value: api });
}
