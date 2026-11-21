# 01 接单与需求价值

**作用：** 把一句「帮我修一下」变成边界清楚、值不值得做也说得明白的任务。  
**用在哪里：** PC 原会话提交时，由 Morde Go 应用层处理。此模块只接单，不改代码、不扩大权限。

**输入 → 输出：** 用户请求和当前会话 → 一张任务卡：仓库、基线 SHA、做与不做的范围、预期行为、必验项、权限、原会话、验收人。缺影响判断的信息才补问。

- **接单前看价值：** 有没有真实反馈、复现或频率证据？没有就先查证，不为了开 PR 凑需求。
- **交付时看正确性：** 按任务卡验收，不因模型换了做法就改目标。
- **使用后看效果：** 比如重连后用户是否不再误以为任务跑了两次。合并但无使用反馈，标「价值待验证」。

**复用与移植：** 直接复用 [Morde Portal OpenAPI](https://github.com/lizhongxuan/morde/blob/main/server/openapi/portal.yaml) 的会话入口的 `POST /api/conversations/{id}/messages` 及消息关联；同一 `Idempotency-Key` 与 `client_message_id` 对应。用户身份和仓库绑定由服务端解析，不信提示词中的自报权限；`202` 只代表消息受理，界面要等真实任务记录才能显示已接单。外部只借 [GitHub Issue Forms](https://docs.github.com/en/communities/using-templates-to-encourage-useful-issues-and-pull-requests/syntax-for-issue-forms) 的结构化提问思路；现有渠道本来就是对话，不移植 Issue 表单引擎。

**验收：** 用户说「修重连重复」，没有仓库时只补问必要项；已明确的同类规则不重复问；未获写权限，仍最多只读分析，不能偷偷形成可写任务。
