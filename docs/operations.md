# 运维说明

1. 为 Provider 和 Analytics 分配独立 Redis 实例或至少独立 DB/ACL。
2. 为 Analytics 创建独立写库用户；成绩源使用只读账号，启动 preflight 会拒绝带写权限或 `WITH GRANT OPTION` 的账号。
3. 先执行 `go run ./cmd/academicctl migrate up`，再启动 Analytics；切换前完成旧表数据校验和一轮新旧读比对。
4. 平台 API 的 `CAMPUS_ACADEMIC_ANALYTICS_TARGET` 指向 Analytics gRPC 地址，Provider target 指向 Provider gRPC 地址；两条连接使用独立客户端证书。
5. 迁移期间保留原平台已 promote 的历史迁移文件，待新库完成切换和审计后再按数据库生命周期单独清理旧表。

## Provider 自动化发布

Provider 与 aTrust 分开管理。aTrust 的状态卷不会随 Provider 镜像发布而删除，
Provider 发布器只对依赖执行幂等检查：

1. `campus-atrust-clients` 不存在时创建为内部网络，已存在时核对网络属性后跳过。
2. `campus-atrust-egress` 不存在时创建为出口网络，已存在时跳过。
3. 共享客户端网络内已有健康的 `atrust-gateway` 时直接复用；容器停止时启动，
   不健康时重启，完全不存在时才通过 `deploy/atrust.compose.yaml` 创建。
4. aTrust 健康后才更新 Provider，并等待 Provider 自身健康检查成功。

自动创建范围仅包含当前 ECS 上的 aTrust 网关及其 Docker 网络。Provider 的共享
Redis 属于跨 ECS 的有状态基础设施，发布器只通过启动与健康检查验证连接，不会
自动创建一个本机 Redis，避免扩容时误生成彼此隔离的数据分片。

首次部署先复制环境变量模板，并把配置、证书和 Secret 放到宿主机受控目录：

```bash
cp deploy/provider.env.example deploy/provider.production.env
chmod 600 deploy/provider.production.env
make production PROVIDER_ENV_FILE="$PWD/deploy/provider.production.env"
```

Review 使用同一发布器：

```bash
cp deploy/provider.env.example deploy/provider.review.env
make review PROVIDER_ENV_FILE="$PWD/deploy/provider.review.env"
```

生产门禁包括：

- `bootstrap.yaml` 的 `environment` 必须是 `production`、Provider 必须启用 mTLS；
- `provider-config.yaml` 的 `active_provider` 必须为 `ouc`，生产禁止 Mock；
- OUC HTTP 代理必须固定为 `http://atrust-gateway:8888`；
- Provider 健康检查通过 `127.0.0.1:9090` 发起 mTLS gRPC，请在
  `CAMPUS_ACADEMIC_RPC_TLS_HOST_DIR` 提供 CA、健康检查客户端证书和服务端证书；
- Provider Redis TLS 文件通过 `CAMPUS_ACADEMIC_PROVIDER_REDIS_TLS_HOST_DIR`
  只读挂载，具体文件名仍由 `bootstrap.yaml` 控制；
- Academic 与 aTrust 镜像必须使用 `@sha256:` 不可变摘要；
- aTrust 用户名、密码文件必须是非空的宿主机绝对路径；
- 任一网络属性、aTrust 健康或 Provider 健康检查不符合预期都会中止发布。

脚本不会删除已有网关、状态卷或网络。回滚 Provider 镜像时仍执行相同命令，只需
将 `CAMPUS_ACADEMIC_IMAGE` 改为上一版本摘要；健康的 aTrust 会被直接复用。

可用以下命令验证发布器的幂等与门禁逻辑，不需要真实 Docker：

```bash
make test-deploy
```
