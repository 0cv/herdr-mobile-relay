import { base64UrlDecode } from './base64url';

export const DEVICE_AUTH_STORAGE_KEY = 'herdr_device_auth_v1';

export type DeviceRole = 'reader' | 'controller';
export type DeviceAuthenticationKind = 'credential' | 'invitation';

export interface RelayDeviceAuthentication {
  kind: DeviceAuthenticationKind;
  id: string;
  version: number;
  secret: string;
}

export interface RelayInvitation extends RelayDeviceAuthentication {
  kind: 'invitation';
  expiresAt: number;
}

export interface RelayDeviceCredential extends RelayDeviceAuthentication {
  kind: 'credential';
  deviceId: string;
  role: DeviceRole;
  locale: string;
  issuedAt: number;
  invitationId?: string;
}

export interface DeviceEnrollmentResult {
  deviceId: string;
  credentialId: string;
  credentialVersion: number;
  credentialSecret?: string;
  role: DeviceRole;
  locale: string;
}

export interface DeviceSummary {
  deviceId: string;
  credentialId: string;
  name: string;
  role: DeviceRole;
  pairedAt: number;
  lastSeenAt?: number;
  current: boolean;
}

export interface RenameDeviceIntent {
  relayId: string;
  deviceId: string;
  name: string;
}

export interface CreateInvitationIntent {
  relayId: string;
  name: string;
  role: DeviceRole;
}

export interface RevokeDeviceIntent {
  relayId: string;
  deviceId: string;
}

export interface ResetDevicesIntent {
  relayId: string;
}

export type DeviceIntentHandler<TIntent, TResult = void> = (intent: TIntent) => Promise<TResult>;

interface PersistedDeviceAuthState {
  version: 1;
  relays: Record<string, RelayInvitation | RelayDeviceCredential>;
}

/**
 * Stores one authentication secret for each relay in ordinary browser storage.
 * This is deliberately not described as a hardware-backed credential store:
 * script running with this origin's privileges can read it.
 */
export class BrowserDeviceCredentialStore {
  constructor(
    private readonly storage: Storage,
    private readonly now: () => number = Date.now,
  ) {}

  get(relayId: string): RelayInvitation | RelayDeviceCredential | null {
    const id = validIdentifier(relayId, 'relay id');
    const entry = this.read().relays[id];
    if (!entry) return null;
    if (entry.kind === 'invitation' && entry.expiresAt <= this.now()) {
      this.remove(id);
      return null;
    }
    return cloneAuthentication(entry);
  }

  saveInvitation(relayId: string, invitation: Omit<RelayInvitation, 'kind'>): RelayInvitation {
    const id = validIdentifier(relayId, 'relay id');
    const entry: RelayInvitation = {
      kind: 'invitation',
      id: validIdentifier(invitation.id, 'invitation id'),
      version: validVersion(invitation.version),
      secret: validSecret(invitation.secret),
      expiresAt: validTimestamp(invitation.expiresAt, 'invitation expiry'),
    };
    if (entry.expiresAt <= this.now()) throw new Error('The device invitation has expired.');
    const state = this.read();
    state.relays[id] = entry;
    this.write(state);
    return cloneAuthentication(entry);
  }

  /**
   * Replaces the one-use invitation and its secret in one localStorage write.
   * A failed write leaves the previously persisted invitation untouched.
   */
  replaceInvitation(
    relayId: string,
    invitationId: string,
    enrollment: DeviceEnrollmentResult & { credentialSecret: string },
  ): RelayDeviceCredential {
    const id = validIdentifier(relayId, 'relay id');
    const expectedInvitationId = validIdentifier(invitationId, 'invitation id');
    const state = this.read();
    const current = state.relays[id];
    if (current?.kind !== 'invitation' || current.id !== expectedInvitationId) {
      throw new Error('The redeemed device invitation is no longer available.');
    }
    const credential = credentialFromEnrollment(enrollment, this.now(), expectedInvitationId);
    state.relays[id] = credential;
    this.write(state);
    return cloneAuthentication(credential);
  }

  updateCredential(relayId: string, enrollment: DeviceEnrollmentResult): RelayDeviceCredential {
    const id = validIdentifier(relayId, 'relay id');
    const state = this.read();
    const current = state.relays[id];
    if (current?.kind !== 'credential') throw new Error('No device credential is stored for this relay.');
    if (current.id !== enrollment.credentialId || current.deviceId !== enrollment.deviceId) {
      throw new Error('The relay authenticated a different device credential.');
    }
    const next = credentialFromEnrollment(
      { ...enrollment, credentialSecret: current.secret },
      current.issuedAt,
      current.invitationId,
    );
    state.relays[id] = next;
    this.write(state);
    return cloneAuthentication(next);
  }

  remove(relayId: string): boolean {
    const id = validIdentifier(relayId, 'relay id');
    const state = this.read();
    if (!state.relays[id]) return false;
    delete state.relays[id];
    this.write(state);
    return true;
  }


  private read(): PersistedDeviceAuthState {
    const raw = this.storage.getItem(DEVICE_AUTH_STORAGE_KEY);
    if (!raw) return emptyState();
    try {
      const parsed = JSON.parse(raw) as unknown;
      if (!isRecord(parsed) || parsed.version !== 1 || !isRecord(parsed.relays)) return emptyState();
      const relays: Record<string, RelayInvitation | RelayDeviceCredential> = {};
      for (const [relayId, value] of Object.entries(parsed.relays)) {
        try {
          const id = validIdentifier(relayId, 'relay id');
          const entry = parseStoredAuthentication(value);
          if (entry) relays[id] = entry;
        } catch {
          // Ignore only the malformed relay entry; another relay may still be usable.
        }
      }
      return { version: 1, relays };
    } catch {
      return emptyState();
    }
  }

  private write(state: PersistedDeviceAuthState): void {
    this.storage.setItem(DEVICE_AUTH_STORAGE_KEY, JSON.stringify(state));
  }
}

export function normalizeDeviceName(value: string): string {
  const name = value.trim().replace(/\s+/g, ' ');
  if (!name) throw new Error('Enter a device name.');
  if (name.length > 64) throw new Error('Device names must be 64 characters or fewer.');
  if (/\p{Cc}/u.test(name)) throw new Error('Device names cannot contain control characters.');
  return name;
}

export function renameDevice<TResult>(
  intent: RenameDeviceIntent,
  send: DeviceIntentHandler<RenameDeviceIntent, TResult>,
): Promise<TResult> {
  return send({
    relayId: validIdentifier(intent.relayId, 'relay id'),
    deviceId: validIdentifier(intent.deviceId, 'device id'),
    name: normalizeDeviceName(intent.name),
  });
}

export function createDeviceInvitation<TResult>(
  intent: CreateInvitationIntent,
  send: DeviceIntentHandler<CreateInvitationIntent, TResult>,
): Promise<TResult> {
  if (intent.role !== 'reader' && intent.role !== 'controller') throw new Error('Choose a valid device role.');
  return send({
    relayId: validIdentifier(intent.relayId, 'relay id'),
    name: normalizeDeviceName(intent.name),
    role: intent.role,
  });
}

export function revokeDevice<TResult>(
  intent: RevokeDeviceIntent,
  send: DeviceIntentHandler<RevokeDeviceIntent, TResult>,
): Promise<TResult> {
  return send({
    relayId: validIdentifier(intent.relayId, 'relay id'),
    deviceId: validIdentifier(intent.deviceId, 'device id'),
  });
}

export function resetDevices<TResult>(
  intent: ResetDevicesIntent,
  send: DeviceIntentHandler<ResetDevicesIntent, TResult>,
): Promise<TResult> {
  return send({ relayId: validIdentifier(intent.relayId, 'relay id') });
}


/**
 * Commits a validated encrypted server finish. Invitation material disappears
 * in the same storage write that saves the issued credential.
 */
export function commitDeviceEnrollment(
  store: BrowserDeviceCredentialStore,
  relayId: string,
  presented: RelayInvitation | RelayDeviceCredential,
  enrollment: DeviceEnrollmentResult,
): RelayDeviceCredential {
  if (presented.kind === 'invitation') {
    if (!enrollment.credentialSecret) throw new Error('Relay did not issue a device credential.');
    return store.replaceInvitation(relayId, presented.id, {
      ...enrollment,
      credentialSecret: enrollment.credentialSecret,
    });
  }
  if (enrollment.credentialSecret) {
    throw new Error('Relay unexpectedly replaced an enrolled device credential.');
  }
  return store.updateCredential(relayId, enrollment);
}

function credentialFromEnrollment(
  enrollment: DeviceEnrollmentResult & { credentialSecret: string },
  issuedAt: number,
  invitationId?: string,
): RelayDeviceCredential {
  return {
    kind: 'credential',
    id: validIdentifier(enrollment.credentialId, 'credential id'),
    version: validVersion(enrollment.credentialVersion),
    secret: validSecret(enrollment.credentialSecret),
    deviceId: validIdentifier(enrollment.deviceId, 'device id'),
    role: validRole(enrollment.role),
    locale: validLocale(enrollment.locale),
    issuedAt: validTimestamp(issuedAt, 'credential issue time'),
    invitationId,
  };
}

function parseStoredAuthentication(value: unknown): RelayInvitation | RelayDeviceCredential | null {
  if (!isRecord(value)) return null;
  const common = {
    id: validIdentifier(value.id, 'authentication id'),
    version: validVersion(value.version),
    secret: validSecret(value.secret),
  };
  if (value.kind === 'invitation') {
    return { kind: 'invitation', ...common, expiresAt: validTimestamp(value.expiresAt, 'invitation expiry') };
  }
  if (value.kind === 'credential') {
    return {
      kind: 'credential',
      ...common,
      deviceId: validIdentifier(value.deviceId, 'device id'),
      role: validRole(value.role),
      locale: validLocale(value.locale),
      issuedAt: validTimestamp(value.issuedAt, 'credential issue time'),
      invitationId: value.invitationId === undefined
        ? undefined
        : validIdentifier(value.invitationId, 'invitation id'),
    };
  }
  return null;
}

function validIdentifier(value: unknown, label: string): string {
  if (typeof value !== 'string') throw new Error(`Invalid ${label}.`);
  const id = value.trim();
  if (!id || id.length > 256 || /[\u0000-\u001f\u007f]/.test(id)) throw new Error(`Invalid ${label}.`);
  return id;
}

function validVersion(value: unknown): number {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 1) {
    throw new Error('Invalid credential version.');
  }
  return value;
}

function validSecret(value: unknown): string {
  if (typeof value !== 'string' || !/^[A-Za-z0-9_-]{43}$/.test(value)) {
    throw new Error('Invalid device authentication secret.');
  }
  let decoded: Uint8Array;
  try {
    decoded = base64UrlDecode(value);
  } catch {
    throw new Error('Invalid device authentication secret.');
  }
  if (decoded.byteLength !== 32) throw new Error('Invalid device authentication secret.');
  return value;
}

function validRole(value: unknown): DeviceRole {
  if (value !== 'reader' && value !== 'controller') throw new Error('Invalid device role.');
  return value;
}

function validLocale(value: unknown): string {
  if (typeof value !== 'string' || !/^[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8})*$/.test(value) || value.length > 35) {
    throw new Error('Invalid device locale.');
  }
  return value;
}

function validTimestamp(value: unknown, label: string): number {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < 0) throw new Error(`Invalid ${label}.`);
  return value;
}

function cloneAuthentication<T extends RelayInvitation | RelayDeviceCredential>(value: T): T {
  return { ...value };
}

function emptyState(): PersistedDeviceAuthState {
  return { version: 1, relays: {} };
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return Boolean(value && typeof value === 'object' && !Array.isArray(value));
}
