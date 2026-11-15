import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { PANE_LEASE_HIDDEN_GRACE_MS } from '$lib/config';
import { PaneSizeLease, type PaneSizeLeaseHost } from '$lib/pane-size-lease.svelte';
import { deferred, makeAgent } from '../support/relay';

interface Fixture {
  lease: PaneSizeLease;
  host: PaneSizeLeaseHost;
  state: {
    columns: number | null;
    rows: number;
    measurable: boolean;
    rowsEnabled: boolean;
    typing: boolean;
    locked: boolean;
    visible: boolean;
    supported: boolean;
    reportable: boolean;
    frame: { paneId: string } | undefined;
  };
  leases: ReturnType<typeof vi.fn>;
  releases: ReturnType<typeof vi.fn>;
  applied: ReturnType<typeof vi.fn>;
}

const target = makeAgent();
let created: PaneSizeLease[] = [];

function build(): Fixture {
  const state: Fixture['state'] = {
    columns: 80,
    rows: 0,
    measurable: true,
    rowsEnabled: false,
    typing: false,
    locked: false,
    visible: true,
    supported: true,
    reportable: true,
    frame: undefined,
  };
  const leases = vi.fn(async (_target, columns: number, rows: number) => ({ columns, rows }));
  const releases = vi.fn(async () => {});
  const applied = vi.fn();
  const host: PaneSizeLeaseHost = {
    agent: () => target,
    supported: () => state.supported,
    locked: () => state.locked,
    visible: () => state.visible,
    rowsEnabled: () => state.rowsEnabled,
    measure: (withRows) => ({
      columns: state.columns,
      rows: withRows ? state.rows : 0,
      measurable: state.measurable,
    }),
    typing: () => state.typing,
    frame: () => state.frame as never,
    lease: leases,
    release: releases,
    applied,
    canReportRelease: () => state.reportable,
  };
  const lease = new PaneSizeLease(host);
  created.push(lease);
  lease.start();
  return { lease, host, state, leases, releases, applied };
}

const settle = () => vi.advanceTimersByTimeAsync(0);

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(1_000_000);
});

afterEach(() => {
  for (const lease of created) lease.stop();
  created = [];
  vi.useRealTimers();
});

it('keeps one lease in flight and merges later requests into one follow-up', async () => {
  const { lease, state, leases } = build();
  const first = deferred<{ columns: number; rows: number }>();
  leases.mockReturnValueOnce(first.promise);

  lease.request(false);
  state.columns = 90;
  lease.request(false);
  state.columns = 100;
  lease.request(false);
  await settle();
  expect(leases).toHaveBeenCalledTimes(1);
  expect(leases).toHaveBeenLastCalledWith(target, 80, 0);

  first.resolve({ columns: 80, rows: 0 });
  await settle();
  expect(leases).toHaveBeenCalledTimes(2);
  expect(leases).toHaveBeenLastCalledWith(target, 100, 0);
  expect(lease.columns).toBe(100);
});

it('skips an unchanged size unless forced', async () => {
  const { lease, leases } = build();
  lease.request(false);
  await settle();
  expect(leases).toHaveBeenCalledTimes(1);

  lease.request(false);
  await settle();
  expect(leases).toHaveBeenCalledTimes(1);

  lease.request(true);
  await settle();
  expect(leases).toHaveBeenCalledTimes(2);
});

it('sends a merged follow-up as forced when any merged request was forced', async () => {
  const { lease, leases } = build();
  lease.request(false);
  await settle();
  const pending = deferred<{ columns: number; rows: number }>();
  leases.mockReturnValueOnce(pending.promise);
  lease.request(true);
  lease.request(true);
  lease.request(false);
  pending.resolve({ columns: 80, rows: 0 });
  await settle();
  expect(leases).toHaveBeenCalledTimes(3);
});

it('re-reads the pane only when the applied size changes', async () => {
  const { lease, leases, applied } = build();
  lease.request(false);
  await settle();
  expect(applied).toHaveBeenCalledTimes(1);
  expect(applied).toHaveBeenCalledWith(target);

  lease.request(true);
  await settle();
  expect(leases).toHaveBeenCalledTimes(2);
  expect(applied).toHaveBeenCalledTimes(1);

  leases.mockResolvedValueOnce({ columns: 70, rows: 0 });
  lease.request(true);
  await settle();
  expect(lease.columns).toBe(70);
  expect(applied).toHaveBeenCalledTimes(2);
});

it('ignores a reply that belongs to an older generation', async () => {
  const { lease, leases, applied } = build();
  const late = deferred<{ columns: number; rows: number }>();
  leases.mockReturnValueOnce(late.promise);
  lease.request(false);
  await settle();

  lease.discard();
  late.resolve({ columns: 80, rows: 0 });
  await settle();
  expect(lease.columns).toBe(0);
  expect(lease.error).toBe('');
  expect(applied).not.toHaveBeenCalled();
});

it('ignores a failure that belongs to an older generation', async () => {
  const { lease, leases } = build();
  const late = deferred<{ columns: number; rows: number }>();
  leases.mockReturnValueOnce(late.promise);
  lease.request(false);
  await settle();

  lease.discard();
  late.reject(new Error('too late'));
  await settle();
  expect(lease.error).toBe('');
});

it('lets the leased rows grow but not shrink while a text input has focus', async () => {
  const { lease, state, leases } = build();
  state.rowsEnabled = true;
  state.rows = 30;
  lease.request(false);
  await settle();
  expect(leases).toHaveBeenLastCalledWith(target, 80, 30);

  state.typing = true;
  state.rows = 18;
  lease.request(false);
  await settle();
  expect(leases).toHaveBeenCalledTimes(1);

  state.rows = 36;
  lease.request(false);
  await settle();
  expect(leases).toHaveBeenLastCalledWith(target, 80, 36);

  state.typing = false;
  state.rows = 18;
  lease.request(false);
  await settle();
  expect(leases).toHaveBeenLastCalledWith(target, 80, 18);
});

it('sends no rows when rows are not enabled', async () => {
  const { lease, state, leases } = build();
  state.rows = 30;
  lease.request(false);
  await settle();
  expect(leases).toHaveBeenLastCalledWith(target, 80, 0);
});

it('renews inside the hidden grace and stops after it', async () => {
  const { lease, state, leases } = build();
  lease.request(false);
  await settle();
  state.visible = false;
  lease.pageHidden();

  await vi.advanceTimersByTimeAsync(10_000);
  expect(leases).toHaveBeenCalledTimes(2);

  await vi.advanceTimersByTimeAsync(PANE_LEASE_HIDDEN_GRACE_MS);
  const afterGrace = leases.mock.calls.length;
  await vi.advanceTimersByTimeAsync(30_000);
  expect(leases).toHaveBeenCalledTimes(afterGrace);

  state.visible = true;
  lease.pageShown();
  lease.request(true);
  await settle();
  expect(leases).toHaveBeenCalledTimes(afterGrace + 1);
});

it('does not renew while locked, even inside the hidden grace', async () => {
  const { lease, state, leases } = build();
  lease.request(false);
  await settle();
  state.visible = false;
  lease.pageHidden();
  state.locked = true;

  await vi.advanceTimersByTimeAsync(30_000);
  expect(leases).toHaveBeenCalledTimes(1);

  state.locked = false;
  await vi.advanceTimersByTimeAsync(10_000);
  expect(leases).toHaveBeenCalledTimes(2);
});

it('reports a lease failure and a size that cannot be measured', async () => {
  const { lease, state, leases } = build();
  leases.mockRejectedValueOnce(new Error('relay refused'));
  lease.request(false);
  await settle();
  expect(lease.error).toBe('Resize Session failed: relay refused');
  expect(lease.columns).toBe(0);

  state.columns = null;
  lease.request(false);
  expect(lease.error).toBe('Resize Session could not measure the terminal cell width.');
});

it('stays silent when there is nothing to measure yet', () => {
  const { lease, state, leases } = build();
  state.columns = null;
  state.measurable = false;
  lease.request(false);
  expect(lease.error).toBe('');
  expect(leases).not.toHaveBeenCalled();
});

it('clears the error when the lease is synchronised again', async () => {
  const { lease, leases } = build();
  leases.mockRejectedValueOnce(new Error('relay refused'));
  lease.request(false);
  await settle();
  expect(lease.error).not.toBe('');

  lease.sync({
    enabled: true, active: true, readOnly: false, questionMode: false, connected: true,
  });
  expect(lease.error).toBe('');
});

it('sends the first request, the first after discard and a repeat after a failed lease', async () => {
  const { lease, leases } = build();
  lease.request(false);
  await settle();
  expect(leases).toHaveBeenCalledTimes(1);

  lease.discard();
  lease.request(false);
  await settle();
  expect(leases).toHaveBeenCalledTimes(2);

  leases.mockRejectedValueOnce(new Error('relay refused'));
  lease.request(true);
  await settle();
  expect(lease.columns).toBe(0);
  lease.request(false);
  await settle();
  expect(leases).toHaveBeenCalledTimes(4);
  expect(leases).toHaveBeenLastCalledWith(target, 80, 0);
});

it('does nothing when the host says leasing is unsupported', async () => {
  const { lease, state, leases } = build();
  state.supported = false;
  lease.request(true);
  await settle();
  expect(leases).not.toHaveBeenCalled();
});

it('does nothing before start and after stop', async () => {
  const { lease, leases } = build();
  lease.stop();
  lease.request(true);
  await settle();
  expect(leases).not.toHaveBeenCalled();
});

it('reports a release failure when the host allows it', async () => {
  const { lease, releases } = build();
  lease.request(false);
  await settle();
  releases.mockRejectedValueOnce(new Error('relay gone'));
  lease.release(true);
  await settle();
  expect(releases).toHaveBeenCalledWith(target);
  expect(lease.error).toBe('Resize Session release failed: relay gone');
});

it('does not report a release failure when the host says the relay is not connected', async () => {
  const { lease, state, releases } = build();
  lease.request(false);
  await settle();
  state.reportable = false;
  releases.mockRejectedValueOnce(new Error('relay gone'));
  lease.release(true);
  await settle();
  expect(lease.error).toBe('');
});

it('does not report a release failure when the caller asks for silence', async () => {
  const { lease, releases } = build();
  lease.request(false);
  await settle();
  releases.mockRejectedValueOnce(new Error('relay gone'));
  lease.release(false);
  await settle();
  expect(lease.error).toBe('');
});

it('releases without reporting when stopped', async () => {
  const { lease, releases } = build();
  lease.request(false);
  await settle();
  releases.mockRejectedValueOnce(new Error('relay gone'));
  lease.stop();
  await settle();
  expect(releases).toHaveBeenCalledTimes(1);
  expect(lease.error).toBe('');
});

it('releases for question mode and read-only, and only discards when off or disconnected', async () => {
  const { lease, releases } = build();
  const inputs = { enabled: true, active: true, readOnly: false, questionMode: false, connected: true };
  lease.request(false);
  await settle();

  lease.sync({ ...inputs, questionMode: true });
  expect(releases).toHaveBeenCalledTimes(1);
  expect(lease.columns).toBe(0);

  lease.request(false);
  await settle();
  lease.sync({ ...inputs, connected: false });
  expect(releases).toHaveBeenCalledTimes(1);
  expect(lease.columns).toBe(0);

  lease.request(false);
  await settle();
  lease.sync({ ...inputs, enabled: false });
  expect(releases).toHaveBeenCalledTimes(1);

  lease.request(false);
  await settle();
  lease.sync({ ...inputs, readOnly: true });
  expect(releases).toHaveBeenCalledTimes(2);
});

it('requests a lease after a tick once synchronised with a usable session', async () => {
  const { lease, leases } = build();
  lease.sync({
    enabled: true, active: true, readOnly: false, questionMode: false, connected: true,
  });
  expect(leases).not.toHaveBeenCalled();
  await settle();
  expect(leases).toHaveBeenCalledTimes(1);
});

it('stops waiting for a settled frame after four seconds, and discard clears the wait', async () => {
  const { lease, state, leases } = build();
  const frame = { paneId: target.pane_id };
  state.frame = frame;
  const pending = deferred<{ columns: number; rows: number }>();
  leases.mockReturnValueOnce(pending.promise);
  lease.request(false);
  await settle();
  expect(lease.frameBaseline).toBe(frame);
  expect(lease.waitExpired).toBe(false);

  await vi.advanceTimersByTimeAsync(3_999);
  expect(lease.waitExpired).toBe(false);
  await vi.advanceTimersByTimeAsync(1);
  expect(lease.waitExpired).toBe(true);

  lease.discard();
  expect(lease.waitExpired).toBe(false);
  expect(lease.frameBaseline).toBeUndefined();
  pending.resolve({ columns: 80, rows: 0 });
  await settle();

  leases.mockReturnValueOnce(deferred<{ columns: number; rows: number }>().promise);
  lease.request(false);
  await settle();
  lease.discard();
  await vi.advanceTimersByTimeAsync(5_000);
  expect(lease.waitExpired).toBe(false);
});

it('clears the wait when a lease fails', async () => {
  const { lease, state, leases } = build();
  state.frame = { paneId: target.pane_id };
  leases.mockRejectedValueOnce(new Error('relay refused'));
  lease.request(false);
  await settle();
  expect(lease.frameBaseline).toBeUndefined();
  await vi.advanceTimersByTimeAsync(5_000);
  expect(lease.waitExpired).toBe(false);
});
