import { expect, it } from 'vitest';
import { consumeServerSentEvents } from './client';

it('parses split SSE frames and joins multiline data', async () => {
  const encoder = new TextEncoder();
  const chunks = [
    'id: 17\r\nevent: task.message\r',
    '\ndata: {"text":\r\ndata: "hello"}\r\n\r\n: heartbeat\r\nid: 18\nevent: task.status\ndata: {"status":"done"}\n\n',
  ];
  const response = new Response(new ReadableStream<Uint8Array>({
    start(controller) {
      chunks.forEach((chunk) => controller.enqueue(encoder.encode(chunk)));
      controller.close();
    },
  }));
  const events: { event: string; data: string; id: string }[] = [];

  await consumeServerSentEvents(response, (event) => events.push(event));

  expect(events).toEqual([
    { event: 'task.message', data: '{"text":\n"hello"}', id: '17' },
    { event: 'task.status', data: '{"status":"done"}', id: '18' },
  ]);
});

it('cancels a pending SSE stream when its signal aborts', async () => {
  let cancelled = false;
  const response = new Response(new ReadableStream<Uint8Array>({
    cancel() { cancelled = true; },
  }));
  const controller = new AbortController();
  const consuming = consumeServerSentEvents(response, () => undefined, controller.signal);

  controller.abort();
  await consuming;

  expect(cancelled).toBe(true);
});
