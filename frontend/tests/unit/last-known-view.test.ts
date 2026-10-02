import { render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import LastKnownView from '$components/LastKnownView.svelte';
import { projectLastKnown } from '$lib/last-known';
import { shownRelayRows } from '$lib/resume-metrics';

describe('read-only last-known presentation', () => {
  it('renders dated display rows without live selectors, links, buttons or targets', () => {
    const summary = projectLastKnown('local-relay', [{ name: 'Saved agent', agent: 'codex', status: 'idle', workspace_id: 'remote-workspace' }],
      [{ workspace_id: 'remote-workspace', label: 'Saved workspace' }], Date.now());
    const { container } = render(LastKnownView, { summary, relayLabel: 'Local computer' });
    expect(screen.getByRole('heading', { name: 'Local computer' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Saved workspace' })).toBeInTheDocument();
    expect(screen.getByRole('status')).toHaveTextContent('reconnecting; read-only');
    expect(container.querySelector('time[datetime]')).not.toBeNull();
    expect(container.querySelectorAll('a, button, article.agent-card, .agent-open, [data-live-relay]')).toHaveLength(0);
    vi.spyOn(container, 'getBoundingClientRect').mockReturnValue({ width: 100, height: 100 } as DOMRect);
    expect(shownRelayRows(container, 'local-relay')).toBe(false);
    expect(container.textContent).not.toContain('remote-workspace');
    expect(screen.getByRole('listitem')).toHaveTextContent('Saved agent — codex, idle');
  });
});
