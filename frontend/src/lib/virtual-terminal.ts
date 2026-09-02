
export interface VirtualTerminalRange {
  start: number;
  end: number;
  top: number;
  bottom: number;
  total: number;
}

export class VirtualTerminalIndex {
  private sizes: number[] = [];
  private tree: number[] = [0];

  get length(): number {
    return this.sizes.length;
  }

  get total(): number {
    return this.prefix(this.sizes.length);
  }

  reset(sizes: readonly number[]): void {
    this.sizes = [...sizes];
    this.tree = [0, ...this.sizes];
    for (let index = 1; index < this.tree.length; index += 1) {
      const parent = index + (index & -index);
      if (parent < this.tree.length) this.tree[parent] += this.tree[index];
    }
  }

  size(index: number): number {
    return this.sizes[index] || 0;
  }

  offset(index: number): number {
    return this.prefix(Math.max(0, Math.min(index, this.sizes.length)));
  }

  update(index: number, size: number): number {
    if (index < 0 || index >= this.sizes.length || !Number.isFinite(size) || size <= 0) return 0;
    const delta = size - this.sizes[index];
    if (Math.abs(delta) < 0.25) return 0;
    this.sizes[index] = size;
    this.add(index, delta);
    return delta;
  }

  indexAt(offset: number): number {
    if (!this.sizes.length) return 0;
    const target = Math.max(0, Math.min(offset, this.total));
    let index = 0;
    let sum = 0;
    let bit = 1;
    while (bit * 2 < this.tree.length) bit *= 2;
    for (; bit > 0; bit = Math.floor(bit / 2)) {
      const next = index + bit;
      if (next < this.tree.length && sum + this.tree[next] <= target) {
        index = next;
        sum += this.tree[next];
      }
    }
    return Math.min(index, this.sizes.length - 1);
  }

  range(scrollTop: number, viewportHeight: number, overscan: number): VirtualTerminalRange {
    const total = this.total;
    if (!this.sizes.length) return { start: 0, end: 0, top: 0, bottom: 0, total };
    const startOffset = Math.max(0, scrollTop - overscan);
    const endOffset = Math.min(total, scrollTop + Math.max(0, viewportHeight) + overscan);
    const start = this.indexAt(startOffset);
    const end = Math.min(this.sizes.length, this.indexAt(endOffset) + 1);
    const top = this.offset(start);
    return {
      start,
      end,
      top,
      bottom: Math.max(0, total - this.offset(end)),
      total,
    };
  }

  private add(index: number, delta: number): void {
    for (let cursor = index + 1; cursor < this.tree.length; cursor += cursor & -cursor) {
      this.tree[cursor] += delta;
    }
  }

  private prefix(count: number): number {
    let sum = 0;
    for (let cursor = count; cursor > 0; cursor -= cursor & -cursor) sum += this.tree[cursor];
    return sum;
  }
}
