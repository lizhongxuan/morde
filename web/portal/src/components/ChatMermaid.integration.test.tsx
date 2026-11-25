import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest';
import { ChatMermaid } from './ChatMermaid';

const textLengthDescriptor = Object.getOwnPropertyDescriptor(SVGElement.prototype, 'getComputedTextLength');
const boundingBoxDescriptor = Object.getOwnPropertyDescriptor(SVGElement.prototype, 'getBBox');

describe('ChatMermaid with the pinned Mermaid renderer', () => {
  beforeAll(() => {
    Object.defineProperty(SVGElement.prototype, 'getComputedTextLength', {
      configurable: true,
      value(this: SVGElement) { return (this.textContent ?? '').length * 8; },
    });
    Object.defineProperty(SVGElement.prototype, 'getBBox', {
      configurable: true,
      value(this: SVGElement) {
        return { x: 0, y: 0, width: (this.textContent ?? '').length * 8, height: 20 };
      },
    });
  });

  afterEach(cleanup);

  afterAll(() => {
    if (textLengthDescriptor) Object.defineProperty(SVGElement.prototype, 'getComputedTextLength', textLengthDescriptor);
    else Reflect.deleteProperty(SVGElement.prototype, 'getComputedTextLength');
    if (boundingBoxDescriptor) Object.defineProperty(SVGElement.prototype, 'getBBox', boundingBoxDescriptor);
    else Reflect.deleteProperty(SVGElement.prototype, 'getBBox');
  });

  it('renders a real flowchart through Mermaid and keeps only safe SVG output', async () => {
    render(<ChatMermaid source={'flowchart LR\n  Start --> Finish'} />);

    const figure = await screen.findByRole('figure', { name: 'Mermaid 流程图' });
    await waitFor(() => expect(figure.querySelector('svg')).toBeInTheDocument());
    expect(figure.querySelectorAll('text')).not.toHaveLength(0);
    expect(figure).toHaveTextContent('Start');
    expect(figure).toHaveTextContent('Finish');
    expect(figure.querySelector('style, a, image, foreignObject, script')).toBeNull();
  });
});
