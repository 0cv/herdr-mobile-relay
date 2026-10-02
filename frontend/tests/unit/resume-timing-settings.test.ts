import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { tick } from 'svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import ResumeTimingSettings from '$components/ResumeTimingSettings.svelte';
import { RESUME_DEADLINE_MS, RESUME_RETENTION_MS, resumeMetrics } from '$lib/resume-metrics';
import { nextSummaryChangeMs } from '$lib/resume-summary';

/** Monotonic clock seen by the app's resume metrics (performance.now). */
let now = 0;

/** Lets microtask refreshes and Svelte updates run. */
async function settle(): Promise<void> {
  for (let index = 0; index < 4; index += 1) {
    await Promise.resolve();
    await tick();
  }
}

/** Time passes for a running page: monotonic, wall clock and timers. */
async function advance(ms: number): Promise<void> {
  now += ms;
  vi.advanceTimersByTime(ms);
  await settle();
}

function attemptsCell(): string {
  const rows = within(screen.getByRole('table')).getAllByRole('row');
  // Body row cells after the row header: wake, attempts, fresh, median, p95.
  return within(rows[1]).getAllByRole('cell')[1].textContent ?? '';
}

async function exportSummary(): Promise<Record<string, any>> {
  await fireEvent.click(screen.getByRole('button', { name: 'Export Redacted Summary' }));
  await settle();
  return JSON.parse((screen.getByLabelText('Redacted resume summary') as HTMLTextAreaElement).value);
}

describe('resume timing settings card', () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] });
    vi.setSystemTime(new Date('2026-10-01T12:00:00Z'));
    now = 10_000;
    vi.spyOn(performance, 'now').mockImplementation(() => now);
    resumeMetrics.setEnabled(true);
    resumeMetrics.reset();
    // One relay that can resume, so the wake enrols a pending sample.
    resumeMetrics.setParticipants(() => [{ id: 'relay-a', hybrid: false }]);
  });

  afterEach(() => {
    resumeMetrics.setParticipants(null);
    resumeMetrics.reset();
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it('exports the deadline outcome of a quiet wake without any later event', async () => {
    resumeMetrics.wake('visible');
    render(ResumeTimingSettings);
    await settle();
    expect(screen.getByRole('status')).toHaveTextContent('1 wake and 1 relay sample recorded.');
    expect(attemptsCell()).toBe('0');

    // The deadline passes with no lifecycle event and no timer firing.
    now += RESUME_DEADLINE_MS + 1_000;
    const summary = await exportSummary();
    expect(summary.groups).toHaveLength(1);
    expect(summary.groups[0]).toMatchObject({
      in_progress: 0,
      valid_attempts: 1,
      fresh_on_time: 0,
      non_completions: { deadline: 1 },
    });
    expect(attemptsCell()).toBe('1');
  });

  it('updates the shown deadline state once while open, and leaves no timer after closing', async () => {
    resumeMetrics.wake('visible');
    const view = render(ResumeTimingSettings);
    await settle();
    expect(attemptsCell()).toBe('0');
    await advance(RESUME_DEADLINE_MS - 1_000);
    expect(attemptsCell()).toBe('0');
    await advance(1_100);
    expect(attemptsCell()).toBe('1');
    // Only the retention expiry is still pending, and only while the card is open.
    expect(vi.getTimerCount()).toBe(1);
    view.unmount();
    expect(vi.getTimerCount()).toBe(0);
  });

  it('drops wakes that left the retention window from the view and the export', async () => {
    resumeMetrics.wake('visible');
    resumeMetrics.hidden();
    render(ResumeTimingSettings);
    await settle();
    expect(screen.getByRole('status')).toHaveTextContent('1 wake and 1 relay sample recorded.');
    await advance(RESUME_RETENTION_MS + 10);
    expect(screen.getByRole('status')).toHaveTextContent('0 wakes and 0 relay samples recorded.');
    expect(screen.queryByRole('table')).toBeNull();

    // An export from a stale view still reflects the ring as it is now.
    resumeMetrics.wake('visible');
    resumeMetrics.hidden();
    await settle();
    now += RESUME_RETENTION_MS + 10;
    vi.setSystemTime(Date.now() + RESUME_RETENTION_MS + 10);
    const summary = await exportSummary();
    expect(summary).toMatchObject({ epochs: 0, samples: 0, groups: [] });
  });

  it('computes the next summary change from deadlines and retention', () => {
    const epoch = (startedAt: number, wallStartedAt: number, closed: boolean) => ({
      trigger: 'visible' as const, lifecycle: null, startedAt, wallStartedAt, hiddenMs: null,
      navigationToAppMs: null, onLine: true, signals: {}, coalesced: 0, unlock: null,
      closed, closedBy: null, samples: [],
    });
    expect(nextSummaryChangeMs([], 0, 0)).toBeNull();
    expect(nextSummaryChangeMs([epoch(1_000, 5_000, false)], 11_000, 15_000)).toBe(RESUME_DEADLINE_MS - 10_000);
    expect(nextSummaryChangeMs([epoch(1_000, 5_000, true)], 11_000, 15_000)).toBe(RESUME_RETENTION_MS - 10_000 + 1);
    // The older of the two clocks decides retention.
    expect(nextSummaryChangeMs([epoch(1_000, 5_000, true)], 11_000, 65_000)).toBe(RESUME_RETENTION_MS - 60_000 + 1);
    expect(nextSummaryChangeMs([epoch(0, 0, false)], RESUME_DEADLINE_MS + 5, 0)).toBe(0);
  });
});
