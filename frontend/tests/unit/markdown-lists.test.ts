import { describe, expect, it } from 'vitest';
import { safeMarkdownHtml } from '../../src/lib/markdown';

describe('conversation markdown lists', () => {
  it('nests bullets under numbered steps and keeps the numbering', () => {
    expect(safeMarkdownHtml('1. First step\n   - detail a\n   - detail b\n2. Second step\n   - detail c\n3. Third step'))
      .toBe('<ol><li>First step<ul><li>detail a</li><li>detail b</li></ul></li><li>Second step<ul><li>detail c</li></ul></li><li>Third step</li></ol>');
  });

  it('nests two levels of bullets', () => {
    expect(safeMarkdownHtml('- a\n  - b\n    - c\n- d'))
      .toBe('<ul><li>a<ul><li>b<ul><li>c</li></ul></li></ul></li><li>d</li></ul>');
  });

  it('keeps the numbers of a list split by blank lines', () => {
    expect(safeMarkdownHtml('1. a\n\n2. b')).toBe('<ol><li>a</li></ol><ol start="2"><li>b</li></ol>');
  });

  it('keeps an indented continuation line inside its item', () => {
    expect(safeMarkdownHtml('- item\n  more text\n- next')).toBe('<ul><li>item<br>more text</li><li>next</li></ul>');
  });

  it('attaches a continuation line to the item it is indented under', () => {
    expect(safeMarkdownHtml('- parent\n  - child\n  parent continuation\n- next'))
      .toBe('<ul><li>parent<ul><li>child</li></ul><br>parent continuation</li><li>next</li></ul>');
  });

  it('still renders an indented quote after a list item as a quote', () => {
    expect(safeMarkdownHtml('- item\n  > quoted')).toBe('<ul><li>item</li></ul><blockquote>quoted</blockquote>');
  });

  it('renders a flat list as before', () => {
    expect(safeMarkdownHtml('- a\n- b\n\nafter')).toBe('<ul><li>a</li><li>b</li></ul><p>after</p>');
  });
});
