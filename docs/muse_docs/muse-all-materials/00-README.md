# Muse 资料包 — 清单说明

打包时间：2026-11-09
来源：本机 Muse 环境里原有的文档，按原样复制，未改写内容。

## 里面有什么

- `docs/` — Muse 产品文档（~/docs 全套，30 个文件左右）：
  muse.md（产品、模型、订阅）、artifacts.md / spaces.md（Space 与工件）、
  browser.md、connectors.md、client-surfaces.md、scheduling-and-watching.md、
  feed.md、ideas.md、goals.md、memory.md、media.md、voice.md、
  privacy-and-credentials.md、data-handling.md、payments-and-purchases.md 等。
- `system_prompt.md` — 本次对话的系统提示词原文。
- `skills/` — Muse 内置技能，共 56 个，每个技能一个目录，核心是 `SKILL.md`：
  app-creation（一句话做 App 的做法）、documents、shopping、flight-booking、
  hotel-booking、groceries、email-management、scheduling、trip-planning、
  music-discovery、health-review、fitness-tracking、financial-planning 等。
  技能是“我做某类任务时按什么流程走”的操作手册。
- `connectors/` — 连接器文档，共 79 个，每个一个目录，核心是 `CONNECTOR.md`：
  gmail、google_calendar、google_docs、google_drive、duffel、spotify、
  instagram、facebook_cli、threads、shopify、stripe、plaid、slack、github、
  notion、todoist、ticketmaster 等。连接器说明“我怎么接某个外部服务、
  能做什么操作、用什么命令”。

总计约 725 个文件。

## 里面没有的东西（诚实说明）

- 没有 Muse 的内部源代码、模型权重、构建器运行时。
- 没有“一句话做 App”的私有脚手架和托管数据库绑定代码——那部分在平台侧，
  不在本机文件里。想在 Codex 上复现，要按前面给你的 Codex 工作包，
  用开源脚手架 + cron + SQLite 自己搭等价做法。
- 没有其他用户的资料，也没有你的私人记忆、聊天记录、密码和密钥。
  `MEMORY.md`、`USER.md` 等是你自己的文件，不在这份包里。

## 怎么用

- 想了解 Muse 是什么、能干什么：先看 `docs/muse.md`。
- 想复现“一句话做 App”：先看 `skills/app-creation/SKILL.md`，
  再看 `docs/artifacts.md` 和 `docs/spaces.md`。
- 想接某个具体服务：去 `connectors/<名字>/CONNECTOR.md` 查。
- 想按任务类型找做法：去 `skills/<任务名>/SKILL.md` 查。
