import { lazy, Suspense } from 'react';

const ChatMarkdownContent = lazy(() => import('./ChatMarkdownContent'));

export function ChatMarkdown({ content }: { content: string }) {
  return <Suspense fallback={<div className="chat-markdown">{content}</div>}>
    <ChatMarkdownContent content={content} />
  </Suspense>;
}
