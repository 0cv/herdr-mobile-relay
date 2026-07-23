<script lang="ts">
  import { tick } from 'svelte';
  import Button from '$components/ui/Button.svelte';
  import { CommandError, relayStore } from '$lib/store';
  import {
    createQuestionDraft,
    questionDraftKey,
    questionSubmitAllowed,
    shouldRestoreQuestionDraft,
    updateQuestionOption,
    updateQuestionOther,
  } from '$lib/questions';
  import type { Agent, QuestionDraft, QuestionInteraction } from '$lib/types';

  const drafts = new Map<string, QuestionDraft>();
  const dirtyDrafts = new Set<string>();
  const OTHER_CHOICE = 'other';

  let {
    agent,
    interaction,
    responding,
  }: {
    agent: Agent;
    interaction: QuestionInteraction;
    responding: boolean;
  } = $props();

  let draft = $state<QuestionDraft>({ selected: new Set(), otherSelected: false, otherText: '' });
  let singleChoice = $state<number | typeof OTHER_CHOICE | null>(null);
  let activeKey = '';
  let formElement: HTMLFormElement;
  let progress = $derived.by(() => {
    const current = interaction.question_index;
    const total = interaction.question_total;
    if (typeof current !== 'number' || typeof total !== 'number') return '';
    if (!Number.isInteger(current) || !Number.isInteger(total) || current < 1 || current > total) return '';
    return `Question ${current} of ${total}`;
  });

  function applyDraft(next: QuestionDraft) {
    draft = next;
    if (interaction.kind !== 'single_select') {
      singleChoice = null;
      return;
    }
    singleChoice = next.otherSelected
      ? OTHER_CHOICE
      : next.selected.values().next().value ?? null;
  }

  $effect(() => {
    const key = questionDraftKey(agent, interaction);
    const incoming = createQuestionDraft(interaction);
    if (key === activeKey) {
      if (!dirtyDrafts.has(key)) {
        applyDraft(incoming);
        drafts.set(key, incoming);
      }
      return;
    }
    activeKey = key;
    const cached = drafts.get(key);
    const restore = dirtyDrafts.has(key)
      && shouldRestoreQuestionDraft(interaction, cached, incoming);
    if (!restore) dirtyDrafts.delete(key);
    applyDraft(restore ? cached! : incoming);
    drafts.set(key, draft);
    void tick().then(() => { if (formElement) formElement.scrollTop = 0; });
  });

  function save(next: QuestionDraft) {
    const key = questionDraftKey(agent, interaction);
    applyDraft(next);
    dirtyDrafts.add(key);
    drafts.set(key, next);
  }

  function choose(index: number, checked: boolean) {
    save(updateQuestionOption(interaction, draft, index, checked));
  }

  function chooseOther(selected: boolean) {
    save(updateQuestionOther(interaction, draft, selected));
  }

  function changeOther(text: string) {
    const selected = interaction.kind === 'single_select' || Boolean(text);
    save(updateQuestionOther(interaction, draft, selected, text));
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (!questionSubmitAllowed(interaction, draft)) {
      relayStore.showToast('Complete the current question before submitting.', true);
      return;
    }
    try {
      const submittedKey = questionDraftKey(agent, interaction);
      const result = await relayStore.answerQuestion(agent, interaction, draft);
      relayStore.clearResponding(agent.pane_id);
      dirtyDrafts.delete(submittedKey);
      drafts.delete(submittedKey);
      const next = result.data?.interaction as QuestionInteraction | undefined;
      if (result.phase === 'advanced' && next) {
        relayStore.applyQuestionInteraction(agent, next);
        relayStore.showToast('Answer saved. Continue with the next question.');
      } else {
        relayStore.applyQuestionInteraction(agent, null);
        relayStore.showToast('Answers submitted.');
      }
    } catch (caught) {
      relayStore.clearResponding(agent.pane_id);
      const error = caught as CommandError;
      const fresh = error.data?.interaction as QuestionInteraction | undefined;
      if (fresh) {
        const freshKey = questionDraftKey(agent, fresh);
        dirtyDrafts.delete(freshKey);
        drafts.delete(freshKey);
        relayStore.applyQuestionInteraction(agent, fresh);
      }
      relayStore.showToast(error.message, true);
    }
  }

  async function previous() {
    try {
      const result = await relayStore.navigateQuestionPrevious(agent, interaction);
      relayStore.clearResponding(agent.pane_id);
      const prior = result.data?.interaction as QuestionInteraction | undefined;
      if (result.phase === 'navigated' && prior) relayStore.applyQuestionInteraction(agent, prior);
      relayStore.showToast('Opened the previous question.');
    } catch (caught) {
      relayStore.clearResponding(agent.pane_id);
      const error = caught as CommandError;
      const fresh = error.data?.interaction as QuestionInteraction | undefined;
      if (fresh) relayStore.applyQuestionInteraction(agent, fresh);
      relayStore.showToast(error.message, true);
    }
  }

  async function chat() {
    try {
      await relayStore.clarifyQuestion(agent, interaction);
      relayStore.clearResponding(agent.pane_id);
      relayStore.applyQuestionInteraction(agent, null);
      relayStore.showToast('Question chat opened.');
    } catch (caught) {
      relayStore.clearResponding(agent.pane_id);
      const error = caught as CommandError;
      const fresh = error.data?.interaction as QuestionInteraction | undefined;
      if (fresh) relayStore.applyQuestionInteraction(agent, fresh);
      relayStore.showToast(error.message, true);
    }
  }
</script>

<form bind:this={formElement} class="question-form" aria-label={interaction.question} aria-busy={responding} onsubmit={submit}>
  {#if progress}<p class="question-progress">{progress}</p>{/if}
  <fieldset disabled={responding}>
    <legend>{interaction.question}</legend>
    {#each interaction.options as option, position (`${interaction.id}:${option.index ?? position}`)}
      {@const index = Number.isInteger(option.index) ? option.index : position}
      <label class="question-choice">
        {#if interaction.kind === 'multi_select'}
          <input
            type="checkbox"
            value={index}
            checked={draft.selected.has(index)}
            onchange={(event) => choose(index, event.currentTarget.checked)}
          />
        {:else}
          <input
            type="radio"
            name="question-answer"
            value={index}
            bind:group={singleChoice}
            onchange={(event) => choose(index, event.currentTarget.checked)}
          />
        {/if}
        <span>
          <strong>{option.label || `Option ${position + 1}`}</strong>
          {#if option.description}<small>{option.description}</small>{/if}
        </span>
      </label>
    {/each}
    <div class="question-other">
      {#if interaction.kind === 'multi_select'}
        <input
          id="question-other-toggle"
          type="checkbox"
          checked={draft.otherSelected}
          aria-label={interaction.other?.label || 'Other'}
          onchange={(event) => chooseOther(event.currentTarget.checked)}
        />
      {:else}
        <input
          id="question-other-toggle"
          type="radio"
          name="question-answer"
          value={OTHER_CHOICE}
          bind:group={singleChoice}
          aria-label={interaction.other?.label || 'Other'}
          onchange={(event) => chooseOther(event.currentTarget.checked)}
        />
      {/if}
      <label for="question-other-toggle">{interaction.other?.label || 'Other'}</label>
      <input
        class="question-other-input"
        value={draft.otherText}
        maxlength="100000"
        aria-label={interaction.other?.placeholder || 'Other answer'}
        placeholder={interaction.other?.placeholder || 'Other answer'}
        onfocus={() => { if (!draft.otherSelected) chooseOther(true); }}
        oninput={(event) => changeOther(event.currentTarget.value)}
      />
    </div>
  </fieldset>
  <div class="question-actions">
    {#if interaction.can_go_back}<Button type="button" variant="secondary" disabled={responding} onclick={previous}>← Previous</Button>{/if}
    <Button type="submit" disabled={responding || !questionSubmitAllowed(interaction, draft)}>{interaction.submit_label || 'Submit'}</Button>
    {#if interaction.can_chat && !interaction.other}<Button type="button" variant="trust" disabled={responding} onclick={chat}>Chat about this</Button>{/if}
  </div>
  <p class="question-status" aria-live="polite">
    {responding ? 'Waiting for agent…' : interaction.kind === 'multi_select' ? 'Selections are sent together when you submit.' : 'Choose one answer.'}
  </p>
</form>
