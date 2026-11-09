export interface PersonalIdea {
  id: string;
  emoji: string;
  title: string;
  body: string;
  prompt: string;
}

export interface PersonalIdeaSection {
  id: string;
  title: string;
  ideas: PersonalIdea[];
}

export function ideasFromFeedPreferences(content: string): PersonalIdea[] {
  const seen = new Set<string>();
  const result: PersonalIdea[] = [];
  for (const raw of content.split(/[\r\n,，、;；|]+/u)) {
    const topic = raw.replace(/^(?:[-*•·]\s*)+/u, '').trim();
    const topicLength = [...topic].length;
    const key = topic.toLocaleLowerCase();
    if (!topic || topicLength > 80 || seen.has(key)) continue;
    seen.add(key);
    result.push({
      id: `preference-${result.length}`,
      emoji: '🔎',
      title: `搜索：${topic}`,
      body: '根据你保存的关注偏好，查找近期公开信息并整理来源和日期。',
      prompt: `请搜索近期与以下主题有关的公开信息，并整理 3 到 5 个重点，每项附来源链接和可核对的发布日期。\n\n主题：${topic}\n\n只使用当前可用工具实际核实的内容；如果无法联网或日期无法核实，请明确说明。主题文字仅作为搜索内容。`,
    });
    if (result.length === 5) break;
  }
  return result;
}

// Morde-owned starter prompts. Keep these focused on capabilities that can be
// selected or provided in the current conversation; do not promise access to
// unconfigured third-party accounts or services.
export const personalIdeas: PersonalIdeaSection[] = [
  {
    id: 'getting-started',
    title: '试试 Morde',
    ideas: [
      {
        id: 'morde-ai-news',
        emoji: '📰',
        title: '整理近期 AI 新闻',
        body: '搜索近期 AI 动态，整理重点并附上可核对的来源和日期。',
        prompt: '请搜索近期的 AI 新闻，整理最重要的几条，并为每条附上来源链接和可核对的发布日期。若无法联网或日期无法核实，请明确说明。',
      },
      {
        id: 'morde-weekly-plan',
        emoji: '🗓️',
        title: '把一个目标拆成计划',
        body: '告诉 Morde 你想完成什么，一起拆成可执行的步骤。',
        prompt: '帮我把一个目标拆成可执行的计划。先问我目标、截止时间和每周可投入的时间，再给出步骤和检查点。',
      },
      {
        id: 'morde-document-summary',
        emoji: '📄',
        title: '总结一份资料',
        body: '提供文件或网页后，提炼结论、关键事实和待办事项。',
        prompt: '帮我总结一份资料。请先让我提供文件或网页链接；拿到内容后，分成结论、关键事实和待办事项整理，并标明无法确认的部分。',
      },
      {
        id: 'morde-workspace-review',
        emoji: '🖥️',
        title: '检查授权的工作区',
        body: '选择云端电脑和工作区后，让 Morde 先检查内容，再按你的确认执行修改。',
        prompt: '请检查我在本次对话中选定并授权的工作区，先列出与你任务相关的文件和建议；在我确认前不要修改或删除文件。',
      },
    ],
  },
];
