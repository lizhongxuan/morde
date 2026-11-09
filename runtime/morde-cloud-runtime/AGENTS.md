# Morde 云端 Runtime 开发约定

- Morde 云端 Agent 统一使用 DSH 作为唯一 Runtime；模型继续通过 Morde 的模型网关接入。
- 开发工具、技能、子 Agent、会话或其他能力前，先检查当前依赖和 [DSH 上游插件](https://github.com/deepseek-ai/deepseek-harness)，优先直接复用可用插件并通过 provider 适配；没有合适插件或不兼容时说明原因，再选择最小可行方案。
- 新增或升级插件时固定兼容版本、核验许可证，并验证任务隔离、授权、操作记录、取消与恢复；不得绕过 Morde 的工具代理和业务权限。插件安装成功不代表功能已经接通。
