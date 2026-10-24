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

  it('keeps one list across blank lines between its items', () => {
    expect(safeMarkdownHtml('1. a\n\n2. b\n\n3. c')).toBe('<ol><li>a</li><li>b</li><li>c</li></ol>');
  });

  it('nests bullets that a blank line separates from their step', () => {
    expect(safeMarkdownHtml('1. First\n\n   - a\n   - b\n\n2. Second'))
      .toBe('<ol><li>First<ul><li>a</li><li>b</li></ul></li><li>Second</li></ol>');
  });

  it('renders an indented paragraph after a blank line inside its item', () => {
    expect(safeMarkdownHtml('1. First\n\n   More about it.\n   Still more.\n2. Second'))
      .toBe('<ol><li>First<p>More about it.<br>Still more.</p></li><li>Second</li></ol>');
  });

  it('keeps the number of a list that a code block interrupts', () => {
    expect(safeMarkdownHtml('1. Run:\n```sh\nmake\n```\n2. Then'))
      .toBe('<ol><li>Run:</li></ol><pre><code class="language-sh">make</code></pre><ol start="2"><li>Then</li></ol>');
  });

  it('keeps an indented continuation line inside its item', () => {
    expect(safeMarkdownHtml('- item\n  more text\n- next')).toBe('<ul><li>item<br>more text</li><li>next</li></ul>');
  });

  it('attaches a continuation line to the item it is indented under without an empty line', () => {
    expect(safeMarkdownHtml('- parent\n  - child\n  parent continuation\n- next'))
      .toBe('<ul><li>parent<ul><li>child</li></ul>parent continuation</li><li>next</li></ul>');
  });

  it('escapes continuation lines', () => {
    expect(safeMarkdownHtml('- item\n  <img src=x>')).toBe('<ul><li>item<br>&lt;img src=x&gt;</li></ul>');
  });

  it('still renders an indented quote after a list item as a quote', () => {
    expect(safeMarkdownHtml('- item\n  > quoted')).toBe('<ul><li>item</li></ul><blockquote>quoted</blockquote>');
  });

  it('ends a list at an unindented paragraph after a blank line', () => {
    expect(safeMarkdownHtml('- a\n- b\n\nafter')).toBe('<ul><li>a</li><li>b</li></ul><p>after</p>');
  });

  it('treats a number longer than nine digits as text', () => {
    expect(safeMarkdownHtml('1234567890. not a list')).toBe('<p>1234567890. not a list</p>');
    expect(safeMarkdownHtml('123456789. a list')).toBe('<ol start="123456789"><li>a list</li></ol>');
  });
});
