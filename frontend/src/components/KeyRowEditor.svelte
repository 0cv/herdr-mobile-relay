<script lang="ts">
  import { onMount, tick } from 'svelte';
  import { get } from 'svelte/store';
  import Button from '$components/ui/Button.svelte';
  import { setTerminalKeyRow, terminalKeyRow } from '$lib/preferences';
  import { ctrlLetters, defaultKeyRow, keyRowEntries, keyRowLabel } from '$lib/terminal-key-row';

  let { close }: { close: () => void } = $props();
  let draft = $state([...get(terminalKeyRow)]);
  let choice = $state('');
  let letter = $state('');
  let position = $state('');
  let failed = $state(false);
  let list: HTMLOListElement;
  let picker: HTMLSelectElement;
  const available = $derived(keyRowEntries.filter((id) => !draft.includes(id)));
  const letters = $derived(ctrlLetters.filter((letter) => available.includes(ctrlKey(letter))));

  onMount(() => picker.focus());

  function ctrlKey(letter: string) {
    return `ctrl+${letter.toLowerCase()}`;
  }

  async function add() {
    const id = choice === 'ctrl-letter' ? ctrlKey(letter) : choice;
    if (!available.includes(id)) return;
    draft.push(id);
    position = `${keyRowLabel(id)} added at position ${draft.length} of ${draft.length}.`;
    choice = '';
    letter = '';
    await tick();
    picker.focus();
  }

  async function move(index: number, direction: -1 | 1) {
    const next = index + direction;
    const id = draft[index];
    [draft[index], draft[next]] = [draft[next], id];
    position = `${keyRowLabel(id)} moved to position ${next + 1} of ${draft.length}.`;
    await tick();
    const buttons = list.children[next].querySelectorAll('button');
    const target = direction === -1 ? buttons[0] : buttons[1];
    (target.disabled ? buttons[direction === -1 ? 1 : 0] : target).focus();
  }

  async function remove(index: number) {
    const [id] = draft.splice(index, 1);
    position = `${keyRowLabel(id)} removed from position ${index + 1}.`;
    await tick();
    (list.children[Math.min(index, draft.length - 1)]?.querySelector<HTMLButtonElement>('button:last-child') ?? picker).focus();
  }

  function reset() {
    draft = [...defaultKeyRow];
    choice = '';
    letter = '';
    position = 'Default order restored in the draft.';
  }

  function save() {
    try {
      setTerminalKeyRow(draft);
    } catch {
      failed = true;
      return;
    }
    close();
  }
</script>

<div class="form-stack">
  <p id="key-row-scope" class="hint">Applies to terminals on this device.</p>
  <label for="key-row-entry">Add a key</label>
  <select id="key-row-entry" aria-describedby="key-row-scope" bind:this={picker} bind:value={choice} onchange={() => { letter = ''; }}>
    <option value="">Choose an entry</option>
    {#each available as id (id)}
      {#if !id.startsWith('ctrl+')}<option value={id}>{keyRowLabel(id)}</option>{/if}
    {/each}
    {#if letters.length}<option value="ctrl-letter">Ctrl+letter</option>{/if}
  </select>
  {#if choice === 'ctrl-letter'}
    <label for="key-row-letter">Letter</label>
    <select id="key-row-letter" bind:value={letter}>
      <option value="">Choose a letter</option>
      {#each letters as letter (letter)}<option value={letter}>{letter}</option>{/each}
    </select>
  {/if}
  <Button size="sm" variant="secondary" disabled={!choice || (choice === 'ctrl-letter' && !letter)} onclick={add}>Add</Button>
  <p class="sr-only" role="status">{position}</p>
  <ol class="key-row-draft" bind:this={list} aria-label="Key row order">
    {#each draft as id, index (id)}
      {@const label = keyRowLabel(id)}
      <li>
        {label}
        <div class="dialog-actions">
          <Button size="sm" variant="secondary" aria-label={`Move earlier: ${label}`} disabled={index === 0} onclick={() => move(index, -1)}>Move earlier</Button>
          <Button size="sm" variant="secondary" aria-label={`Move later: ${label}`} disabled={index === draft.length - 1} onclick={() => move(index, 1)}>Move later</Button>
          <Button size="sm" variant="ghost" aria-label={`Remove ${label}`} onclick={() => remove(index)}>Remove</Button>
        </div>
      </li>
    {/each}
  </ol>
  {#if failed}<p class="warning" role="alert">Could not save the key row on this device.</p>{/if}
  <div class="dialog-actions">
    <Button variant="secondary" onclick={reset}>Reset to default</Button>
    <Button onclick={save}>Save</Button>
    <Button variant="ghost" onclick={close}>Cancel</Button>
  </div>
</div>

<style>
  .key-row-draft { margin: 0; padding-inline-start: 1.5rem; }
  li { margin-bottom: .65rem; }
</style>
