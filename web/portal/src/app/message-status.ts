import type { components } from '../api/schema';

type ConversationMessage = components['schemas']['ConversationMessage'];

export function routingStatusText(status: ConversationMessage['routing_status']): string | null {
  if (status === 'pending') return '已受理，等待处理';
  if (status === 'processing') return '正在处理消息';
  if (status === 'waiting_user') return '等待补充或确认';
  if (status === 'waiting_adjustment') return '已收到修改，等待调整';
  if (status === 'failed') return '消息处理失败';
  return null;
}

/** Keep routine routing receipts out of the user bubble; Task cards show actionable Task state separately. */
export function userVisibleRoutingStatusText(status: ConversationMessage['routing_status']): string | null {
  if (status === 'pending') return '已收到，正在等待处理';
  if (status === 'processing') return 'Agent 正在理解你的请求';
  if (status === 'waiting_adjustment') return '已收到修改，正在更新任务';
  return null;
}

export function deliveryStatusText(status: ConversationMessage['delivery_status'] | undefined): string {
  if (status === 'pending') return '已发送，等待处理';
  if (status === 'applied') return '已发送';
  return '已发送，处理状态待确认';
}

export function routingFailureText(reason: ConversationMessage['routing_failure_reason']): string {
  // This route category also covers provider errors and timeouts; it does not
  // establish that the model is unavailable or that the user is out of quota.
  if (reason === 'model_unavailable') return '暂时无法确定具体原因';
  if (reason === 'routing_context_unavailable') return '暂时无法获取完整的会话上下文';
  if (reason === 'message_too_long') return '消息及上下文超过当前模型输入上限，原文已保留；请缩短后重新发送。';
  return '暂时无法完成消息处理';
}
