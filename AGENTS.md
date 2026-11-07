# Morde 开发约定

## Agent Runtime 与 DSH 插件

- Morde 云端 AI Agent 统一使用 DSH（DeepSeek Harness）作为唯一的 Agent Runtime。对话、并发任务和子 Agent 可以分别拥有执行上下文，但不另外实现 Agent 循环或并行引入第二套 Agent Runtime。GPT、DeepSeek 等模型继续通过 Morde 模型网关接入。
- 开发或调整 Agent 能力前，先查看当前项目已接入的 DSH 组件和 [DSH 上游插件](https://github.com/deepseek-ai/deepseek-harness)，包括工具、MCP、技能、子 Agent、会话和沙箱等；有符合需求的插件时优先直接复用，通过插件和 provider 机制做必要适配，避免重复开发。没有合适插件、版本不兼容或无法满足现有约束时，说明原因并选择最小可行方案。
- 以 `project/nanoMuse/harness/morde-cloud-runtime/package.json` 和锁文件为当前版本依据。新增或升级插件时检查版本兼容及许可证，按项目依赖管理规则固定版本，并验证现有会话、检查点、任务取消与恢复。
- DSH 插件接入必须遵守 Morde 的用户、会话和任务隔离，以及现有授权、审批、凭据、操作记录和产物管理边界；不得为了直接加载插件绕过这些机制。通过相关测试验证实际功能，插件存在或安装成功不等于功能已接通。
