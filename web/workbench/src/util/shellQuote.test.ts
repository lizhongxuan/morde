import { describe, expect, it } from 'vitest';
import { shellQuote } from './shellQuote';

describe('shellQuote', () => {
  it.each([
    ["a'; rm -rf /", "'a'\\''; rm -rf /'"],
    ['name with spaces', "'name with spaces'"],
    ['line one\nline two', "'line one\nline two'"],
    ['$(touch /tmp/owned)', "'$(touch /tmp/owned)'"],
    ['`touch /tmp/owned`', "'`touch /tmp/owned`'"],
  ])('quotes %j as a single POSIX shell word', (input, expected) => {
    expect(shellQuote(input)).toBe(expected);
  });
});
