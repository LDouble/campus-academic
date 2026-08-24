# 独立部署架构

```text
campus-platform API/Worker
        │ mTLS gRPC
        ├──────────────> academic-provider ──> 学校系统
        │                    │
        │                    └── Provider Redis（会话、缓存、限流）
        └──────────────> academic-analytics ──> Analytics MySQL（发布批次）
                             │       │
                             │       ├── Analytics Redis（Asynq）
                             │       └── 外部只读源或 Analytics 托管成绩源
                             └── 通过率/排名等公开聚合能力
```

Provider 与 Analytics 没有平台数据库写权限。外部成绩源账号只允许对 `grade_details` 执行聚合所需的只读查询；Analytics 托管源账号可额外获得 `INSERT`、`UPDATE`，供 Redis Stream 消费者按 `(student_id, grade_id)` 唯一键 UPSERT，且必须用 `observed_at` 防止旧消息覆盖新数据。发布前校验快照，发布后只读不可变批次。后续排名、分位数和更多聚合应在 Analytics 内增加模块与 RPC operation，不把明细成绩传回平台。

平台侧的授权、学生身份归属和 HTTP envelope 仍由 `campus-platform` 负责；Analytics 只信任经过 mTLS 的平台调用，并对手动重算命令执行自己的幂等任务审计。
