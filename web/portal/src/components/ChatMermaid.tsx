import { createElement, useEffect, useId, useState, type ReactNode } from 'react';
import './ChatMermaid.css';

const MAX_SOURCE_LENGTH = 16_000;
const MAX_DIAGRAM_LINES = 500;
const MAX_EDGES = 160;
const SVG_NAMESPACE = 'http://www.w3.org/2000/svg';

const allowedSvgElements = new Set([
  'svg', 'g', 'path', 'rect', 'circle', 'ellipse', 'line', 'polyline', 'polygon',
  'defs', 'marker', 'pattern', 'linearGradient', 'radialGradient', 'stop',
  'text', 'tspan', 'clipPath', 'mask', 'filter', 'feDropShadow', 'feGaussianBlur', 'feOffset',
  'feFlood', 'feColorMatrix', 'feComponentTransfer', 'feFuncA', 'feFuncB',
  'feFuncG', 'feFuncR', 'feComposite', 'feMerge', 'feMergeNode', 'title', 'desc',
]);

const normalizedSvgElementNames: Record<string, string> = { fedropshadow: 'feDropShadow' };

const allowedSvgAttributes = new Set([
  'aria-label', 'aria-labelledby', 'class', 'clip-path', 'color', 'cx', 'cy',
  'd', 'dominant-baseline', 'fill', 'fill-opacity', 'fill-rule', 'filter',
  'font-family', 'font-size', 'font-weight', 'height', 'id', 'marker-end',
  'marker-mid', 'marker-start', 'markerHeight', 'markerUnits', 'markerWidth', 'mask',
  'offset', 'opacity', 'orient', 'points', 'refX', 'refY',
  'preserveAspectRatio', 'r', 'role', 'rx', 'ry', 'shape-rendering', 'stop-color',
  'stop-opacity', 'stdDeviation', 'stroke', 'stroke-dasharray', 'stroke-linecap', 'stroke-linejoin',
  'stroke-opacity', 'stroke-width', 'text-anchor', 'transform', 'version', 'viewBox',
  'visibility', 'width', 'x', 'x1', 'x2', 'xmlns', 'y', 'y1', 'y2',
  'dx', 'dy', 'flood-color', 'flood-opacity',
]);

const svgAttributeNames: Record<string, string> = {
  'clip-path': 'clipPath',
  'dominant-baseline': 'dominantBaseline',
  'fill-opacity': 'fillOpacity',
  'fill-rule': 'fillRule',
  'font-family': 'fontFamily',
  'font-size': 'fontSize',
  'font-weight': 'fontWeight',
  'flood-color': 'floodColor',
  'flood-opacity': 'floodOpacity',
  'marker-end': 'markerEnd',
  'marker-mid': 'markerMid',
  'marker-start': 'markerStart',
  markerHeight: 'markerHeight',
  markerUnits: 'markerUnits',
  markerWidth: 'markerWidth',
  'shape-rendering': 'shapeRendering',
  'stop-color': 'stopColor',
  'stop-opacity': 'stopOpacity',
  'stroke-dasharray': 'strokeDasharray',
  'stroke-linecap': 'strokeLinecap',
  'stroke-linejoin': 'strokeLinejoin',
  'stroke-opacity': 'strokeOpacity',
  'stroke-width': 'strokeWidth',
  'text-anchor': 'textAnchor',
  class: 'className',
};

let mermaidInitialized = false;

function hasSafeFlowchartSource(source: string): boolean {
  if (!source.trim() || source.length > MAX_SOURCE_LENGTH) return false;

  const lines = source.split(/\r?\n/);
  if (lines.length > MAX_DIAGRAM_LINES) return false;

  const header = lines.find((line) => {
    const trimmed = line.trim();
    return trimmed && !trimmed.startsWith('%%');
  })?.trim();
  if (!header || !/^(?:flowchart|graph)(?:\s+(?:TB|TD|BT|RL|LR))?$/i.test(header)) return false;

  if (/%%\s*\{|^\s*---\s*$/im.test(source)) return false;
  if (/^\s*(?:click|classDef|class|style|linkStyle)\b/im.test(source)) return false;
  if (/\b(?:https?|ftp|file|data|javascript):/i.test(source)) return false;
  if (/!?\[[^\]]*\]\([^)]*\)/.test(source)) return false;
  if (/\b(?:shape\s*:\s*image|img\s*:)/i.test(source)) return false;
  if (/<\s*\/?\s*[a-z][^>]*>/i.test(source)) return false;

  return true;
}

function assertSafeSvgAttribute(name: string, value: string): void {
  if (/^on[a-z]/i.test(name) || /^(?:href|xlink:href|src|srcset)$/i.test(name)) {
    throw new Error('Unsupported interactive or external SVG attribute');
  }
  if (name === 'xmlns' && value === SVG_NAMESPACE) return;

  const references = value.matchAll(/url\(\s*(['"]?)(.*?)\1\s*\)/gi);
  for (const reference of references) {
    if (!/^#[\w:.-]+$/.test(reference[2] ?? '')) {
      throw new Error('External SVG references are not supported');
    }
  }

  if (/(?:https?:|ftp:|file:|data:|javascript:)/i.test(value)) {
    throw new Error('External SVG references are not supported');
  }
}

function svgNodeToReact(node: Node, key: number | string): ReactNode {
  if (node.nodeType === Node.TEXT_NODE) return node.textContent;
  if (node.nodeType !== Node.ELEMENT_NODE) return null;

  const element = node as Element;
  const tagName = normalizedSvgElementNames[element.localName] ?? element.localName;

  // Mermaid's generated CSS is discarded; the host stylesheet provides fixed safe styles.
  if (tagName === 'style') return null;
  if (element.namespaceURI !== SVG_NAMESPACE || !allowedSvgElements.has(tagName)) {
    throw new Error('Unsupported Mermaid SVG element');
  }

  const props: Record<string, string | number> = { key };
  for (const attribute of Array.from(element.attributes)) {
    const { name, value } = attribute;
    assertSafeSvgAttribute(name, value);
    if (!allowedSvgAttributes.has(name)) continue;
    props[svgAttributeNames[name] ?? name] = value;
  }

  const children = Array.from(element.childNodes).map((child, index) => svgNodeToReact(child, index));
  return createElement(tagName, props, ...children);
}

function parseSafeSvg(svg: string): ReactNode {
  if (typeof DOMParser === 'undefined') throw new Error('SVG parsing is unavailable');
  const parsed = new DOMParser().parseFromString(svg, 'image/svg+xml');
  if (parsed.querySelector('parsererror') || parsed.documentElement.localName !== 'svg') {
    throw new Error('Mermaid returned invalid SVG');
  }
  return svgNodeToReact(parsed.documentElement, 'diagram');
}

async function renderFlowchart(id: string, source: string): Promise<string> {
  const { default: mermaid } = await import('mermaid');
  if (!mermaidInitialized) {
    mermaid.initialize({
      startOnLoad: false,
      securityLevel: 'strict',
      htmlLabels: false,
      suppressErrorRendering: true,
      maxTextSize: MAX_SOURCE_LENGTH,
      maxEdges: MAX_EDGES,
    });
    mermaidInitialized = true;
  }

  const result = await mermaid.render(id, source);
  return result.svg;
}

type RenderState = { status: 'loading' } | { status: 'error' } | { status: 'success'; diagram: ReactNode };

export function ChatMermaid({ source }: { source: string }) {
  const reactId = useId();
  const diagramId = `morde-mermaid-${reactId.replace(/[^a-zA-Z0-9_-]/g, '')}`;
  const [renderResult, setRenderResult] = useState<{ source: string; state: RenderState }>({
    source: '',
    state: { status: 'loading' },
  });
  const [copyMessage, setCopyMessage] = useState('');
  const renderState = !hasSafeFlowchartSource(source)
    ? { status: 'error' } as const
    : renderResult.source === source ? renderResult.state : { status: 'loading' } as const;

  useEffect(() => {
    let cancelled = false;

    if (!hasSafeFlowchartSource(source)) return () => { cancelled = true; };
    void renderFlowchart(diagramId, source)
      .then((svg) => parseSafeSvg(svg))
      .then((diagram) => {
        if (!cancelled) setRenderResult({ source, state: { status: 'success', diagram } });
      })
      .catch(() => {
        if (!cancelled) setRenderResult({ source, state: { status: 'error' } });
      });

    return () => { cancelled = true; };
  }, [diagramId, source]);

  async function copySource() {
    try {
      await navigator.clipboard.writeText(source);
      setCopyMessage('源码已复制');
    } catch {
      setCopyMessage('无法自动复制，请从源码框选择并复制');
    }
  }

  const hasError = renderState.status === 'error';

  return <figure className="chat-markdown-mermaid" aria-label="Mermaid 流程图">
    {renderState.status === 'success' ? <div className="chat-markdown-mermaid-diagram">{renderState.diagram}</div>
      : <p className="chat-markdown-mermaid-status" role="status">
        {hasError ? '此 Mermaid 图表暂不能安全显示，源码仍可复制。' : '正在生成流程图…'}
      </p>}
    <div className="chat-markdown-mermaid-tools">
      <button type="button" onClick={() => { void copySource(); }}>复制 Mermaid 源码</button>
      {copyMessage && <span role="status">{copyMessage}</span>}
      {hasError ? <pre><code>{source}</code></pre>
        : <details>
          <summary>查看 Mermaid 源码</summary>
          <pre><code>{source}</code></pre>
        </details>}
    </div>
  </figure>;
}

