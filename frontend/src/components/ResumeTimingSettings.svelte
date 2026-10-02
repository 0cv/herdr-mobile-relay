<script lang="ts">
  import { onDestroy, onMount } from 'svelte';
  import AppSwitch from '$components/ui/AppSwitch.svelte';
  import Button from '$components/ui/Button.svelte';
  import Card from '$components/ui/Card.svelte';
  import { resumeMetrics } from '$lib/resume-metrics';
  import {
    formatQuantile,
    nextSummaryChangeMs,
    resumeLifecycleLabel,
    resumePathLabel,
    summarizeResume,
  } from '$lib/resume-summary';

  let enabled = $state(resumeMetrics.enabled);
  let exported = $state('');
  let downloadUrl = $state('');
  let status = $state('');
  let summary = $state(summarizeResume([]));
  let refreshTimer: ReturnType<typeof setTimeout> | undefined;
  let mounted = false;

  /**
   * Re-reads the ring. Deadlines and retention are applied when the ring is
   * read, and time passing changes them without any event, so the summary is
   * recomputed on every recorded change, at export, and once more when the
   * next deadline or retention expiry is due while this card is open. No
   * timer runs while Settings is closed.
   */
  function refresh(): void {
    clearTimeout(refreshTimer);
    refreshTimer = undefined;
    if (!mounted) return;
    const epochs = resumeMetrics.snapshot();
    summary = summarizeResume(epochs);
    const wait = nextSummaryChangeMs(epochs, performance.now(), Date.now());
    if (wait !== null) refreshTimer = setTimeout(refresh, Math.min(wait + 25, 2_147_483_647));
  }

  onMount(() => {
    mounted = true;
    refresh();
    // Reading the ring can itself close an expired wake and bump the
    // revision, so changes are picked up in a microtask, never re-entrantly.
    const stop = resumeMetrics.revision.subscribe(() => queueMicrotask(refresh));
    return () => {
      mounted = false;
      stop();
      clearTimeout(refreshTimer);
    };
  });

  function releaseDownload(): void {
    if (downloadUrl) URL.revokeObjectURL(downloadUrl);
    downloadUrl = '';
  }

  function changeEnabled(next: boolean): void {
    resumeMetrics.setEnabled(next);
    enabled = next;
    exported = '';
    releaseDownload();
    status = next
      ? 'Resume timing is on. Measurements stay in this tab only.'
      : 'Resume timing is off, and recorded timings were cleared.';
  }

  function clearTimings(): void {
    resumeMetrics.clear();
    exported = '';
    releaseDownload();
    status = 'Recorded resume timings were cleared.';
  }

  function exportSummary(): void {
    // Export what the ring holds now, with deadlines and retention applied.
    refresh();
    exported = JSON.stringify(summary, null, 2);
    releaseDownload();
    try {
      downloadUrl = URL.createObjectURL(new Blob([exported], { type: 'application/json' }));
    } catch {
      downloadUrl = '';
    }
    status = `Prepared a redacted summary of ${summary.samples} ${summary.samples === 1 ? 'sample' : 'samples'}.`;
  }

  onDestroy(releaseDownload);
</script>

<Card class="resume-timing" aria-labelledby="resume-timing-title">
  <h3 id="resume-timing-title">Resume Timing</h3>
  <p class="hint" id="resume-timing-description">
    Measures how long this app takes to show fresh agents after it wakes. Up to {summary.retention.max_epochs} wakes
    from the last {summary.retention.max_age_hours} hours stay in this tab's memory and are never uploaded. Only the
    connection path, phase durations and outcomes are kept, never computer names, addresses or content.
  </p>
  <AppSwitch
    checked={enabled}
    label="Measure Resume Timing"
    descriptionId="resume-timing-description"
    onchange={changeEnabled}
  />
  <p class="hint" role="status" aria-live="polite">
    {status || (enabled
      ? `${summary.epochs} ${summary.epochs === 1 ? 'wake' : 'wakes'} and ${summary.samples} relay ${summary.samples === 1 ? 'sample' : 'samples'} recorded.`
      : 'Resume timing is off.')}
  </p>
  {#if enabled && summary.groups.length}
    <div class="resume-table-scroll">
      <table class="resume-table">
        <caption>Time to fresh agents after a wake, all valid attempts, 60 s deadline</caption>
        <thead>
          <tr>
            <th scope="col">Path</th>
            <th scope="col">Wake</th>
            <th scope="col">Attempts</th>
            <th scope="col">Fresh in time</th>
            <th scope="col">Median</th>
            <th scope="col">95th percentile</th>
          </tr>
        </thead>
        <tbody>
          {#each summary.groups as group (`${group.path}|${group.lifecycle}`)}
            <tr>
              <th scope="row">{resumePathLabel(group.path)}</th>
              <td>{resumeLifecycleLabel(group.lifecycle)}</td>
              <td>{group.valid_attempts}</td>
              <td>{group.fresh_on_time}</td>
              <td>{formatQuantile(group.time_to_fresh_ms.p50)}</td>
              <td>{formatQuantile(group.time_to_fresh_ms.p95)}</td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
  <p class="hint">
    The delay before the app's code runs after the phone wakes, and DNS, TCP and TLS inside a connection, cannot be
    observed by a web app and are reported as unavailable rather than estimated.
  </p>
  <div class="form-actions">
    <Button variant="secondary" disabled={!enabled || !summary.samples} onclick={exportSummary}>Export Redacted Summary</Button>
    <Button variant="ghost" disabled={!summary.epochs} onclick={clearTimings}>Clear Resume Timings</Button>
  </div>
  {#if exported}
    <label class="resume-export-label" for="resume-summary-export">Redacted resume summary</label>
    <textarea id="resume-summary-export" class="resume-export" readonly rows="8" value={exported}></textarea>
    {#if downloadUrl}
      <a class="hint" href={downloadUrl} download="herdr-resume-summary.json">Download the redacted summary file</a>
    {/if}
  {/if}
</Card>

<style>
  .resume-table-scroll { margin-top: .5rem; overflow-x: auto; }
  .resume-table { border-collapse: collapse; font-size: .72rem; width: 100%; }
  .resume-table caption { color: var(--muted); text-align: start; }
  .resume-table th, .resume-table td { border-bottom: 1px solid var(--border); padding: .3rem .35rem; text-align: start; }
  .resume-export-label { display: block; margin-top: .6rem; }
  .resume-export { font-family: monospace; font-size: .7rem; width: 100%; }
</style>
