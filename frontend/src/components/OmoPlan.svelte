<script lang="ts">
  import type { OmoTodoState } from '$lib/types';

  type OmoTaskStatus = OmoTodoState['phases'][number]['tasks'][number]['status'];
  const STATUS_LABELS: Record<OmoTaskStatus, string> = {
    pending: 'Pending',
    in_progress: 'In progress',
    completed: 'Completed',
    abandoned: 'Abandoned',
  };

  let {
    plan,
    title = 'OMO plan',
  }: {
    plan: OmoTodoState;
    title?: string;
  } = $props();

  const componentId = $props.id();
  const headingId = `${componentId}-heading`;
  const updatedAt = $derived.by(() => {
    if (!plan.updated_at) return null;
    const timestamp = new Date(plan.updated_at);
    return Number.isNaN(timestamp.valueOf()) ? null : timestamp;
  });
  const hasTasks = $derived(plan.phases.some((phase) => phase.tasks.length > 0));
</script>

<section class="omo-plan" aria-labelledby={headingId}>
  <header>
    <div>
      <h3 id={headingId}>{title}</h3>
      <span class="omo-read-only">Read only</span>
    </div>
    {#if updatedAt}
      <time datetime={plan.updated_at}>{updatedAt.toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' })}</time>
    {/if}
  </header>

  {#if !plan.available}
    <p class="omo-plan-state" role="status">
      Plan unavailable{#if plan.reason_code} <code>{plan.reason_code}</code>{/if}
    </p>
  {:else if !hasTasks}
    <p class="omo-plan-state">No todo stages were reported.</p>
  {:else}
    <ol class="omo-phases" aria-label="Plan stages">
      {#each plan.phases as phase, phaseIndex (`${phase.name}:${phaseIndex}`)}
        <li class="omo-phase">
          <h4>{phase.name}</h4>
          <ol class="omo-stages">
            {#each phase.tasks as task, taskIndex (task.id ?? `${phaseIndex}:${taskIndex}`)}
              <li class={`omo-stage omo-stage-${task.status}`}>
                <span class="omo-stage-mark" aria-hidden="true"></span>
                <span class="omo-stage-content">
                  <span>{task.content}</span>
                  <small>{STATUS_LABELS[task.status]}</small>
                </span>
              </li>
            {/each}
          </ol>
        </li>
      {/each}
    </ol>
  {/if}

  {#if plan.truncated}
    <p class="omo-plan-limit" role="status">Additional plan stages were omitted by the source.</p>
  {/if}
</section>

<style>
  .omo-plan {
    border: 1px solid var(--border);
    border-radius: .75rem;
    background: var(--card);
    padding: .875rem;
  }

  header,
  header > div,
  .omo-stage,
  .omo-stage-content small {
    display: flex;
  }

  header {
    align-items: flex-start;
    justify-content: space-between;
    gap: .75rem;
  }

  header > div {
    align-items: center;
    gap: .5rem;
    min-width: 0;
  }

  h3,
  h4,
  p {
    margin: 0;
  }

  h3 {
    font-size: .95rem;
  }

  h4 {
    font-size: .82rem;
  }

  time,
  small,
  .omo-plan-state,
  .omo-plan-limit {
    color: var(--muted);
    font-size: .78rem;
  }

  .omo-read-only {
    border: 1px solid var(--border);
    border-radius: 999px;
    color: var(--muted);
    font-size: .65rem;
    font-weight: 700;
    letter-spacing: .04em;
    padding: .12rem .42rem;
    text-transform: uppercase;
    white-space: nowrap;
  }

  .omo-plan-state,
  .omo-plan-limit {
    margin-top: .75rem;
  }

  .omo-plan-state code {
    margin-left: .35rem;
  }

  .omo-phases,
  .omo-stages {
    display: grid;
    list-style: none;
    padding: 0;
  }

  .omo-phases {
    gap: .75rem;
    margin: .75rem 0 0;
  }

  .omo-stages {
    gap: .25rem;
    margin: .25rem 0 0;
  }

  .omo-stage {
    align-items: flex-start;
    gap: .65rem;
    min-height: 2.75rem;
    padding: .45rem .25rem;
  }

  .omo-stage-mark {
    border: 2px solid var(--muted);
    border-radius: 50%;
    flex: 0 0 .8rem;
    height: .8rem;
    margin-top: .2rem;
  }

  .omo-stage-in_progress .omo-stage-mark {
    border-color: var(--primary);
    box-shadow: inset 0 0 0 2px var(--card);
    background: var(--primary);
  }

  .omo-stage-completed .omo-stage-mark {
    border-color: var(--success);
    background: var(--success);
  }

  .omo-stage-abandoned {
    color: var(--muted);
  }

  .omo-stage-abandoned .omo-stage-mark {
    border-color: var(--danger);
  }

  .omo-stage-content {
    display: grid;
    gap: .2rem;
    min-width: 0;
  }

  .omo-stage-content > span {
    overflow-wrap: anywhere;
  }

  .omo-stage-content small {
    gap: .5rem;
  }
</style>
