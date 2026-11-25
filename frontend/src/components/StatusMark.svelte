<script lang="ts">
  import type { StatusAppearance } from '$lib/status-marks';

  let { shape, tone, label, class: extra = '' }: StatusAppearance & { label?: string; class?: string } = $props();
</script>

<span
  class={`status-dot status-${shape} status-${tone} ${extra}`}
  role={label ? 'img' : undefined}
  aria-label={label}
  aria-hidden={label ? undefined : 'true'}
></span>
