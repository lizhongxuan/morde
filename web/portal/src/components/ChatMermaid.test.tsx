import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ChatMarkdown } from './ChatMarkdown';

const { mermaidInitialize, mermaidRender } = vi.hoisted(() => ({
  mermaidInitialize: vi.fn(),
  mermaidRender: vi.fn(),
}));

vi.mock('mermaid', () => ({
  default: {
    initialize: mermaidInitialize,
    render: mermaidRender,
  },
}));

const flowchart = 'flowchart LR\n  Start --> Finish';
let previousClipboard: PropertyDescriptor | undefined;

describe('ChatMarkdown Mermaid rendering', () => {
  beforeEach(() => {
    previousClipboard = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
    mermaidInitialize.mockClear();
    mermaidRender.mockReset();
    mermaidRender.mockResolvedValue({
      svg: '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 160 60"><style>.node{fill:red}</style><g class="node"><rect x="0" y="0" width="70" height="30"/><text x="4" y="20">Start</text></g><path class="flowchart-link" d="M70 15 L100 15"/><text x="105" y="20">Finish</text></svg>',
    });
  });

  afterEach(() => {
    cleanup();
    if (previousClipboard) Object.defineProperty(navigator, 'clipboard', previousClipboard);
    else Reflect.deleteProperty(navigator, 'clipboard');
  });

  it('renders a flowchart as safe inline SVG', async () => {
    render(<ChatMarkdown content={`\`\`\`mermaid\n${flowchart}\n\`\`\``} />);

    const figure = await screen.findByRole('figure', { name: 'Mermaid 流程图' });
    await waitFor(() => expect(figure.querySelector('svg')).toBeInTheDocument());
    expect(figure.parentElement?.tagName).not.toBe('PRE');
    expect(figure.querySelector('svg')).toBeInTheDocument();
    expect(figure.querySelectorAll('text')).toHaveLength(2);
    expect(figure).toHaveTextContent('Start');
    expect(figure).toHaveTextContent('Finish');
    expect(figure.querySelector('style')).toBeNull();
    expect(mermaidInitialize).toHaveBeenCalledWith(expect.objectContaining({
      startOnLoad: false,
      securityLevel: 'strict',
      htmlLabels: false,
      maxEdges: expect.any(Number),
    }));
    expect(mermaidRender).toHaveBeenCalledWith(expect.stringMatching(/^morde-mermaid-/), flowchart);
  });

  it('shows invalid Mermaid syntax with the original copyable source', async () => {
    mermaidRender.mockRejectedValue(new Error('Syntax error'));
    render(<ChatMarkdown content={`\`\`\`mermaid\nflowchart LR\n  Start -->\n\`\`\``} />);

    expect(await screen.findByRole('status')).toHaveTextContent('此 Mermaid 图表暂不能安全显示');
    const figure = screen.getByRole('figure', { name: 'Mermaid 流程图' });
    expect(figure.parentElement?.tagName).not.toBe('PRE');
    expect(figure.querySelector('pre code')?.textContent).toBe('flowchart LR\n  Start -->');
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
    fireEvent.click(screen.getByRole('button', { name: '复制 Mermaid 源码' }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith('flowchart LR\n  Start -->'));
    expect(document.querySelector('svg')).toBeNull();
  });

  it('keeps unsafe HTML and injected SVG inert without breaking surrounding Markdown', async () => {
    mermaidRender.mockResolvedValue({
      svg: '<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><foreignObject><img src="https://tracker.invalid/pixel" onerror="alert(1)"/></foreignObject><script>alert(1)</script></svg>',
    });
    const unsafeSource = 'flowchart LR\n  A[<img src="https://tracker.invalid/pixel" onerror="alert(1)">] --> B';
    render(<ChatMarkdown content={`Before **safe**\n\n\`\`\`mermaid\n${unsafeSource}\n\`\`\`\n\n\`\`\`mermaid\n${flowchart}\n\`\`\`\n\nAfter **still safe**`} />);

    await waitFor(() => expect(screen.getAllByText('此 Mermaid 图表暂不能安全显示，源码仍可复制。')).toHaveLength(2));
    expect(screen.getByText('safe').tagName).toBe('STRONG');
    expect(screen.getByText('still safe').tagName).toBe('STRONG');
    expect(screen.getAllByRole('figure')[0]?.querySelector('pre code')?.textContent).toBe(unsafeSource);
    expect(document.querySelector('script, img, a, foreignObject')).toBeNull();
    expect(document.querySelector('svg')).toBeNull();
    expect(mermaidRender).toHaveBeenCalledOnce();
    expect(mermaidRender).toHaveBeenCalledWith(expect.stringMatching(/^morde-mermaid-/), flowchart);
  });
});
