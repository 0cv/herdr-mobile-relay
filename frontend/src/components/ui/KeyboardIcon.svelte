<script lang="ts">
  // The glyph of the Keyboard button that opens literal terminal input. Messages show it in place of the button's
  // name; the visually hidden label keeps that name for screen readers and copied text.
  let { label = '', inline = false }: { label?: string; inline?: boolean } = $props();
</script>

<svg class={inline ? 'inline-icon' : 'key-icon'} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false">
  <rect x="2.5" y="6" width="19" height="12" rx="2"></rect>
  <path d="M6.5 10h.01M10 10h.01M14 10h.01M17.5 10h.01M6.5 14h.01M17.5 14h.01M9.5 14h5"></path>
</svg>{#if label}<span class="sr-only">{label}</span>{/if}
