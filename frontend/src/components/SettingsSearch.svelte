<script lang="ts">
  let { root }: { root?: HTMLElement } = $props();
  let query = $state('');
  let noMatches = $state(false);

  const normalize = (label: string) => label.toLowerCase().replace(/\s+/g, '');
  const excluded = 'dialog, [hidden], [aria-hidden="true"], .sr-only, select, .choice-grid button';

  function labelText(label: HTMLElement): string {
    const walker = document.createTreeWalker(label, NodeFilter.SHOW_TEXT, {
      acceptNode: node => node.parentElement?.closest(excluded) ? NodeFilter.FILTER_REJECT : NodeFilter.FILTER_ACCEPT,
    });
    let text = '';
    while (walker.nextNode()) text += walker.currentNode.nodeValue;
    return text;
  }

  $effect(() => {
    const needle = normalize(query);
    if (!root || !needle) {
      noMatches = false;
      return;
    }

    const page = root;
    const filtered = new Set<Element>();
    function hide(element?: Element | null) {
      if (!element) return;
      filtered.add(element);
      element.classList.add('settings-filtered');
    }

    function restore() {
      for (const element of filtered) element.classList.remove('settings-filtered');
      filtered.clear();
    }

    function filter() {
      restore();
      const matches = new Map<Element, boolean>();
      let found = false;
      for (const label of page.querySelectorAll<HTMLElement>('.card :is(legend, label, button, summary)')) {
        if (label.closest(excluded)) continue;
        const unit = label.closest('form, [data-settings-pinned]') ?? label;
        const match = normalize(labelText(label)).includes(needle);
        found ||= match;
        matches.set(unit, matches.get(unit) || match);
      }

      const cards = new Set<Element>();
      for (const [label, match] of matches) {
        if (match || label.hasAttribute('data-settings-pinned')) {
          const card = label.closest('.card');
          if (card) cards.add(card);
          continue;
        }
        if (label.hasAttribute('data-settings-context')) continue;
        if (label.tagName === 'LEGEND') {
          hide(label.parentElement);
          const hint = label.parentElement?.nextElementSibling;
          if (hint?.matches('.hint')) hide(hint);
        } else {
          hide(label);
          const control = (label as HTMLLabelElement).control;
          hide(control);
          for (const id of (control?.getAttribute('aria-describedby') || '').split(' ')) {
            if (id) hide(document.getElementById(id));
          }
        }
      }
      for (const card of page.querySelectorAll(':scope > .card')) {
        if (!cards.has(card) && !card.querySelector('dialog[open]')) hide(card);
      }
      noMatches = !found;
    }

    filter();
    const observer = new MutationObserver(filter);
    observer.observe(page, { childList: true, characterData: true, subtree: true });
    return () => {
      observer.disconnect();
      restore();
    };
  });
</script>

<label class="settings-search">
  Search settings
  <input type="search" bind:value={query} autocomplete="off" spellcheck="false" autocapitalize="off" />
</label>
<p class="settings-search-status" role="status">{noMatches ? 'No settings match.' : ''}</p>
