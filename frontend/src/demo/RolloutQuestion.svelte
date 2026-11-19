<script lang="ts">
  import QuestionForm from '../components/QuestionForm.svelte';
  import type { ComponentProps } from 'svelte';

  let { interaction, ...form }: ComponentProps<typeof QuestionForm> = $props();
  const descriptions: Record<string, string> = {
    'Preview first': 'Start with a small group using the preview link. Keep classic search as the default while we check feedback.',
    'All at once': 'Make the new search page the default for everyone. Keep the classic page ready in case we need to roll back.',
  };
  const displayed = $derived(interaction.id === 'atlas-release-rollout'
    ? { ...interaction, options: interaction.options.map((option) => ({ ...option, description: descriptions[option.label] })) }
    : interaction);
</script>

<QuestionForm {...form} interaction={displayed} />
