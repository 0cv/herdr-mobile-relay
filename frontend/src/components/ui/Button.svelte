<script lang="ts">
  import type { HTMLButtonAttributes } from 'svelte/elements';
  import type { Snippet } from 'svelte';

  type Props = HTMLButtonAttributes & {
    variant?: 'default' | 'secondary' | 'danger' | 'ghost' | 'trust';
    size?: 'default' | 'sm' | 'icon';
    children?: Snippet;
  };

  let { variant = 'default', size = 'default', class: className = '', children, type = 'button', ...rest }: Props = $props();
</script>

<button {type} class={`button button-${variant} button-${size} ${className}`} {...rest}>
  {@render children?.()}
</button>
