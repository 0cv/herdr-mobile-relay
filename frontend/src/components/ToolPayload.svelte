<script lang="ts">
  import Button from '$components/ui/Button.svelte';
  import { clampPayload, formatToolPayload } from '$lib/conversation';

  let { label, raw }: { label: string; raw: string } = $props();

  let expanded = $state(false);

  const payloadId = $props.id();
  const formatted = $derived(formatToolPayload(raw));
  const clamp = $derived(clampPayload(formatted));
  const lines = $derived(formatted.split('\n').length);
</script>

<section>
  <h4>{label}</h4>
  <pre id={payloadId}>{expanded ? formatted : clamp.preview}</pre>
  {#if clamp.clamped}
    <Button
      variant="ghost"
      size="sm"
      aria-expanded={expanded}
      aria-controls={payloadId}
      onclick={() => (expanded = !expanded)}
    >
      {expanded ? 'Show less' : `Show all ${lines} lines`}
    </Button>
  {/if}
</section>
