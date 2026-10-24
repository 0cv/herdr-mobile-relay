export interface ConversationMessageViewState {
  tableOffsets: number[];
  openTools: Set<string>;
}

export class ConversationMessageStateStore {
  private scope = '';
  private entries = new Map<string, ConversationMessageViewState>();

  setScope(scope: string): void {
    if (scope === this.scope) return;
    this.scope = scope;
    this.entries.clear();
  }

  get(id: string): ConversationMessageViewState {
    let state = this.entries.get(id);
    if (!state) {
      if (this.entries.size >= 20_016) this.entries.delete(this.entries.keys().next().value!);
      state = { tableOffsets: [], openTools: new Set() };
      this.entries.set(id, state);
    }
    return state;
  }

  retain(ids: Set<string>): void {
    for (const id of this.entries.keys()) if (!ids.has(id)) this.entries.delete(id);
  }

  retainLive(ids: Set<string>): void {
    for (const id of this.entries.keys()) if (id.startsWith('live:') && !ids.has(id)) this.entries.delete(id);
  }

  clear(): void { this.entries.clear(); }
}
