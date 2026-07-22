<script lang="ts">
  import { onMount } from 'svelte';
  import AppDialog from '$components/ui/AppDialog.svelte';
  import Button from '$components/ui/Button.svelte';
  import { activityMatchesSearch, activityTone } from '$lib/activity';
  import { navigate } from '$lib/router';
  import { relayStore } from '$lib/store';
  import type { Activity } from '$lib/types';

  const activities = relayStore.activities;
  let search = $state('');
  let confirmOpen = $state(false);
  let deleting = $state(false);
  const visible = $derived($activities.filter((activity) => activityMatchesSearch(activity, search.trim())));

  onMount(() => relayStore.requestActivities());

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
