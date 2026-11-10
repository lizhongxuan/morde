# DATA-04：个人附件与历史导入实施 TODO

> 开发量：按一个 AI 会话、前置就绪后的连续开发估算，最多一天；只计实现与交接记录。测试编写、运行和验收单独进行，不计入这一天。
> 范围估算不是完成承诺；发现超出一天时把剩余实现按文件/接口继续合并拆开，保持原验收条件。
> 当前状态：DATA-04.01/.02 已完成本地集成与定向验收。附件 schema、DATA-05 额度预留/结算、历史导入关系、路由与清理均已接入；2026-11-02 临时 PostgreSQL 上相关 migration 与 race 集成测试通过。最新本地 make lint 和 internal/api 测试通过；全量 make test 仍被其他包的 Workspace/Hub/worker 用例阻断，详见本地验收记录。
> 直接前置：依赖 DATA-01；额度准入由 DATA-05 衔接；公共 schema/路由由会话 5 负责。
> 修改边界：个人附件会话、分片、提交/读取/回收和显式会话历史导入。
> 写入规则：[五会话执行入口](../并行执行入口.md)；只修改本会话的指定文件与本会话文档。公共合同/路由/迁移变更在本文件交接给会话 5。
> 本地约束：只在本地修改和验证，不发送或上传文件，不向远端部署。

## 本次开发

- [x] **DATA-04.01 实现受限附件上传**：已完成 domain/app/API/Postgres 实现；上传绑定账号与资源库条目，单片上限 4 MiB、校验大小和 SHA-256；额度预留、附件引用、版本/变更回执与结算在事务中衔接，超额提交保留原对象并释放预留。
- [x] **DATA-04.02 实现读取、清理与历史导入**：已完成 owner 授权流读取、取消/过期清理和显式会话历史导入；历史消息为 taskless，不触发 Task。`00093` schema、每分钟有界清理、路由注册、请求体上限及 OpenAPI/client allowlist 已集成。

## 原任务与已有进度

下列编号用于追溯完整业务规则和拆分前证据；同一原编号涉及多个会话时，各会话只完成自身范围。原项全部实现及验收通过后才可视为完成。

| 原编号 | 原任务 | 拆分前状态 |
|---|---|---|
| [F04-T02.07](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 实现个人附件上传和读取 | 未开始，汇总，依赖 F04-T02.07a、F04-T02.07b、F04-T02.07c |
| [F04-T02.07a](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 建立个人附件上传会话 | 未开始，依赖 F04-T01.02 |
| [F04-T02.07b](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 实现有界附件分片上传 | 未开始，依赖 F04-T02.07a |
| [F04-T02.07c](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 实现附件提交、读取与清理 | 未开始，依赖 F04-T02.07b、F04-T02.02 |
| [F04-T02.08](../../融合04-nanoMuse功能及管理后台迁移实施任务todo清单文档.md) | 实现显式会话历史导入 | 未开始，依赖 F04-T02.02、F04-T02.07 |

## 交接

- **本会话文件**：新增 `server/internal/domain/personal/attachment.go` 及测试、`server/internal/app/personal_attachments.go` 及测试、`server/internal/api/personal_attachments.go` 及测试、`server/internal/store/postgres/personal_attachments.go` 及 PostgreSQL 集成测试；历史导入新增 `server/internal/domain/conversation/history_import.go` 及测试、`server/internal/app/personal_history_import.go` 及测试、`server/internal/api/personal_history_import.go` 及测试、`server/internal/store/postgres/personal_history_import.go` 及测试。并修改资源库对象校验、会话消息响应和消息读取以支持已提交附件 ID 与 taskless 导入消息。
- **附件迁移已集成于 `00093_personal_attachments_history_import.sql`**：包含 `personal_attachment_uploads` 与 `personal_attachment_chunks`，主键/唯一键/外键均绑定 `service_instance_id` 和 `owner_id`；上传行保存 `upload_id`、可空 `attachment_id`、资源库目标/修订、大小、摘要、媒体类型、幂等键、状态、quota reservation ID、释放确认、提交回执和过期时间。分片按 upload/sequence 保存大小、SHA-256 和正文，单片不超过 4 MiB；上传有效期 24 小时。
- **额度事务已与 DATA-05 集成**：`CommitPersonalAttachment` 在同一 PostgreSQL 事务中调用 Go helper `commitPersonalAttachmentQuotaReservationInTx`，锁定并核验 owner、upload、字节数，将预留原子结算为已用额度；缺失、不匹配或已释放预留会回滚附件引用。Reserve 按 owner+upload ID 幂等，Release owner-scoped 幂等。预留写入上传行之前进程退出时，由 `CleanupExpiredPersonalDataQuotaReservations` 回收过期孤儿预留；上传清理有界重试。
- **历史导入关系已集成于 `00093`**：`personal_history_import_mappings` 按来源/设备账号/来源会话/来源消息建立 owner-scoped 幂等键；`conversation_message_attachments` 约束已提交附件、owner 和消息归属，并有附件索引。导入消息 taskless，不触发 Task。
- **早期 DDL 合同草案**（实现已落在 `00093_personal_attachments_history_import.sql`；本段只作设计背景，结构、约束和实际行为以迁移及 Store 实现为准）：

```sql
ALTER TABLE conversation_messages
  ADD CONSTRAINT conversation_messages_id_owner_conversation_unique UNIQUE (id, owner_id, conversation_id);

CREATE TABLE personal_attachment_uploads (
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  upload_id text NOT NULL CHECK (upload_id ~ '^pu_[a-f0-9]{32}$'),
  attachment_id text CHECK (attachment_id IS NULL OR attachment_id ~ '^pa_[a-f0-9]{32}$'),
  object_type text NOT NULL CHECK (object_type = 'library_item'),
  object_id text NOT NULL CHECK (octet_length(object_id) BETWEEN 1 AND 128),
  base_revision bigint NOT NULL CHECK (base_revision >= 0),
  size_bytes bigint NOT NULL CHECK (size_bytes BETWEEN 0 AND 9007199250546688),
  sha256 text NOT NULL CHECK (sha256 ~ '^[a-f0-9]{64}$'),
  media_type text NOT NULL CHECK (octet_length(media_type) BETWEEN 1 AND 128),
  request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  idempotency_key text NOT NULL CHECK (octet_length(idempotency_key) BETWEEN 8 AND 128),
  state text NOT NULL CHECK (state IN ('pending','active','committed','cancelled','expired')),
  reservation_id text,
  reservation_released boolean NOT NULL DEFAULT false,
  commit_idempotency_key text,
  result_revision bigint,
  change_sequence bigint,
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (service_instance_id, owner_id, upload_id),
  UNIQUE (service_instance_id, owner_id, idempotency_key),
  UNIQUE (service_instance_id, owner_id, upload_id, attachment_id),
  CHECK ((state = 'committed') = (attachment_id IS NOT NULL AND commit_idempotency_key IS NOT NULL
    AND result_revision > 0 AND change_sequence > 0)),
  CHECK (reservation_id IS NULL OR octet_length(reservation_id) BETWEEN 8 AND 128)
);
CREATE UNIQUE INDEX personal_attachment_uploads_owner_attachment
  ON personal_attachment_uploads(service_instance_id,owner_id,attachment_id) WHERE attachment_id IS NOT NULL;
CREATE INDEX personal_attachment_uploads_expiry
  ON personal_attachment_uploads(service_instance_id,expires_at,upload_id) WHERE state IN ('pending','active');
CREATE INDEX personal_attachment_uploads_release_retry
  ON personal_attachment_uploads(service_instance_id,expires_at,upload_id)
  WHERE state IN ('cancelled','expired') AND reservation_id IS NOT NULL AND NOT reservation_released;

CREATE TABLE personal_attachment_chunks (
  service_instance_id uuid NOT NULL,
  owner_id text NOT NULL,
  upload_id text NOT NULL,
  sequence integer NOT NULL CHECK (sequence >= 0),
  size_bytes bigint NOT NULL CHECK (size_bytes BETWEEN 1 AND 4194304),
  sha256 text NOT NULL CHECK (sha256 ~ '^[a-f0-9]{64}$'),
  body bytea NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (service_instance_id,owner_id,upload_id,sequence),
  FOREIGN KEY (service_instance_id,owner_id,upload_id)
    REFERENCES personal_attachment_uploads(service_instance_id,owner_id,upload_id) ON DELETE CASCADE,
  CHECK (octet_length(body) = size_bytes)
);

CREATE TABLE personal_history_import_mappings (
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  source_id text NOT NULL CHECK (octet_length(source_id) BETWEEN 1 AND 128),
  device_account_id text NOT NULL CHECK (octet_length(device_account_id) BETWEEN 1 AND 128),
  source_conversation_id text NOT NULL CHECK (octet_length(source_conversation_id) BETWEEN 1 AND 128),
  source_message_id text NOT NULL DEFAULT '',
  requested_conversation_id text NOT NULL DEFAULT '',
  canonical_conversation_id text NOT NULL,
  canonical_message_id text,
  content_hash bytea,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (service_instance_id,owner_id,source_id,device_account_id,source_conversation_id,source_message_id),
  FOREIGN KEY (canonical_conversation_id,owner_id) REFERENCES conversations(id,owner_id) ON DELETE CASCADE,
  FOREIGN KEY (canonical_message_id,owner_id,canonical_conversation_id)
    REFERENCES conversation_messages(id,owner_id,conversation_id) ON DELETE CASCADE,
  CHECK ((source_message_id='' AND canonical_message_id IS NULL AND content_hash IS NULL)
      OR (octet_length(source_message_id) BETWEEN 1 AND 128 AND canonical_message_id IS NOT NULL AND octet_length(content_hash)=32)),
  CHECK (requested_conversation_id='' OR requested_conversation_id ~ '^c[a-z0-9]{7}$')
);

CREATE TABLE conversation_message_attachments (
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  conversation_id text NOT NULL,
  message_id text NOT NULL,
  upload_id text NOT NULL,
  attachment_id text NOT NULL CHECK (attachment_id ~ '^pa_[a-f0-9]{32}$'),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (service_instance_id,owner_id,message_id,attachment_id),
  FOREIGN KEY (service_instance_id,owner_id,upload_id,attachment_id)
    REFERENCES personal_attachment_uploads(service_instance_id,owner_id,upload_id,attachment_id) ON DELETE RESTRICT,
  FOREIGN KEY (message_id,owner_id,conversation_id)
    REFERENCES conversation_messages(id,owner_id,conversation_id) ON DELETE CASCADE,
  FOREIGN KEY (conversation_id,owner_id) REFERENCES conversations(id,owner_id) ON DELETE CASCADE
);
CREATE INDEX conversation_message_attachments_owner_attachment
  ON conversation_message_attachments(service_instance_id,owner_id,attachment_id);
```

- **路由、启动及后台清理已集成**：附件和历史导入服务已注入 `api.Deps`/`cmd/portal`，路由挂载到 `/api/personal-data`，历史导入为 `POST /api/personal-data/history/import`。handler 限制创建 JSON 16 KiB、分片 4 MiB、历史导入 JSON 8 MiB；分片要求 `Content-Length` 与 `X-Chunk-SHA256`。普通 API body cap 不变；OpenAPI、client credential allowlist 已更新；后台每分钟按最多 1000 条清理过期上传和孤儿 quota reservation。
- **初始本地验收记录（已被下方主工作区联调取代）**：DATA-04 单独实现时曾在 clean copy 和临时 schema stub 上通过附件/历史导入测试；当时 DATA-05/会话 5 迁移仍待接入。当前完成证据以 2026-11-02 主工作区联调记录为准。
- 对应验证在 [独立验收清单](../验收资料/验收入口.md) 执行并记录，不计入本开发文档的一天预算。
- `server/AGENTS.md` 要求接口、实现和测试在同一变更交付；可先完成代码并标“待验证”，合入/提交前再按独立验收清单补齐测试与所需检查。
- 2026-11-02 主工作区联调：本地临时 PostgreSQL 16 执行 `go test -race -count=1 ./internal/store/postgres -run '^TestPostgres(Personal|ScheduleObservation)'` 通过，覆盖当前 `00093`–`00097` 迁移、附件上传/提交/额度结算、降额时取消并释放预留、历史导入附件关联和 Schedule 用量；`go test -race -count=1 ./internal/domain/personal ./internal/app` 与 quota API 定向 race 测试、`go build ./cmd/portal ./internal/store/postgres`、`go vet ./internal/api ./internal/app ./internal/domain/personal ./internal/store/postgres`、`make generate` 通过。Portal `npm run typecheck` 通过，4 个相关 Vitest 文件 40/40 通过。本地 MSW 管理端已实际保存个人额度并显示成功通知与版本 v2；未连接测试服务器。
- 历史全量检查记录：当时 `go test -race ./internal/api` 受范围外管理统计断言影响，`make lint` 留有 record/admin 侧诊断；后续重验已确认当前 `make lint` 和 `internal/api` 普通测试通过。全仓当前门槛见 DATA-01 至 DATA-07 验收记录。
