<script lang="ts">
  import type { HTMLAttributes } from 'svelte/elements';
  import type { Snippet } from 'svelte';

  type Props = HTMLAttributes<HTMLElement> & { children?: Snippet };
  let { class: className = '', children, ...rest }: Props = $props();
</script>

<section class={`card ${className}`} {...rest}>
  {@render children?.()}
</section>
