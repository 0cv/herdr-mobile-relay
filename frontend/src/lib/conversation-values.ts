import type { ConversationBrowseDiagnostics, ConversationEntry, OmoTodoState } from './types';

export function cloneEntries(entries: ConversationEntry[]): ConversationEntry[] {
  return entries.map((entry) => ({
    ...entry,
    ...(entry.tools ? { tools: entry.tools.map((tool) => ({ ...tool })) } : {}),
  }));
}

export function cloneDiagnostics(value: ConversationBrowseDiagnostics | undefined): ConversationBrowseDiagnostics | undefined {
  return value ? { ...value } : undefined;
}

export function estimateEntriesBytes(entries: ConversationEntry[]): number {
  return entries.reduce((total, entry) => total + 96 + entry.id.length * 2 + entry.timestamp.length * 2 + entry.text.length * 2
    + (entry.tools || []).reduce((tools, tool) => tools + 64 + (tool.id?.length || 0) * 2 + tool.name.length * 2
      + (tool.input?.length || 0) * 2 + (tool.output?.length || 0) * 2, 0), 0);
}

export function clonePlan(plan: OmoTodoState): OmoTodoState {
  return {
    ...plan,
    phases: plan.phases.map((phase) => ({ ...phase, tasks: phase.tasks.map((task) => ({ ...task })) })),
  };
}
