import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';

export default function ChatMarkdownContent({ content }: { content: string }) {
  return <div className="chat-markdown">
    <ReactMarkdown
      remarkPlugins={[remarkGfm]}
      skipHtml
      components={{
        a: ({ href, children }) => {
          const external = href?.startsWith('https://') || href?.startsWith('http://');
          return href ? <a href={href} target={external ? '_blank' : undefined}
            rel={external ? 'noopener noreferrer' : undefined}>{children}</a> : <>{children}</>;
        },
        // Avoid loading model-provided remote images implicitly in the user's browser.
        img: ({ alt }) => alt ? <span className="chat-markdown-image-note">图片：{alt}</span> : null,
      }}
    >{content}</ReactMarkdown>
  </div>;
}
