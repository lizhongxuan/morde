import ReactMarkdown from 'react-markdown';
import remarkGfm from 'remark-gfm';
import { ChatMermaid } from './ChatMermaid';

export default function ChatMarkdownContent({ content }: { content: string }) {
  return <div className="chat-markdown">
    <ReactMarkdown
      remarkPlugins={[remarkGfm]}
      skipHtml
      components={{
        pre: ({ children, node }) => {
          const containsMermaidCode = node?.children.some((child) => {
            if (child.type !== 'element' || child.tagName !== 'code') return false;
            const classes = child.properties.className;
            return Array.isArray(classes) && classes.includes('language-mermaid');
          });
          return containsMermaidCode ? <>{children}</> : <pre>{children}</pre>;
        },
        code: ({ className, children }) => {
          if (className?.split(/\s+/).includes('language-mermaid')) {
            return <ChatMermaid source={String(children).replace(/\n$/, '')} />;
          }
          return <code className={className}>{children}</code>;
        },
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
