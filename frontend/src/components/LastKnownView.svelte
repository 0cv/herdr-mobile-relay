<script lang="ts">
  import { onMount } from 'svelte';
  import type { LastKnownSummary } from '$lib/last-known';
  let { summary, relayLabel }: { summary: LastKnownSummary; relayLabel: string } = $props();
  const observed = $derived(new Date(summary.lastFreshAt));
  let now = $state(Date.now());
  const minutes = $derived(Math.max(0, Math.floor((now - summary.lastFreshAt) / 60_000)));
  onMount(() => {
    const timer = setInterval(() => { now = Date.now(); }, 30_000);
    return () => clearInterval(timer);
  });
</script>

<section class="last-known" aria-label={`${relayLabel} last-known summary`} data-last-known>
  <h2>{relayLabel}</h2>
  <p role="status">Last seen <time datetime={observed.toISOString()}>{observed.toLocaleString()}</time> — reconnecting; read-only</p>
  <p>{minutes} minute{minutes === 1 ? '' : 's'} old. This local summary is not current inventory. It expires within 60 minutes of the time shown.</p>
  {#if summary.truncated}<p>Some labels or rows were shortened or omitted.</p>{/if}
  {#each summary.groups as group (group.id)}
    <h3>{group.label}</h3>
    <ul>
      {#each summary.rows.filter((row) => row.group === group.id) as row (row.id)}
        <li>{row.label} — {row.type}, {row.status}</li>
      {/each}
    </ul>
  {/each}
  <ul>
    {#each summary.rows.filter((row) => row.group === null) as row (row.id)}
      <li>{row.label} — {row.type}, {row.status}</li>
    {/each}
  </ul>
  {#if !summary.rows.length}<p>No agents were in this last-known summary.</p>{/if}
</section>

<style>
  .last-known { margin-block: 1rem; padding: 1rem; border: 1px solid var(--border); border-radius: .75rem; }
  h2, h3 { margin-block: .5rem; }
  p { color: var(--text-muted); }
</style>
