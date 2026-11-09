import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { ChatMarkdown } from './ChatMarkdown';

describe('ChatMarkdown', () => {
  it('renders common markdown and GFM tables as readable elements', async () => {
    render(<ChatMarkdown content={'**重点**\n\n- 搜索\n- 总结\n\n| 来源 | 日期 |\n| --- | --- |\n| 官方公告 | 今天 |'} />);

    expect((await screen.findByText('重点', {}, { timeout: 5_000 })).tagName).toBe('STRONG');
    expect(screen.getByRole('list').querySelectorAll('li')).toHaveLength(2);
    expect(screen.getByRole('table')).toBeInTheDocument();
  });

  it('keeps HTML inert, rejects unsafe link schemes and does not load remote images', async () => {
    render(<ChatMarkdown content={'<img src="https://tracker.invalid/pixel" onerror="alert(1)">\n\n[unsafe](javascript:alert(1))\n\n![remote image](https://tracker.invalid/image.png)\n\n[article](https://example.com/news)'} />);

    await screen.findByText('图片：remote image');
    expect(document.querySelector('img')).not.toBeInTheDocument();
    expect(screen.getByText('图片：remote image')).toBeInTheDocument();
    expect(screen.getByText('unsafe').closest('a')).toBeNull();
    expect(screen.getByRole('link', { name: 'article' })).toHaveAttribute('target', '_blank');
    expect(screen.getByRole('link', { name: 'article' })).toHaveAttribute('rel', 'noopener noreferrer');
  });
});
