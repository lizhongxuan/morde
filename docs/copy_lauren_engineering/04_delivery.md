# 04 PR 与原会话交付

**作用：** 给用户一份能审的 PR，而不是一串日志或第二个对话。  
**用在哪里：** 03 给出结论后，由 Morde 服务端向授权的目标仓库交付，再通过原会话显示。

**输入 → 输出：** 原会话 ID、仓库/任务分支、候选 SHA、验收结果、当前授权 → PR 编号和链接、原会话结果消息。消息只给结论、证据和未验证项；人决定合不合并。

- 有授权时才开 PR。若必需 CI 只在 PR 中运行，可以先开**一份草稿**触发；通过后把同一份草稿改待审，不开第二份。
- 创建响应丢失时，把状态交给 05 对账；先查同仓库、目标分支、任务分支和任务标识，再核对当前 head SHA，不盲重试。
- PC/iOS 读取同一原会话。发送前重新检查会话和产物访问权限；不另发到其他地方。

**复用与移植：** 直接复用 [GitHub Pull Requests REST API](https://docs.github.com/en/rest/pulls/pulls) 和现有 Morde 会话投递路径。如采用 GitHub App，用[短期、限仓库的安装令牌](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-an-installation-access-token-for-a-github-app)，由服务端执行；把推送与开 PR 的权限分开限制。自己持久化任务与 PR 的关联，**不要假定 GitHub 创建 PR 的接口替你保证幂等**。不复制第三方开 PR 工具；人审和 GitHub 分支保护仍独立于 Morde 的就绪标记。

**验收：** 制造一次「GitHub 已成功，Morde 没收到应答」：最多一个 PR、一条最终结果；无法确定时显示待对账；任何路径都不会自动合并或部署。
