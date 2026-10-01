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
 * }} FixtureConfig
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
 * The inventory text a relay slot renders for one wake epoch.
 *
 * @param {number} slot
 * @param {number} epoch
 */
export function resumeMarker(slot, epoch) {
  return `resume-${slot}-${epoch}-ok`;
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
  /** @type {{ epoch: number; dateOffset: number; discarded: boolean }} */
  let persisted = { epoch: 1, dateOffset: 0, discarded: false };
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
  };
  let visibility = 'visible';

  /** @param {boolean} discarded */
  function persist(discarded) {
    try {
      sessionStorage.setItem(SESSION_KEY, JSON.stringify({ epoch: state.epoch, dateOffset: state.dateOffset, discarded }));
    } catch {
      // The harness only needs persistence across an emulated discard.
    }
  }
  if (persisted.discarded) {
    Object.defineProperty(document, 'wasDiscarded', { configurable: true, value: true });
  }
  persist(false);

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

  /** @param {number} slot @param {number} epoch */
  function markerFor(slot, epoch) {
    return `resume-${slot}-${epoch}-ok`;
  }
  /** @param {FixtureRelay} relay */
  function inventoryFor(relay) {
    const marker = markerFor(relay.slot, state.epoch);
    const workspaceId = `fixture-w${relay.slot}`;
    return {
      agents: [{
        pane_id: `${workspaceId}:p1`,
        workspace_id: workspaceId,
        tab_id: `${workspaceId}:t1`,
        tab_number: 1,
        tab_label: marker,
        cwd: '/home/fixture-private/project',
        status: 'working',
        project: marker,
        agent: 'codex',
        server_session_id: 'fixture-session',
        terminal_id: `fixture-terminal-${relay.slot}`,
        generation: 1,
        agent_session_id: '',
      }],
      workspaces: [{
        workspace_id: workspaceId,
        number: relay.slot,
        label: marker,
        pane_count: 1,
        tab_count: 1,
        cwd: '/home/fixture-private/project',
      }],
    };
  }

  /**
   * The relay side of one encrypted session on one path. It authenticates
   * with the stored device credential exactly as the real relay would.
   */
  class RelaySession {
    /**
     * @param {FixtureRelay} relay
     * @param {'websocket' | 'gateway' | 'webrtc'} path
     * @param {(frame: string | Uint8Array) => void} emit
     * @param {() => void} refuse
     */
    constructor(relay, path, emit, refuse) {
      this.relay = relay;
      this.path = path;
      this.emit = emit;
      this.refuse = refuse;
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
        if (this.path === 'webrtc') state.directHandshakes += 1;
        else state.handshakes += 1;
        await this.snapshot(true);
        return;
      }
      if (message.type === 'refresh_agents') {
        state.refreshes += 1;
        await this.snapshot(false);
        return;
      }
      if (message.type === 'webrtc_offer' && this.path === 'gateway' && config.direct) {
        state.directRelay = this.relay;
        await this.send({ type: 'webrtc_answer', request_id: message.request_id, sdp: 'v=0 fixture-answer' });
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
      const ciphertext = new Uint8Array(await crypto.subtle.encrypt(
        { name: 'AES-GCM', iv: frameNonce(sequence), additionalData: frameAad('s2c', sequence), tagLength: 128 },
        this.sendKey,
        encoder.encode(JSON.stringify(message)),
      ));
      if (this.path === 'websocket') {
        this.emit(JSON.stringify({ type: 'e2ee', version: 2, sequence, ciphertext: encode64(ciphertext) }));
        return;
      }
      const frame = new Uint8Array(10 + ciphertext.length);
      frame[0] = 2;
      frame[1] = 0;
      new DataView(frame.buffer).setBigUint64(2, BigInt(sequence), false);
      frame.set(ciphertext, 10);
      this.emit(frame);
    }

    /** @param {boolean} initial */
    async snapshot(initial) {
      const { agents, workspaces } = inventoryFor(this.relay);
      const inventory = { state: 'ready', stale: false, last_success_at: Date.now() };
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
      await this.send({ type: 'agents', agents });
      await this.send({ type: 'workspaces', workspaces });
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
      this.session = new RelaySession(relay, 'websocket', (frame) => this.text(String(frame)), () => this.serverClose(4401, ''));
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
        this.session = new RelaySession(this.relay, 'gateway', (frame) => this.binary(frame), () => this.serverClose(1000, 'device_unauthorized'));
        return;
      }
      const logical = this.reassembly.push(data);
      if (logical) this.session?.receive(sealed(logical) ? logical : decoder.decode(logical));
    }

    /** @param {string} payload */
    text(payload) {
      count(payload.length);
      setTimeout(() => {
        if (!this.dead && this.readyState === 1) this.onmessage?.({ data: payload });
      }, Math.floor(this.delay / 2));
    }

    /** @param {string | Uint8Array} frame */
    binary(frame) {
      const bytes = typeof frame === 'string' ? encoder.encode(frame) : frame;
      for (const piece of chunkFrame(bytes, 262_144)) {
        count(piece.byteLength);
        setTimeout(() => {
          if (!this.dead && this.readyState === 1) this.onmessage?.({ data: piece.buffer });
        }, Math.floor(this.delay / 2));
      }
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
      this.session = new RelaySession(relay, 'webrtc', (frame) => this.deliver(frame), () => this.peer.close());
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

    /** @param {string | Uint8Array} frame */
    deliver(frame) {
      const bytes = typeof frame === 'string' ? encoder.encode(frame) : frame;
      for (const piece of chunkFrame(bytes, 16_384)) {
        count(piece.byteLength);
        setTimeout(() => {
          if (!this.dead && this.readyState === 'open') this.onmessage?.({ data: piece.buffer });
        }, Math.floor(this.peer.delay / 2));
      }
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
      setTimeout(() => {
        if (this.connectionState === 'closed' || !relay) return;
        this.connectionState = 'connected';
        this.onconnectionstatechange?.(new Event('connectionstatechange'));
        for (const channel of this.channels) channel.start(relay);
      }, this.delay);
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
   * First paint of each slot's inventory for each epoch, recorded from the
   * page itself so a render that beats the harness's own call still counts
   * from the moment it happened. -1 marks a render whose frame is pending.
   *
   * @type {Map<string, number>}
   */
  const rendered = new Map();
  /** @type {Set<() => void>} */
  const waiters = new Set();
  /** @param {number} slot @param {number} epoch */
  const renderKey = (slot, epoch) => `${slot}:${epoch}`;
  function scan() {
    const epoch = state.epoch;
    const pending = config.relays.filter((relay) => !rendered.has(renderKey(relay.slot, epoch)));
    // Inventory behind the unlock dialog is not presented to the user.
    if (!pending.length || !document.body || document.getElementById('unlock-dialog')) return;
    const text = document.body.textContent || '';
    for (const relay of pending) {
      if (!text.includes(markerFor(relay.slot, epoch))) continue;
      const key = renderKey(relay.slot, epoch);
      rendered.set(key, -1);
      // The inventory is in the DOM; the next animation frame paints it.
      requestAnimationFrame(() => {
        rendered.set(key, performance.now());
        for (const waiter of [...waiters]) waiter();
      });
    }
  }
  new MutationObserver(scan).observe(document, { subtree: true, childList: true, characterData: true, attributes: true });

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
    /** Arms the next load as a discarded page with a new inventory epoch. */
    prepareDiscard() {
      state.epoch += 1;
      persist(true);
      return state.epoch;
    },
    /** @param {{ delayMs: number; cancel: boolean }} next */
    setUnlock(next) {
      state.lock = next;
    },
    awaitFresh,
    measure,
    hasRendered,
    marker: markerFor,
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
