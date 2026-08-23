# campus-academic

`campus-academic` 是教务能力的独立服务仓库，面向社区协作和独立部署。当前包含：

- `academic-provider`：负责 OUC 登录、课表、成绩、考试、选课和课程目录 Provider；会话、查询缓存、限流和 Provider 配置均由本服务拥有。
- `academic-analytics`：负责通过率聚合、不可变发布批次、课程/教师趋势、定时任务和手动重算；只向成绩源数据库执行只读聚合，不返回学生级数据。
- `proto/academic/*/v1`：Provider 与 Analytics 的 gRPC 版本化契约，生成代码位于 `pkg/`。

平台 API 仓库只保留 HTTP、身份与权限，并通过 mTLS gRPC 调用本仓服务。Provider 与 Analytics 使用不同的 Redis；Analytics 写入自己的 MySQL，成绩源通过只读 DSN 接入。

仓库构建两个独立镜像：`Dockerfile.provider` 只打包 Provider，
`Dockerfile.analytics` 打包 Analytics 及其数据库迁移工具。两份制品可独立发布、扩容和回滚，
但继续共享本仓库中的版本化 gRPC 契约。

多实例部署时，所有 Provider 副本必须共享同一个 Provider Redis，所有
Analytics 副本必须共享另一个 Analytics Redis；两类服务之间禁止共用 Redis。
生产组网、TLS 参数和迁移步骤见
[`docs/multi-instance-deployment.md`](docs/multi-instance-deployment.md)。
Provider 的 aTrust 依赖和幂等发布方式见
[`docs/operations.md`](docs/operations.md#provider-自动化发布)。

## 本地启动

```bash
cp bootstrap.yaml.example bootstrap.yaml
cp provider-config.yaml.example provider-config.yaml
go mod download
go test ./...
```

补齐 Compose 原有的数据库、成绩源和密钥环境变量后，使用本地 Redis 时必须
显式启用 `local` profile：

```bash
docker compose -f deploy/compose.yaml --profile local up -d
```

未启用 `local` profile 时，Compose 不会启动本地 Redis，Provider 与 Analytics
分别使用 `CAMPUS_ACADEMIC_PROVIDER_REDIS_*` 和
`CAMPUS_ACADEMIC_ANALYTICS_REDIS_*` 指向的外部服务。

启动 Provider：

```bash
CAMPUS_ACADEMIC_PROVIDER_KEY=$(openssl rand -base64 32) \
CAMPUS_ACADEMIC_QUERY_KEY=$(openssl rand -base64 32) \
go run ./cmd/academic-provider
```

启动 Analytics 前，需要设置 `CAMPUS_ACADEMIC_ANALYTICS_DSN` 和
`CAMPUS_ACADEMIC_ANALYTICS_SOURCE_DSN`，并先执行本仓库迁移：

```bash
CAMPUS_ACADEMIC_MIGRATION_URL='mysql://user:password@tcp(127.0.0.1:3306)/campus_academic?multiStatements=true' \
go run ./cmd/academicctl migrate up
CAMPUS_ACADEMIC_ANALYTICS_DSN='user:password@tcp(127.0.0.1:3306)/campus_academic?parseTime=true' \
CAMPUS_ACADEMIC_ANALYTICS_SOURCE_DSN='readonly:password@tcp(platform-mysql:3306)/campus?parseTime=true' \
go run ./cmd/academic-analytics
```

生产/Review 必须关闭明文 gRPC，并为 Provider 与 Analytics 分别配置 TLS 根目录、服务端证书和客户端证书。服务启动时会校验 Analytics 成绩源表及只读账号权限。

## 质量门槛

```bash
make generate-check
make test
make test-race
make vet
git diff --check
```

协议变更必须先修改 `proto/`，再运行 `buf lint`、`buf generate` 和 breaking 检查。聚合 SQL、迁移和发布规则属于 Analytics 的服务边界；平台仓库不得复制这些实现。
