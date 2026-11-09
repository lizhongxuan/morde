// @vitest-environment node
import { fileURLToPath } from 'node:url';
import { readFile } from 'node:fs/promises';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { extractAssistantPDFText } from './assistant-pdf';

const pdfMock = vi.hoisted(() => ({
  globalWorkerOptions: { workerSrc: '' },
  getDocument: vi.fn(),
}));

const tesseractMock = vi.hoisted(() => ({
  createWorker: vi.fn(),
}));

vi.mock('pdfjs-dist', () => ({
  GlobalWorkerOptions: pdfMock.globalWorkerOptions,
  getDocument: pdfMock.getDocument,
}));

vi.mock('tesseract.js', () => ({
  createWorker: tesseractMock.createWorker,
  OEM: { LSTM_ONLY: 1 },
}));

type MockPage = {
  getTextContent: ReturnType<typeof vi.fn>;
  getViewport: ReturnType<typeof vi.fn>;
  render: ReturnType<typeof vi.fn>;
  cleanup: ReturnType<typeof vi.fn>;
};

function configurePDF(pages: Array<Array<{ str: string; hasEOL?: boolean }>>, size = { width: 300, height: 144 }) {
  const pageProxies: MockPage[] = pages.map((items) => ({
    getTextContent: vi.fn(async () => ({ items })),
    getViewport: vi.fn(({ scale }: { scale: number }) => ({ width: size.width * scale, height: size.height * scale })),
    render: vi.fn(() => ({ promise: Promise.resolve(), cancel: vi.fn() })),
    cleanup: vi.fn(),
  }));
  const getPage = vi.fn(async (pageNumber: number) => pageProxies[pageNumber - 1]!);
  const destroy = vi.fn(async () => undefined);
  pdfMock.getDocument.mockReturnValue({
    promise: Promise.resolve({ numPages: pages.length, getPage, destroy }),
    destroy,
  });
  return { destroy, getPage, pageProxies };
}

function configureOCR(texts: string[] = ['扫描件识别结果']) {
  const canvasSizes: Array<{ width: number; height: number }> = [];
  const worker = {
    recognize: vi.fn(async (canvas: { width: number; height: number }) => {
      canvasSizes.push({ width: canvas.width, height: canvas.height });
      return { data: { text: texts[Math.min(canvasSizes.length - 1, texts.length - 1)] ?? '' } };
    }),
    terminate: vi.fn(async () => undefined),
  };
  tesseractMock.createWorker.mockResolvedValue(worker);
  const canvas = { width: 0, height: 0, getContext: vi.fn(() => ({})) };
  const fetch = vi.fn();
  vi.stubGlobal('location', new URL('http://127.0.0.1:5173/assistant'));
  vi.stubGlobal('document', { createElement: vi.fn(() => canvas) });
  vi.stubGlobal('fetch', fetch);
  return { worker, canvas, canvasSizes, fetch };
}

describe('extractAssistantPDFText', () => {
  beforeEach(() => {
    pdfMock.getDocument.mockReset();
    tesseractMock.createWorker.mockReset();
  });

  afterEach(() => vi.unstubAllGlobals());

  it('extracts page text and skips OCR when PDF.js finds text', async () => {
    const { destroy, getPage } = configurePDF([
      [{ str: 'Morde 工作说明', hasEOL: true }, { str: '本地提取' }],
      [{ str: '第二页' }],
    ]);
    const file = new File(['synthetic PDF bytes'], 'brief.pdf', { type: 'application/pdf' });

    const text = await extractAssistantPDFText(file);

    expect(text).toContain('[第 1 页]\nMorde 工作说明\n本地提取');
    expect(text).toContain('[第 2 页]\n第二页');
    expect(pdfMock.getDocument).toHaveBeenCalledWith(expect.objectContaining({
      data: expect.any(Uint8Array), maxImageSize: 0, stopAtErrors: true, useWasm: false,
    }));
    expect(pdfMock.globalWorkerOptions.workerSrc).toContain('pdf.worker.min.mjs');
    expect(getPage).toHaveBeenCalledTimes(2);
    expect(destroy).toHaveBeenCalledOnce();
    expect(tesseractMock.createWorker).not.toHaveBeenCalled();
  });

  it('runs local OCR for scanned pages and terminates the worker', async () => {
    const { destroy, pageProxies } = configurePDF([[]]);
    const { worker, canvas, fetch } = configureOCR();

    const text = await extractAssistantPDFText(new File(['synthetic PDF bytes'], 'scan.pdf'));

    expect(text).toContain('[第 1 页]\n扫描件识别结果');
    expect(fetch).not.toHaveBeenCalled();
    expect(tesseractMock.createWorker).toHaveBeenCalledWith(
      'chi_sim',
      1,
      expect.objectContaining({
        workerPath: 'http://127.0.0.1:5173/ocr/tesseract-worker.js',
        corePath: expect.stringMatching(/^http:\/\/127\.0\.0\.1:5173\//),
        langPath: 'http://127.0.0.1:5173/ocr/',
        workerBlobURL: false,
        cacheMethod: 'none',
      }),
    );
    expect(worker.recognize).toHaveBeenCalledWith(canvas);
    expect(worker.terminate).toHaveBeenCalledOnce();
    expect(pageProxies[0]?.cleanup).toHaveBeenCalledTimes(2);
    expect(destroy).toHaveBeenCalledOnce();
  });

  it('enforces OCR page and pixel caps', async () => {
    const { pageProxies } = configurePDF(Array.from({ length: 7 }, () => []), { width: 5000, height: 3000 });
    const { canvasSizes } = configureOCR(Array(5).fill('识别文字'));

    const text = await extractAssistantPDFText(new File(['synthetic PDF bytes'], 'large-scan.pdf'));

    expect(text).toContain('仅处理前 5 页，共 7 页');
    expect(canvasSizes).toHaveLength(5);
    expect(canvasSizes.every(({ width, height }) => width * height <= 2_000_000 && Math.max(width, height) <= 2400)).toBe(true);
    pageProxies.slice(5).forEach((page) => expect(page.render).not.toHaveBeenCalled());
  });

  it('caps OCR-derived UTF-8 text at 48 KiB', async () => {
    configurePDF([[]]);
    configureOCR(['中'.repeat(30_000)]);

    const text = await extractAssistantPDFText(new File(['synthetic PDF bytes'], 'long-scan.pdf'));

    expect(text).toContain('[PDF 文字已截断：达到读取上限。]');
    expect(new TextEncoder().encode(text).byteLength).toBeLessThanOrEqual(48 * 1024);
  });

  it('aborts OCR and releases the worker and PDF resources on cancellation', async () => {
    const { destroy } = configurePDF([[]]);
    const { worker } = configureOCR();
    worker.recognize.mockImplementation(() => new Promise(() => undefined));
    const controller = new AbortController();
    const extraction = extractAssistantPDFText(new File(['synthetic PDF bytes'], 'cancel.pdf'), controller.signal);

    await vi.waitFor(() => expect(worker.recognize).toHaveBeenCalledOnce());
    controller.abort();
    await expect(extraction).rejects.toMatchObject({ name: 'AbortError' });

    expect(worker.terminate).toHaveBeenCalledOnce();
    expect(destroy).toHaveBeenCalledOnce();
  });

  it('recognizes a local synthetic scan with the installed Tesseract engine and model', async () => {
    const tesseract = await vi.importActual<typeof import('tesseract.js')>('tesseract.js');
    const langPath = fileURLToPath(new URL('../../public/ocr/', import.meta.url));
    const worker = await tesseract.createWorker('chi_sim', tesseract.OEM.LSTM_ONLY, {
      langPath, cacheMethod: 'none', gzip: true,
    });
    try {
      const png = await readFile(new URL('./fixtures/assistant-pdf-ocr.png', import.meta.url));
      const result = await worker.recognize(png);
      expect(result.data.text.replace(/\s/g, '').toUpperCase()).toContain('MORDE');
    } finally {
      await worker.terminate();
    }
  }, 60_000);

  it('rejects empty, oversized, excessive-page PDFs and caps extracted text', async () => {
    await expect(extractAssistantPDFText(new File([], 'empty.pdf'))).rejects.toThrow('PDF 文件为空');
    await expect(extractAssistantPDFText(new File([new Uint8Array(16 * 1024 * 1024 + 1)], 'large.pdf')))
      .rejects.toThrow('不能超过 16 MiB');
    expect(pdfMock.getDocument).not.toHaveBeenCalled();

    const excessive = configurePDF(Array.from({ length: 101 }, () => []));
    await expect(extractAssistantPDFText(new File(['pdf'], 'many-pages.pdf'))).rejects.toThrow('不能超过 100 页');
    expect(excessive.getPage).not.toHaveBeenCalled();
    expect(excessive.destroy).toHaveBeenCalledOnce();

    configurePDF([[{ str: '中'.repeat(20_000) }]]);
    const text = await extractAssistantPDFText(new File(['pdf'], 'long.pdf'));
    expect(text).toContain('[PDF 文字已截断：达到读取上限。]');
    expect(new TextEncoder().encode(text).byteLength).toBeLessThanOrEqual(48 * 1024);
  });
});
