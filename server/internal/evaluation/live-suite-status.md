# 每周 44 条 live 套件执行状态（P3-4）

套件文件：`testdata/suite-live-v1.json`（`morde-pc-chat-live-v1.0.0`，44 条）。
本文件只记每条**能不能由人工执行**，不记通过/失败。

## 口径

- **通过 0/44**：没有任何一条标为通过。通过只能由真实执行判定，写进
  每周周报，不写进本文件。本文件里的状态只有两种：
  `manual-runnable`（今天能由人工执行）与 `blocked`（有已知缺口挡着，
  连执行都执行不了）。
- **周自动跑 0/44 可跑**：自动跑的条件不具备——无 Inspect AI runner，
  Judge key 未配。自动跑起来之前，每周只能人工抽跑本表里
  `manual-runnable` 的条目；`blocked` 的条目先清缺口再跑。
- **手动可跑的判定**：今天在用户 PC + 现线上服务器上能由人工执行，
  只依赖用例自身点名的前置条件（PC、模型、云电脑、测试账号）。手动
  可跑 ≠ 会通过：020/023 这类缺口测量用例跑起来就是为了如实记失败。
- **更新规则**：状态变化时改本表，并同步改
  `live_suite_test.go` 里锁死的计数（manual-runnable 36 / blocked 8）；
  通过/失败只写周报，不改本表。

## 状态表

| 用例 ID | 状态 | 原因 |
|---|---|---|
| PC-CHAT-001 | manual-runnable | 纯文字问答，无需云电脑；DeepSeek Flash 已验证可用（2026-11-18 15 次调用成功），需人工在 PC 上执行 |
| PC-CHAT-002 | manual-runnable | 同 001，多轮上下文无需额外能力 |
| PC-CHAT-003 | manual-runnable | 同 001，澄清行为无需额外能力 |
| PC-CHAT-004 | manual-runnable | 同 001，粘贴文本摘要无需额外能力 |
| PC-CHAT-005 | blocked | 内置搜索未配置（2026-11-21 部署时在配置里明确关闭）；需先配好搜索能力才能跑 |
| PC-CHAT-006 | manual-runnable | 纯对话内代码生成，不读写本机文件 |
| PC-CHAT-007 | manual-runnable | 测试云电脑 hdqokxzv 在跑（2026-11-21 建好，running）；需人工在 PC 上指定该电脑操作 |
| PC-CHAT-008 | manual-runnable | 需真实并行派两个子 Agent；云电脑与 DSH 在跑，需人工在 PC 上执行并观察进度 |
| PC-CHAT-009 | manual-runnable | 需一个处理中的只读分析任务；人工可控 |
| PC-CHAT-010 | manual-runnable | 需两个可观察的文本任务；人工可控（若环境无法稳定控制任务时长，执行时记阻塞） |
| PC-CHAT-011 | manual-runnable | 需两个同时处理的不同主题任务；人工可控 |
| PC-CHAT-012 | manual-runnable | 关闭/刷新客户端即可复现，无需额外能力 |
| PC-CHAT-013 | manual-runnable | 断网/恢复网络人工可控 |
| PC-CHAT-014 | manual-runnable | 建两个会话即可，无需额外能力 |
| PC-CHAT-015 | blocked | 需构造超上下文窗口（约 25 万 tokens）的多轮对话，手工无法完成；需脚本或自动化构造 |
| PC-CHAT-016 | blocked | 需两个独立测试账号，未确认是否已备好；备好账号后可跑 |
| PC-CHAT-017 | manual-runnable | 审批卡 + 测试云电脑均具备；需人工在 PC 上触发并点批准 |
| PC-CHAT-018 | manual-runnable | 同 017，点驳回并核对云电脑上无文件 |
| PC-CHAT-019 | manual-runnable | 审批卡字段核对，人工逐项记录即可（本就是缺口测量） |
| PC-CHAT-020 | manual-runnable | 只做缺口测量：打开任务详情核对事件顺序，缺失如实记失败，不预设通过 |
| PC-CHAT-021 | manual-runnable | Workspace 交付 + 对话内文件链接，人工核对即可 |
| PC-CHAT-022 | manual-runnable | 指向不存在的云电脑即可复现失败，人工核对失败记录 |
| PC-CHAT-023 | manual-runnable | 只测用量/成本有没有显示位置，人工核对即可（不预设计价口径） |
| PC-CHAT-024 | manual-runnable | 可中断的测试任务 + 断网/关闭页面，人工可控；与 012 联合判定 |
| SPEC-CLF-01 | manual-runnable | 发 1 个汉字即可，无需额外能力 |
| SPEC-CLF-02 | manual-runnable | 发一段超 2 KiB 的长消息即可 |
| SPEC-CLF-03 | blocked | 需在网关/模型层注入一次非法分类输出，无手动注入手段；需先有故障注入入口 |
| SPEC-CLF-04 | manual-runnable | 连发两条消息即可，人工控制发送间隔 |
| SPEC-CLF-05 | manual-runnable | 发一条意图含糊的消息即可 |
| SPEC-ERR-01 | blocked | 需模型侧注入一次 429/超时，无手动注入手段；需先有故障注入入口（或等真实 429 出现时记录） |
| SPEC-ERR-02 | manual-runnable | 指向已失败/不存在的云电脑即可复现 DSH 失败，观察暂停与恢复 |
| SPEC-ERR-03 | manual-runnable | 发一条必然超长的请求即可 |
| SPEC-ERR-04 | manual-runnable | 断网人工可控 |
| SPEC-ERR-05 | blocked | 依赖 P3-6 重复错误沉淀机制，本批后一项才做；当前无记录可查 |
| SPEC-APR-01 | manual-runnable | 触发一次审批、逐项核对卡面即可 |
| SPEC-APR-02 | manual-runnable | 驳回 + 改目标重发，人工可控 |
| SPEC-APR-03 | blocked | 审批超时机制未核实是否实现（超时时长与 fail-closed 行为）；需先确认再跑 |
| SPEC-APR-04 | manual-runnable | 触发一次审批，核对对话流与审批区两处即可 |
| SPEC-COST-01 | manual-runnable | 跑两个任务、核对成本显示位置即可（缺口测量） |
| SPEC-COST-02 | manual-runnable | 用量未知场景下核对成本字段是否写未知；后端常为 unknown，可直接观察 |
| SPEC-COST-03 | blocked | 需先确认预算上限配置入口与超预算停表行为，未核实 |
| SPEC-MRG-01 | manual-runnable | 连发两条同话题消息即可 |
| SPEC-MRG-02 | manual-runnable | 连发两条不同话题消息即可 |
| SPEC-MRG-03 | manual-runnable | 任务处理中追发补充即可 |

合计：manual-runnable 36 / blocked 8 / passed 0。
