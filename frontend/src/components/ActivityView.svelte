<script lang="ts">
  import { onMount } from 'svelte';
  import AppDialog from '$components/ui/AppDialog.svelte';
  import Button from '$components/ui/Button.svelte';
  import { activityMatchesSearch, activityTone } from '$lib/activity';
  import { dailyActivitySummary, formatWorkingDuration } from '$lib/daily-activity';
  import { navigate } from '$lib/router';
  import { relayStore } from '$lib/store';
  import type { Activity } from '$lib/types';

  const activities = relayStore.activities;
  const agents = relayStore.agents;
  let search = $state('');
  let confirmOpen = $state(false);
  let deleting = $state(false);
  let now = $state(Date.now());
  const visible = $derived($activities.filter((activity) => activityMatchesSearch(activity, search.trim())));
  const daily = $derived(dailyActivitySummary($activities, $agents, now));

  onMount(() => {
    relayStore.requestActivities();
    const timer = setInterval(() => { now = Date.now(); }, 60_000);
    return () => clearInterval(timer);
  });

  function open(activity: Activity) {
    navigate({ view: 'activity_detail', key: activity.activity_key });
  }

  async function deleteAll() {
    if (deleting) return;
    deleting = true;
    try {
      await relayStore.clearActivities();
      confirmOpen = false;
      relayStore.showToast('Activity deleted.');
    } catch (error) {
      confirmOpen = false;
      relayStore.showToast((error as Error).message, true);
    } finally {
      deleting = false;
    }
  }
</script>

<main class="page activity-page" aria-labelledby="activity-title">
  <div class="activity-detail-head activity-toolbar">
    <h2 id="activity-title">Activity</h2>
    <Button variant="danger" size="sm" disabled={!$activities.length || deleting} onclick={() => { confirmOpen = true; }}>Delete all</Button>
  </div>
  <section class="activity-summary" aria-labelledby="activity-summary-title">
    <header>
      <div>
        <h3 id="activity-summary-title">Last 24 hours</h3>
        <p>Across {daily.relays} {daily.relays === 1 ? 'relay' : 'relays'} · retained activity</p>
      </div>
      <time datetime={new Date(daily.since).toISOString()}>Since {new Date(daily.since).toLocaleTimeString([], { hour: 'numeric', minute: '2-digit' })}</time>
    </header>
    <div class="activity-summary-metrics">
      <div><strong>{formatWorkingDuration(daily.workingMs)}</strong><span>Working</span></div>
      <div><strong>{daily.attention}</strong><span>Needed you</span></div>
      <div><strong>{daily.completions}</strong><span>Completed</span></div>
      <div><strong>{daily.actions}</strong><span>Actions</span></div>
    </div>
    {#if daily.agents.length}
      <details>
        <summary>By agent <span>{daily.agents.length}</span></summary>
        <div class="activity-summary-agents">
          {#each daily.agents as item (item.key)}
            <div>
              <span><strong>{item.label}</strong><small>@{item.host}</small></span>
              <span>{formatWorkingDuration(item.workingMs)} · {item.attention} needs · {item.completions} done</span>
            </div>
          {/each}
        </div>
      </details>
    {/if}
  </section>
  <label class="sr-only" for="activity-search">Search activity</label>
  <input id="activity-search" class="activity-search" bind:value={search} type="search" placeholder="Search activity…" />
  <div class="activity-list" aria-live="polite">
    {#if !$activities.length}
      <div class="empty-state">No activity yet.</div>
    {:else if !visible.length}
      <div class="empty-state">No matching activity.</div>
    {/if}
    {#each visible as activity (activity.activity_key)}
      <button type="button" class="agent-card activity-item" onclick={() => open(activity)}>
        <span class="activity-title">
          <span class={`status-dot status-${activityTone(activity.status)}`}></span>
          <strong class="agent-project">{activity.summary || activity.kind || 'Activity'}</strong>
          <time datetime={new Date(Number(activity.timestamp)).toISOString()}>{new Date(Number(activity.timestamp)).toLocaleString([], { dateStyle: 'short', timeStyle: 'short' })}</time>
          <span class="activity-chevron" aria-hidden="true">›</span>
        </span>
        <span class="activity-meta">{[activity.relay_label, activity.project, activity.session, activity.agent, activity.status].filter(Boolean).join(' · ')}</span>
      </button>
    {/each}
  </div>
</main>

<AppDialog
  id="delete-activities-dialog"
  bind:open={confirmOpen}
  title="Delete all activity?"
  description="This permanently deletes the activity history stored by every configured relay."
  dismissible={!deleting}
>
  <p class="hint">Running agents and their conversations are not affected.</p>
  <div class="dialog-actions">
    <Button variant="danger" disabled={deleting} onclick={deleteAll}>{deleting ? 'Deleting…' : 'Delete all'}</Button>
    <Button variant="ghost" disabled={deleting} onclick={() => { confirmOpen = false; }}>Cancel</Button>
  </div>
</AppDialog>
