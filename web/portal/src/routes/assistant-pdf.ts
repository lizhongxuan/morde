import pdfWorkerURL from 'pdfjs-dist/build/pdf.worker.min.mjs?url';
import tesseractCoreURL from 'tesseract.js-core/tesseract-core-lstm.wasm.js?url';
import { createWorker, OEM } from 'tesseract.js';

const maxAssistantPDFBytes = 16 * 1024 * 1024;
const maxAssistantPDFPages = 100;
const maxAssistantPDFTextBytes = 48 * 1024;
const maxAssistantPDFOCRPages = 5;
const maxAssistantPDFOCRPixels = 2_000_000;
const maxAssistantPDFOCRSide = 2400;
const maxAssistantPDFOCRScale = 1.5;
const truncatedPDFTextMarker = '\n[PDF 文字已截断：达到读取上限。]';
const truncatedPDFTextMarkerBytes = new TextEncoder().encode(truncatedPDFTextMarker).length;

type PDFTextItem = { str: string; hasEOL?: boolean };
type PDFRenderTask = { promise: Promise<unknown>; cancel(): void };

function utf8ByteLength(character: string): number {
  const codePoint = character.codePointAt(0) ?? 0;
  if (codePoint <= 0x7f) return 1;
  if (codePoint <= 0x7ff) return 2;
  return codePoint <= 0xffff ? 3 : 4;
}

function createBoundedText() {
  const parts: string[] = [];
  let outputBytes = 0;
  let truncated = false;
  return {
    append(value: string): boolean {
      for (const character of value) {
        const bytes = utf8ByteLength(character);
        if (outputBytes + bytes + truncatedPDFTextMarkerBytes > maxAssistantPDFTextBytes) {
          truncated = true;
          return false;
        }
        parts.push(character);
        outputBytes += bytes;
      }
      return true;
    },
    finish(): string {
      const text = parts.join('').trim();
      if (!truncated) return text;
      let bounded = '';
      let bytes = 0;
      for (const character of text) {
        const size = utf8ByteLength(character);
        if (bytes + size + truncatedPDFTextMarkerBytes > maxAssistantPDFTextBytes) break;
        bounded += character;
        bytes += size;
      }
      return `${bounded}${truncatedPDFTextMarker}`;
    },
  };
}

function abortError(): DOMException {
  return new DOMException('PDF 文字提取已取消。', 'AbortError');
}

function throwIfAborted(signal?: AbortSignal): void {
  if (signal?.aborted) throw abortError();
}

function waitForAbortable<T>(promise: Promise<T>, signal?: AbortSignal): Promise<T> {
  if (!signal) return promise;
  if (signal.aborted) return Promise.reject(abortError());
  return new Promise((resolve, reject) => {
    const cleanup = () => signal.removeEventListener('abort', onAbort);
    const onAbort = () => { cleanup(); reject(abortError()); };
    signal.addEventListener('abort', onAbort, { once: true });
    promise.then((value) => { cleanup(); resolve(value); }, (error: unknown) => { cleanup(); reject(error); });
  });
}

function localAssetURL(path: string): string {
  const url = new URL(path, globalThis.location.href);
  if (url.origin !== globalThis.location.origin) throw new Error('OCR 资源必须从当前 Morde 服务加载。');
  return url.href;
}

function applicationAssetURL(path: string): string {
  const base = new URL(import.meta.env.BASE_URL, globalThis.location.href);
  return localAssetURL(new URL(path, base).href);
}

async function createLocalOCRWorker(signal?: AbortSignal) {
  throwIfAborted(signal);
  const langPath = applicationAssetURL('ocr/');
  return createWorker('chi_sim', OEM.LSTM_ONLY, {
    workerPath: applicationAssetURL('ocr/tesseract-worker.js'),
    corePath: localAssetURL(tesseractCoreURL),
    langPath,
    workerBlobURL: false,
    cacheMethod: 'none',
    gzip: true,
  });
}

export async function extractAssistantPDFText(file: File, signal?: AbortSignal): Promise<string> {
  if (file.size < 1) throw new Error('PDF 文件为空。');
  if (file.size > maxAssistantPDFBytes) throw new Error('PDF 文件不能超过 16 MiB。');
  throwIfAborted(signal);

  const pdfjs = await import('pdfjs-dist');
  pdfjs.GlobalWorkerOptions.workerSrc = pdfWorkerURL;
  const loadingTask = pdfjs.getDocument({
    data: new Uint8Array(await file.arrayBuffer()),
    disableFontFace: true,
    maxImageSize: 0,
    stopAtErrors: true,
    useWasm: false,
  });

  let renderTask: PDFRenderTask | undefined;
  let ocrWorker: Awaited<ReturnType<typeof createLocalOCRWorker>> | undefined;
  let destroyPromise: Promise<void> | undefined;
  let terminatePromise: Promise<void> | undefined;
  const destroyPDF = () => destroyPromise ??= loadingTask.destroy();
  const terminateOCR = () => ocrWorker ? terminatePromise ??= ocrWorker.terminate().then(() => undefined) : Promise.resolve();
  const cancelWork = () => {
    renderTask?.cancel();
    void destroyPDF();
    void terminateOCR();
  };
  signal?.addEventListener('abort', cancelWork, { once: true });

  try {
    const document = await waitForAbortable(loadingTask.promise, signal);
    if (document.numPages > maxAssistantPDFPages) {
      throw new Error(`PDF 页数不能超过 ${maxAssistantPDFPages} 页。`);
    }

    const extracted = createBoundedText();
    let hasMeaningfulText = false;
    for (let pageNumber = 1; pageNumber <= document.numPages; pageNumber += 1) {
      throwIfAborted(signal);
      const page = await waitForAbortable(document.getPage(pageNumber), signal);
      try {
        const textContent = await waitForAbortable(page.getTextContent({ includeMarkedContent: false }), signal);
        if (pageNumber > 1) extracted.append('\n');
        extracted.append(`[第 ${pageNumber} 页]\n`);
        for (const value of textContent.items) {
          if (typeof value !== 'object' || value === null || !('str' in value)) continue;
          const item = value as PDFTextItem;
          if (item.str.trim()) hasMeaningfulText = true;
          if (!extracted.append(item.str) || (item.hasEOL && !extracted.append('\n'))) break;
        }
        extracted.append('\n');
      } finally {
        page.cleanup();
      }
    }

    if (hasMeaningfulText) return extracted.finish();

    const recognized = createBoundedText();
    let hasOCRText = false;
    const pagesToOCR = Math.min(document.numPages, maxAssistantPDFOCRPages);
    const pendingWorker = createLocalOCRWorker(signal).then((worker) => {
      ocrWorker = worker;
      if (signal?.aborted) void terminateOCR();
      return worker;
    });
    const worker = await waitForAbortable(pendingWorker, signal);
    for (let pageNumber = 1; pageNumber <= pagesToOCR; pageNumber += 1) {
      throwIfAborted(signal);
      const page = await waitForAbortable(document.getPage(pageNumber), signal);
      const canvas = globalThis.document.createElement('canvas');
      try {
        const naturalViewport = page.getViewport({ scale: 1 });
        const naturalPixels = naturalViewport.width * naturalViewport.height;
        if (!Number.isFinite(naturalPixels) || naturalPixels <= 0) throw new Error('扫描件页面尺寸无效。');
        const scale = Math.min(maxAssistantPDFOCRScale, Math.sqrt(maxAssistantPDFOCRPixels / naturalPixels),
          maxAssistantPDFOCRSide / Math.max(naturalViewport.width, naturalViewport.height));
        if (!Number.isFinite(scale) || scale < 0.05) throw new Error('扫描件页面尺寸过大，无法安全进行 OCR。');
        const viewport = page.getViewport({ scale });
        canvas.width = Math.max(1, Math.floor(viewport.width));
        canvas.height = Math.max(1, Math.floor(viewport.height));
        const pixels = canvas.width * canvas.height;
        if (!Number.isSafeInteger(pixels) || pixels > maxAssistantPDFOCRPixels) throw new Error('扫描件页面超过 OCR 像素上限。');
        const context = canvas.getContext('2d', { willReadFrequently: true });
        if (!context) throw new Error('浏览器无法创建 PDF 页面画布。');
        renderTask = page.render({ canvas, canvasContext: context, viewport });
        await waitForAbortable(renderTask.promise, signal);
        renderTask = undefined;
        const result = await waitForAbortable(worker.recognize(canvas), signal);
        if (result.data.text.trim()) hasOCRText = true;
        if (pageNumber > 1) recognized.append('\n');
        recognized.append(`[第 ${pageNumber} 页]\n`);
        recognized.append(result.data.text);
        recognized.append('\n');
      } finally {
        renderTask?.cancel();
        renderTask = undefined;
        canvas.width = 0;
        canvas.height = 0;
        page.cleanup();
      }
      if (signal?.aborted) throw abortError();
      if (recognized.finish().includes(truncatedPDFTextMarker)) break;
    }
    if (!hasOCRText) throw new Error('PDF 中没有可提取或可识别的文字。');
    if (document.numPages > pagesToOCR) recognized.append(`\n[扫描件 OCR 仅处理前 ${pagesToOCR} 页，共 ${document.numPages} 页。]`);
    return recognized.finish();
  } finally {
    signal?.removeEventListener('abort', cancelWork);
    renderTask?.cancel();
    await terminateOCR();
    await destroyPDF();
  }
}
