import { readFile } from 'node:fs/promises';
import type { PhaseBudget } from './budget';
import { delay } from './webdriver';

export interface FixtureInfo {
  app_url: string;
  relay_urls: string[];
  setup_urls: string[];
  control_url: string;
  control_secret: string;
  ca_certificate: string;
  old_release: string;
  candidate_release: string;
}

export interface FixtureRequest {
  sequence: number;
  method: string;
  path: string;
  release: string;
  fault?: string;
  fault_id?: string;
  fault_generation?: string;
  status: number;
  content_type: string;
  cache_control: string;
  bytes: number;
  sha256: string;
}

export interface FaultAction {
  action: string;
  id: string;
  generation: string;
  path: string;
  kind?: string;
  policy?: string;
  remaining: number;
  request_count: number;
}

export interface FixtureRelayState {
  name: string;
  invitation_auth_count: number;
  credential_auth_count: number;
  credential_pseudonyms: string[];
  connections: number;
  install_update_count: number;
  deploy_app_update_count: number;
}

export interface FixtureState {
  active_release: string;
  app_url: string;
  invalidated?: boolean;
  invalidation_reason?: string;
  requests: FixtureRequest[];
  request_counts: Record<string, number>;
  fault_actions: FaultAction[];
  faults: Array<{ id: string; generation: string; method: string; path: string; kind: string; remaining: number }>;
  relays: FixtureRelayState[];
}

export async function readJsonFile<T>(filename: string): Promise<T> {
  return JSON.parse(await readFile(filename, 'utf8')) as T;
}

export async function waitForFixtureInfo(filename: string, timeoutMs: number, budget?: PhaseBudget): Promise<FixtureInfo> {
  const deadline = Date.now() + Math.min(timeoutMs, budget?.remainingMs ?? timeoutMs);
  while (Date.now() < deadline) {
    try {
      return await readJsonFile<FixtureInfo>(filename);
    } catch {
      await delay(250, budget);
    }
  }
  throw new Error('FIXTURE_STARTUP: fixture did not publish its private info before the deadline');
}

export async function fixtureControl(info: Pick<FixtureInfo, 'control_url' | 'control_secret'>, path: string, method = 'GET', body?: unknown): Promise<any> {
  const response = await fetch(`${info.control_url}${path}`, {
    method,
    headers: {
      'X-Herdr-Fixture-Secret': info.control_secret,
      ...(body === undefined ? {} : { 'content-type': 'application/json' }),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(30_000),
  });
  const text = await response.text();
  if (!response.ok) throw new Error(`FIXTURE_CONTROL: HTTP ${response.status}: ${text.slice(0, 300)}`);
  return text ? JSON.parse(text) : null;
}

export async function fixtureState(info: Pick<FixtureInfo, 'control_url' | 'control_secret'>): Promise<FixtureState> {
  return fixtureControl(info, '/state') as Promise<FixtureState>;
}

export async function waitForFault(
  info: Pick<FixtureInfo, 'control_url' | 'control_secret'>,
  path: string,
  kind: string,
  faultId: string,
  faultGeneration: string,
  budget?: PhaseBudget,
): Promise<{ state: FixtureState; request: FixtureRequest }> {
  const deadline = Date.now() + Math.min(30_000, budget?.remainingMs ?? 30_000);
  while (Date.now() < deadline) {
    const state = await fixtureState(info);
    if (state.invalidated) throw new Error(`FIXTURE_FAULT_EXPIRED: ${state.invalidation_reason || 'fault lifetime expired'}`);
    const request = [...state.requests].reverse().find((candidate) => candidate.path === path
      && candidate.fault_id === faultId
      && candidate.fault_generation === faultGeneration);
    if (request?.fault === kind && request.status > 0) return { state, request };
    await delay(250, budget);
  }
  throw new Error(`FIXTURE_FAULT: ${kind} for ${path} was not consumed`);
}
