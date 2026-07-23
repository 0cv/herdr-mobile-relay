<script lang="ts">
  import { onMount } from 'svelte';
  import Button from '$components/ui/Button.svelte';
  import { activityTone } from '$lib/activity';
  import { clientPaneId } from '$lib/agents';
  import { navigate, replaceView } from '$lib/router';
  import { relayStore } from '$lib/store';

  const activities = relayStore.activities;
  const agents = relayStore.agents;

  let { key }: { key: string } = $props();

  const activity = $derived($activities.find((item) => item.activity_key === key) || null);
  const paneId = $derived(activity ? clientPaneId(activity.relay_id, activity.pane_id || '') : '');
  const agent = $derived(paneId ? $agents.find((item) => item.pane_id === paneId) || null : null);
  const meta = $derived(activity
    ? [activity.relay_label, activity.project, activity.session, activity.agent, activity.status].filter(Boolean).join(' · ')
    : '');
  const when = $derived(activity ? new Date(Number(activity.timestamp)) : null);

  // A cold deep-link (reload on #activity=… or a push that launched the app)
  // may arrive before the relay's activity history streams in.
  onMount(() => relayStore.requestActivities());

  function goToThread() {
    if (!activity?.pane_id) return;
    if (agent) void relayStore.acknowledgePane(agent);
    navigate({ view: 'terminal', paneId });
  }

  async function copyExtract() {
    if (!activity?.extract) return;
    if (!navigator.clipboard?.writeText) {
      relayStore.showToast('Clipboard access is unavailable. Select the text manually.', true);
      return;
    }
    try {
      await navigator.clipboard.writeText(activity.extract);
      relayStore.showToast('Excerpt copied.');
    } catch {
      relayStore.showToast('Could not copy the excerpt. Select it manually.', true);
    }
  }
</script>

<main class="page activity-detail" aria-labelledby="activity-detail-title">
  {#if !activity}
    <div class="empty-state" role="status">Loading activity…</div>
    <Button variant="secondary" onclick={() => replaceView({ view: 'activity' })}>Back to activity</Button>
  {:else}
    <div class="activity-detail-head">
      <span class={`status-dot status-${activityTone(activity.status)}`}></span>
      <h2 id="activity-detail-title">{activity.summary || activity.kind || 'Activity'}</h2>
    </div>
    {#if when}
      <time class="activity-detail-time" datetime={when.toISOString()}>
        {when.toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' })}
      </time>
    {/if}
    {#if meta}<p class="activity-detail-meta">{meta}</p>{/if}

    <div class="activity-detail-actions">
      {#if activity.pane_id && agent}
        <Button onclick={goToThread}>Go to thread →</Button>
      {:else if activity.pane_id}
        <p class="activity-detail-gone">This conversation is no longer running.</p>
      {/if}
    </div>

    <section class="activity-extract" aria-label="Captured excerpt">
      {#if activity.extract}
        <Button variant="ghost" size="icon" class="copy-extract" title="Copy excerpt" aria-label="Copy excerpt" onclick={copyExtract}>
          <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <rect x="9" y="9" width="13" height="13" rx="2"></rect>
            <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path>
          </svg>
        </Button>
        <pre>{activity.extract}</pre>
      {:else}
        <p class="empty-state">No excerpt was captured for this event.</p>
      {/if}
    </section>
  {/if}
</main>
