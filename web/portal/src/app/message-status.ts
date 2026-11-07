import type { components } from '../api/schema';

type ConversationMessage = components['schemas']['ConversationMessage'];

export function routingStatusText(status: ConversationMessage['routing_status']): string | null {
  if (status === 'pending') return '已受理，等待处理';
  if (status === 'processing') return '正在判断任务';
  if (status === 'waiting_user') return '等待补充或确认';
  if (status === 'waiting_adjustment') return '已收到修改，等待调整';
  if (status === 'failed') return '消息处理失败';
  return null;
}

export function deliveryStatusText(status: ConversationMessage['delivery_status'] | undefined): string {
  if (status === 'pending') return '已发送，等待处理';
  if (status === 'applied') return '已发送';
  return '已发送，处理状态待确认';
}

export function routingFailureText(reason: ConversationMessage['routing_failure_reason']): string {
  if (reason === 'model_unavailable') return '模型暂时不可用或用量待核对';
  if (reason === 'routing_context_unavailable') return '消息上下文暂时无法读取';
  if (reason === 'routing_action_unavailable') return '任务状态暂时无法更新';
  if (reason === 'message_too_long') return '消息及上下文超过当前模型输入上限，原文已保留；请缩短后重新发送。';
  return '处理暂时不可用';
}
