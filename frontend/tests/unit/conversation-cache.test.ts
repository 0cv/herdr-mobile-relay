import { afterEach, describe, expect, it } from 'vitest';
import {
  clearConversationPreviews,
  clearConversationPreviewsForRelay,
  configureConversationCache,
  conversationPreviewCacheSize,
  getConversationPreview,
  putConversationPreview,
  resetConversationCacheLimits,
} from '$lib/conversation-cache';
import type { ConversationEntry, OmoTodoState } from '$lib/types';

function entry(id: string, role: 'user' | 'assistant', text = id, tools?: ConversationEntry['tools']): ConversationEntry {
  return {
    id,
    role,
    text,
    timestamp: '2026-01-01T00:00:00Z',
    ...(tools ? { tools } : {}),
  };
}

function preview(identity: string, relayId = 'relay-1', entries: ConversationEntry[] = [entry('u1', 'user', 'question'), entry('a1', 'assistant', 'answer')]) {
  return {
    identity,
    relayId,
    savedAt: 100,
    sourceRevision: 'revision-1',
    historical: false,
    entries,
  };
}

const plan: OmoTodoState = {
  available: true,
  phases: [{ name: 'phase', tasks: [{ content: 'task', status: 'pending' }] }],
  truncated: false,
};

afterEach(() => {
  resetConversationCacheLimits();
  clearConversationPreviews();
});

describe('conversation preview cache', () => {
  it('returns cloned entries, tools, diagnostics, and plans', () => {
    const tools = [{ id: 'tool-1', name: 'Bash', input: 'ls', output: 'ok' }];
    const diagnostics = { oversized_records: 1, corrupt_records: 2, source_truncated: false };
    putConversationPreview({
      ...preview('identity-1', 'relay-1', [entry('u1', 'user', 'question'), entry('a1', 'assistant', 'answer', tools)]),
      diagnostics,
      plan,
    }, 100);

    const first = getConversationPreview('identity-1', 101)!;
    first.entries[1].text = 'mutated';
    first.entries[1].tools![0].output = 'mutated';
    first.diagnostics!.corrupt_records = 99;
    first.plan!.phases[0].tasks[0].content = 'mutated';

    const second = getConversationPreview('identity-1', 102)!;
    expect(second.entries[1].text).toBe('answer');
    expect(second.entries[1].tools![0].output).toBe('ok');
    expect(second.diagnostics?.corrupt_records).toBe(2);
    expect(second.plan?.phases[0].tasks[0].content).toBe('task');
  });

  it('expires from the successful save time and clears by relay', () => {
    putConversationPreview(preview('identity-1', 'relay-1'), 100);
    putConversationPreview(preview('identity-2', 'relay-2'), 100);
    expect(getConversationPreview('identity-1', 109)).not.toBeNull();

    configureConversationCache({ ttlMs: 10 });
    // configureConversationCache deliberately resets values; put fresh values
    // to exercise the TTL boundary without relying on wall-clock time.
    putConversationPreview(preview('identity-1', 'relay-1'), 100);
    putConversationPreview(preview('identity-2', 'relay-2'), 100);
    expect(getConversationPreview('identity-1', 109)).not.toBeNull();
    expect(getConversationPreview('identity-1', 110)).toBeNull();

    putConversationPreview(preview('identity-1', 'relay-1'), 100);
    putConversationPreview(preview('identity-2', 'relay-2'), 100);
    clearConversationPreviewsForRelay('relay-1');
    expect(getConversationPreview('identity-1', 101)).toBeNull();
    expect(getConversationPreview('identity-2', 101)).not.toBeNull();
  });

  it('evicts least recently used identities and enforces aggregate bytes', () => {
    configureConversationCache({ maxIdentities: 2, totalBytes: 1_500 });
    expect(putConversationPreview(preview('identity-1'), 1)).toBe(true);
    expect(putConversationPreview(preview('identity-2'), 2)).toBe(true);
    expect(getConversationPreview('identity-1', 3)).not.toBeNull();
    expect(putConversationPreview(preview('identity-3'), 4)).toBe(true);
    expect(getConversationPreview('identity-1', 5)).not.toBeNull();
    expect(getConversationPreview('identity-2', 5)).toBeNull();
    expect(getConversationPreview('identity-3', 5)).not.toBeNull();

    configureConversationCache({ maxIdentities: 6, totalBytes: 1 });
    expect(putConversationPreview(preview('too-large'), 10)).toBe(false);
    expect(conversationPreviewCacheSize().identities).toBe(0);
  });

  it('keeps whole newest exchanges and skips an oversized newest exchange', () => {
    configureConversationCache({ maxEntries: 4, itemBytes: 1_500 });
    const accepted = putConversationPreview(preview('identity-1', 'relay-1', [
      entry('u0', 'user', 'old question'),
      entry('a0', 'assistant', 'old answer'),
      entry('u1', 'user', 'new question'),
      entry('a1', 'assistant', 'new answer'),
    ]), 100);
    expect(accepted).toBe(true);
    expect(getConversationPreview('identity-1', 101)?.entries.map(({ id }) => id)).toEqual(['u0', 'a0', 'u1', 'a1']);

    configureConversationCache({ maxEntries: 2, itemBytes: 1_500 });
    expect(putConversationPreview(preview('identity-2', 'relay-1', [
      entry('u0', 'user', 'old question'),
      entry('a0', 'assistant', 'old answer'),
      entry('u1', 'user', 'new question'),
      entry('a1', 'assistant', 'new answer'),
    ]), 100)).toBe(true);
    expect(getConversationPreview('identity-2', 101)?.entries.map(({ id }) => id)).toEqual(['u1', 'a1']);

    expect(putConversationPreview(preview('identity-3', 'relay-1', [
      entry('u1', 'user', 'x'.repeat(1_000)),
      entry('a1', 'assistant', 'answer'),
    ]), 100)).toBe(false);
    expect(getConversationPreview('identity-3', 101)).toBeNull();
  });
});

describe('conversation clone values through cache', () => {
  type Preview = import('$lib/conversation-cache').ConversationPreview;
  type Tool = NonNullable<ConversationEntry['tools']>[number];
  type Phase = OmoTodoState['phases'][number];
  type Task = Phase['tasks'][number];

  afterEach(() => {
    resetConversationCacheLimits();
    clearConversationPreviews();
  });

  function fixture() {
    const sentinels = {
      entry: { label: 'entry extension' }, tool: { label: 'tool extension' },
      plan: { label: 'plan extension' }, phase: { label: 'phase extension' }, task: { label: 'task extension' },
    };
    const tool = { id: 'tool', name: 'Read', input: 'file', output: 'tool result', extension: sentinels.tool };
    const entries = [entry('u', 'user', 'question'), { ...entry('a', 'assistant', 'answer', [tool]), extension: sentinels.entry }];
    const task = { content: 'task', status: 'pending' as Task['status'], extension: sentinels.task };
    const phase = { name: 'phase', tasks: [task], extension: sentinels.phase };
    const plan = { available: true, phases: [phase], truncated: false, extension: sentinels.plan };
    return { ...preview('clone-values', 'relay-1', entries), plan, sentinels };
  }

  function expectValues(value: Preview, sentinels: ReturnType<typeof fixture>['sentinels']) {
    expect(value.entries.map(({ id, role, text }) => [id, role, text])).toEqual([['u', 'user', 'question'], ['a', 'assistant', 'answer']]);
    expect(value.entries[1].tools).toHaveLength(1);
    expect(value.entries[1].tools![0]).toMatchObject({ id: 'tool', name: 'Read', input: 'file', output: 'tool result' });
    expect(value.plan).toMatchObject({ available: true, truncated: false, phases: [{ name: 'phase', tasks: [{ content: 'task', status: 'pending' }] }] });
    const extension = (target: object) => (target as { extension?: object }).extension;
    expect(extension(value.entries[1])).toBe(sentinels.entry);
    expect(extension(value.entries[1].tools![0])).toBe(sentinels.tool);
    expect(extension(value.plan!)).toBe(sentinels.plan);
    expect(extension(value.plan!.phases[0])).toBe(sentinels.phase);
    expect(extension(value.plan!.phases[0].tasks[0])).toBe(sentinels.task);
  }

  function targets(value: Preview): object[] {
    return [value.entries, ...value.entries, value.entries[1].tools!, value.entries[1].tools![0],
      value.plan!, value.plan!.phases, value.plan!.phases[0], value.plan!.phases[0].tasks, value.plan!.phases[0].tasks[0]];
  }

  function mutate(value: Preview) {
    value.entries[0].text = 'changed question';
    value.entries[1].text = 'changed answer';
    value.entries[1].tools![0].output = 'changed output';
    value.plan!.available = false;
    value.plan!.phases[0].name = 'changed phase';
    value.plan!.phases[0].tasks[0].content = 'changed task';
    value.plan!.phases[0].tasks[0].status = 'completed';
  }

  it('isolates preview ingress and repeated read clones', () => {
    const source = fixture();
    const before = JSON.stringify(source);
    expect(putConversationPreview(source, 100)).toBe(true);
    expect(JSON.stringify(source)).toBe(before);
    expectValues(source, source.sentinels);
    const first = getConversationPreview('clone-values', 101)!;
    expect(first.savedAt).toBe(100);
    expectValues(first, source.sentinels);
    mutate(source);
    mutate(first);
    const second = getConversationPreview('clone-values', 102)!;
    expectValues(second, source.sentinels);
    const mapped = [source, first, second].map(targets);
    for (let index = 0; index < mapped[0].length; index++) {
      expect(new Set(mapped.map((values) => values[index])).size).toBe(3);
    }
  });

  it('preserves tool presence while projecting absent plans', () => {
    const empty: Tool[] = [];
    const entries: ConversationEntry[] = [
      entry('absent', 'user'),
      { ...entry('undefined', 'user'), tools: undefined },
      { ...entry('null', 'user'), tools: null as unknown as ConversationEntry['tools'] },
      { ...entry('empty', 'user'), tools: empty },
    ];
    const variants: Preview[] = [
      preview('absent-plan', 'relay-1', entries),
      { ...preview('undefined-plan', 'relay-1', entries), plan: undefined },
      { ...preview('null-plan', 'relay-1', entries), plan: null as unknown as OmoTodoState },
    ];
    expect(variants.map((value) => Object.hasOwn(value, 'plan'))).toEqual([false, true, true]);
    expect(variants[1].plan).toBeUndefined();
    expect(variants[2].plan).toBeNull();
    for (let index = 0; index < variants.length; index++) {
      const source = variants[index];
      expect(putConversationPreview(source, 100 + index * 3)).toBe(true);
      const result = getConversationPreview(source.identity, 101 + index * 3)!;
      expect(Object.hasOwn(result, 'plan')).toBe(false);
      expect(result.plan).toBeUndefined();
      expect(result.entries.map(({ id }) => id)).toEqual(['absent', 'undefined', 'null', 'empty']);
      for (const values of [entries, result.entries]) {
        expect(values.map((value) => Object.hasOwn(value, 'tools'))).toEqual([false, true, true, true]);
        expect(values[0].tools).toBeUndefined();
        expect(values[1].tools).toBeUndefined();
        expect(values[2].tools).toBeNull();
        expect(values[3].tools).toEqual([]);
      }
      expect(result.entries).not.toBe(entries);
      for (let offset = 0; offset < entries.length; offset++) expect(result.entries[offset]).not.toBe(entries[offset]);
      expect(result.entries[3].tools).not.toBe(empty);
    }
  });

  it('preserves sparse nested clone arrays', () => {
    const tool: Tool = { id: 'sparse-tool', name: 'Read', output: 'sparse output' };
    const tools = new Array<Tool>(2);
    tools[1] = tool;
    const task: Task = { content: 'sparse task', status: 'pending' };
    const tasks = new Array<Task>(2);
    tasks[1] = task;
    const phase: Phase = { name: 'sparse phase', tasks };
    const phases = new Array<Phase>(2);
    phases[1] = phase;
    const source: Preview = {
      ...preview('sparse', 'relay-1', [entry('dense-user', 'user', 'question', tools)]),
      plan: { available: true, phases, truncated: false },
    };
    expect(putConversationPreview(source, 100)).toBe(true);
    const first = getConversationPreview('sparse', 101)!;
    const second = getConversationPreview('sparse', 102)!;
    const mapped: object[][] = [];
    for (const value of [source, first, second]) {
      expect(value.entries).toHaveLength(1);
      expect(0 in value.entries).toBe(true);
      expect(value.entries[0].id).toBe('dense-user');
      expect(value.entries[0].text).toBe('question');
      const nestedTools = value.entries[0].tools!;
      const nestedPhases = value.plan!.phases;
      const nestedTasks = nestedPhases[1].tasks;
      for (const values of [nestedTools, nestedPhases, nestedTasks]) {
        expect(values).toHaveLength(2);
        expect(0 in values).toBe(false);
        expect(1 in values).toBe(true);
      }
      expect(nestedTools[1].id).toBe('sparse-tool');
      expect(nestedTools[1].name).toBe('Read');
      expect(nestedTools[1].output).toBe('sparse output');
      expect(value.plan!.available).toBe(true);
      expect(value.plan!.truncated).toBe(false);
      expect(nestedPhases[1].name).toBe('sparse phase');
      expect(nestedTasks[1].content).toBe('sparse task');
      expect(nestedTasks[1].status).toBe('pending');
      mapped.push([value.entries, value.entries[0], nestedTools, nestedTools[1], value.plan!, nestedPhases, nestedPhases[1], nestedTasks, nestedTasks[1]]);
    }
    for (let index = 0; index < mapped[0].length; index++) {
      expect(new Set(mapped.map((values) => values[index])).size).toBe(3);
    }
  });

  it('preserves preview entry and plan getter order', () => {
    const trace: string[] = [];
    const tool = { name: 'Read', get output() { trace.push('tool.output'); return 'tool result'; } };
    const tools = [tool];
    const sourceEntry = {
      ...entry('getter', 'user', 'question'),
      get tools() { trace.push('entry.tools'); return tools; },
      get after() { trace.push('entry.after'); return 'entry after'; },
    };
    const task: Task = { status: 'pending', get content() { trace.push('task.content'); return 'task'; } };
    const tasks = [task];
    const phase = {
      name: 'phase',
      get tasks() { trace.push('phase.tasks'); return tasks; },
      get after() { trace.push('phase.after'); return 'phase after'; },
    };
    const phases = [phase];
    const sourcePlan = {
      available: true, truncated: false,
      get phases() { trace.push('plan.phases'); return phases; },
      get after() { trace.push('plan.after'); return 'plan after'; },
    };
    const entries = [sourceEntry];
    const source: Preview = { ...preview('getters', 'relay-1', entries), plan: sourcePlan };
    expect(putConversationPreview(source, 100)).toBe(true);
    const expected = ['entry.tools', 'entry.after', 'entry.tools', 'entry.tools', 'tool.output',
      'plan.phases', 'plan.after', 'plan.phases', 'phase.tasks', 'phase.after', 'phase.tasks', 'task.content'];
    expect(trace).toEqual(expected);
    const result = getConversationPreview('getters', 101)!;
    expect(trace).toEqual(expected);
    expect(result.entries).not.toBe(entries);
    const copied = result.entries[0];
    expect(copied).not.toBe(sourceEntry);
    expect(copied.id).toBe('getter');
    expect(copied.text).toBe('question');
    expect((copied as ConversationEntry & { after: string }).after).toBe('entry after');
    expect(copied.tools).not.toBe(tools);
    expect(copied.tools).toHaveLength(1);
    expect(copied.tools![0]).not.toBe(tool);
    expect(copied.tools![0].name).toBe('Read');
    expect(copied.tools![0].output).toBe('tool result');
    expect(result.plan).not.toBe(sourcePlan);
    expect(result.plan!.available).toBe(true);
    expect(result.plan!.truncated).toBe(false);
    expect((result.plan as OmoTodoState & { after: string }).after).toBe('plan after');
    expect(result.plan!.phases).not.toBe(phases);
    expect(result.plan!.phases).toHaveLength(1);
    expect(result.plan!.phases[0]).not.toBe(phase);
    expect(result.plan!.phases[0].name).toBe('phase');
    expect((result.plan!.phases[0] as Phase & { after: string }).after).toBe('phase after');
    expect(result.plan!.phases[0].tasks).not.toBe(tasks);
    expect(result.plan!.phases[0].tasks).toHaveLength(1);
    expect(result.plan!.phases[0].tasks[0]).not.toBe(task);
    expect(result.plan!.phases[0].tasks[0].content).toBe('task');
    expect(result.plan!.phases[0].tasks[0].status).toBe('pending');
  });
});
