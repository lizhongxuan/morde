import { expect, it } from 'vitest';
import { MessageReceiptOrder } from './message-receipt-order';

it('waits for the prior receipt only within its original account and service scope', async () => {
  const order = new MessageReceiptOrder();
  const finish = order.begin('account-and-service-a', 'message-001');
  const prior = order.previous('account-and-service-a', 'message-001');
  expect(prior).toBeDefined();
  expect(order.previous('account-and-service-b', 'message-001')).toBeUndefined();
  finish(false);
  expect(await prior).toBe(false);
  expect(order.previous('account-and-service-a', 'message-001')).toBeUndefined();
  const retry = order.begin('account-and-service-a', 'message-001');
  const retried = order.previous('account-and-service-a', 'message-001');
  retry(true);
  expect(await retried).toBe(true);
});
