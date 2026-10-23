import type { Agent, QuestionDraft, QuestionInteraction } from './types';

// A question the relay returned with a result, or undefined when it is missing or malformed.
export function returnedInteraction(result: { data?: Record<string, unknown> }): QuestionInteraction | undefined {
  const value = result.data?.interaction;
  if (!value || typeof value !== 'object') return undefined;
  const candidate = value as QuestionInteraction;
  if (!candidate.id || !candidate.question || !Array.isArray(candidate.options)) return undefined;
  return candidate;
}

// The toast for an answer the relay rejected. After a lone question, a rejection that carries
// a question means the agent moved on to it; one we cannot read shows up with the next pane
// update. In a questionnaire an unexpected question stays an error.
export function answerFailureToast(
  error: { message: string; data?: Record<string, unknown> },
  fresh: QuestionInteraction | undefined,
  answered: QuestionInteraction,
): [message: string, isError: boolean] {
  if ((answered.question_total ?? 1) > 1) return [error.message, true];
  if (fresh) return ['The agent asked another question.', false];
  if (error.data?.interaction) return ['The agent asked another question; it will appear with the next update.', false];
  return [error.message, true];
}

export function questionDraftKey(agent: Agent, interaction: QuestionInteraction): string {
  return `${agent.pane_id}::${interaction.id}`;
}

export function createQuestionDraft(interaction: QuestionInteraction): QuestionDraft {
  return {
    selected: new Set(interaction.options.filter((option) => option.selected).map((option) => option.index)),
    otherSelected: Boolean(interaction.other?.selected),
    otherText: String(interaction.other?.text || ''),
  };
}

export function questionSubmitAllowed(interaction: QuestionInteraction | null, draft: QuestionDraft | null): boolean {
  if (!interaction || !draft) return false;
  if (interaction.kind === 'multi_select') return true;
  const otherAllowed = draft.otherSelected
    && (Boolean(draft.otherText.trim()) || Boolean(interaction.other?.allow_empty));
  return draft.selected.size === 1 || otherAllowed;
}

export function shouldRestoreQuestionDraft(
  interaction: QuestionInteraction,
  cached: QuestionDraft | undefined,
  incoming: QuestionDraft,
): boolean {
  if (!cached) return false;
  return questionSubmitAllowed(interaction, cached)
    || !questionSubmitAllowed(interaction, incoming);
}

export function updateQuestionOption(
  interaction: QuestionInteraction,
  draft: QuestionDraft,
  index: number,
  checked: boolean,
): QuestionDraft {
  const selected = new Set(draft.selected);
  let otherSelected = draft.otherSelected;
  let otherText = draft.otherText;
  if (interaction.kind === 'single_select') {
    selected.clear();
    if (checked) selected.add(index);
    otherSelected = false;
    otherText = '';
  } else if (checked) selected.add(index);
  else selected.delete(index);
  return { selected, otherSelected, otherText };
}

export function updateQuestionOther(
  interaction: QuestionInteraction,
  draft: QuestionDraft,
  selected: boolean,
  text = draft.otherText,
): QuestionDraft {
  const choices = new Set(draft.selected);
  if (interaction.kind === 'single_select' && selected) choices.clear();
  return {
    selected: choices,
    otherSelected: selected,
    otherText: interaction.kind === 'multi_select' && !selected ? '' : text,
  };
}
