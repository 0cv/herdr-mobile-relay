import type { ConversationEntry, OmoTodoState } from './types';

export function cloneEntries(entries: ConversationEntry[]): ConversationEntry[] {
  return entries.map((entry) => ({
    ...entry,
    ...(entry.tools ? { tools: entry.tools.map((tool) => ({ ...tool })) } : {}),
  }));
}

export function clonePlan(plan: OmoTodoState): OmoTodoState {
  return {
    ...plan,
    phases: plan.phases.map((phase) => ({ ...phase, tasks: phase.tasks.map((task) => ({ ...task })) })),
  };
}
