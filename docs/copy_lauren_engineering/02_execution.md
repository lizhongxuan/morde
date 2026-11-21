# 02 隔离执行

**作用：** 让一个 Agent 在不碰主分支、不覆盖用户本地改动的前提下完成一件小事。  
**用在哪里：** 已授权、已持久化的任务交给现有 Morde 运行时之后。

**输入 → 输出：** 仓库、基线 SHA、工作范围、允许工具和预算 → 专属分支、候选提交、修前复现／验收样例、交给 03 的验证记录。不能复现或超出范围时停，不报成功。

1. 从固定基线建立独立分支和工作目录。
2. Bug 先复现，小需求先确定一个可执行的验收样例。
3. 做最小改动；执行允许的检查。不要顺手重构；不要操作主分支或用户的工作目录。

**复用与移植：** 优先复用 Morde 现有 [DSH 路径](https://github.com/lizhongxuan/morde/blob/main/server/internal/worker/cloud_dsh_session_tree.go) 和 [Host Runtime 执行入口](https://github.com/lizhongxuan/morde/blob/main/server/internal/app/runtime_tool_executor.go)，另调用 [Git worktree](https://git-scm.com/docs/git-worktree) 做工作目录隔离，不复制 Git 源码。**worktree 不是安全沙箱**：工具、文件、密钥和网络权限仍由运行环境限制。Agent 不持有推送或开 PR 的凭据，这些动作只走 04 的受控入口。[Noodle（MIT）](https://github.com/poteto/noodle/blob/main/docs/concepts/scheduling.md) 借鉴 worktree 生命周期和后续串行合并，P0 不移植调度器；如以后确实缺清理组件，先验证对应包和边界再决定。

**验收：** 任务改动只出现在专属分支，用户已有修改未动；修前样例能复现，候选提交明确；超出允许范围时，在创建 PR 之前停止。
