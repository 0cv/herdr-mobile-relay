<script lang="ts">
  import Button from '$components/ui/Button.svelte';
  import type { AttachmentBatchSnapshot } from '$lib/attachments';

  let {
    snapshot,
    oncancel,
    onrestart,
  }: {
    snapshot: AttachmentBatchSnapshot;
    oncancel: () => void | Promise<void>;
    onrestart: () => void | Promise<void>;
  } = $props();

  function stateLabel(state: string): string {
    if (state === 'uploading') return 'Uploading';
    if (state === 'ready') return 'Ready';
    if (state === 'rejected') return 'Rejected';
    if (state === 'interrupted') return 'Interrupted';
    return 'Selected';
  }
</script>

<section class="attachment-progress" aria-label="Attachment upload" aria-busy={snapshot.uploading}>
  <ul>
    {#each snapshot.items as item (item.clientId)}
      <li>
        <div class="attachment-row">
          <strong title={item.name}>{item.name}</strong>
          <span>{stateLabel(item.state)}</span>
        </div>
        <progress max="100" value={Math.round(item.progress * 100)} aria-label={`${item.name} upload progress`}></progress>
        {#if item.issue}<small role="alert">{item.issue.code.replaceAll('_', ' ')}</small>{/if}
      </li>
    {/each}
  </ul>
  {#if snapshot.uploading}
    <Button variant="secondary" size="sm" onclick={() => void oncancel()}>Cancel upload</Button>
  {:else if snapshot.items.some((item) => item.state === 'interrupted')}
    <div class="attachment-actions">
      <Button size="sm" onclick={() => void onrestart()}>Restart interrupted files</Button>
      <Button variant="secondary" size="sm" onclick={() => void oncancel()}>Discard upload</Button>
    </div>
  {/if}
</section>

<style>
  .attachment-progress { display: grid; gap: .55rem; padding: .65rem; border: 1px solid var(--border); border-radius: .65rem; }
  ul { display: grid; gap: .5rem; margin: 0; padding: 0; list-style: none; }
  li { display: grid; gap: .25rem; }
  .attachment-row { display: flex; justify-content: space-between; gap: .75rem; font-size: .85rem; }
  .attachment-row strong { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .attachment-row span, small { color: var(--muted); }
  progress { width: 100%; }
  .attachment-actions { display: flex; flex-wrap: wrap; gap: .5rem; }
</style>
