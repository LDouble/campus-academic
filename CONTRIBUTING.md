# 贡献指南

请围绕单一能力提交小范围变更，并在 PR 中说明是否影响 gRPC 契约、迁移、配置、数据脱敏或部署拓扑。

- 不提交真实账号、密码、成绩、Cookie、证书私钥或生产 DSN。
- Provider 不得持久化学生密码；Analytics 不得返回或存储学生级投影。
- 所有数据库查询使用 `WithContext(ctx)`；统计表只通过版本化迁移变更。
- 协议变更必须同时更新客户端映射、服务端映射和兼容性测试。
- 提交前运行 `make generate-check make test make vet` 以及 `git diff --check`。
