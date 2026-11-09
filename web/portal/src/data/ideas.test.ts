import { describe, expect, it } from 'vitest';
import { ideasFromFeedPreferences } from './ideas';

describe('ideasFromFeedPreferences', () => {
  it('creates bounded, deduplicated search suggestions from saved topics', () => {
    const ideas = ideasFromFeedPreferences('• 人工智能，机器人\n人工智能; 云端计算');

    expect(ideas.map((idea) => idea.title)).toEqual(['搜索：人工智能', '搜索：机器人', '搜索：云端计算']);
    expect(ideas[0]?.prompt).toContain('主题：人工智能');
    expect(ideas[0]?.prompt).toContain('来源链接和可核对的发布日期');
  });

  it('ignores blank and oversized fragments and returns no more than five topics', () => {
    const oversized = '界'.repeat(81);
    const ideas = ideasFromFeedPreferences(` ,\n${oversized},A,B,C,D,E,F`);

    expect(ideas.map((idea) => idea.title)).toEqual(['搜索：A', '搜索：B', '搜索：C', '搜索：D', '搜索：E']);
  });

  it('does not split text on full-width symbols inside an otherwise valid topic', () => {
    const ideas = ideasFromFeedPreferences('AI＋机器人（产业链）');

    expect(ideas).toHaveLength(1);
    expect(ideas[0]?.title).toBe('搜索：AI＋机器人（产业链）');
  });
});
