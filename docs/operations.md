# 运维说明

1. 为 Provider 和 Analytics 分配独立 Redis 实例或至少独立 DB/ACL。
2. 为 Analytics 创建独立写库用户；外部成绩源使用只读账号。托管成绩源可显式启用受限写模式，启动 preflight 仅接受精确的 `SELECT`、`INSERT`、`UPDATE`，并拒绝 `DELETE`、DDL、`WITH GRANT OPTION` 等越权授权。
3. 先执行 `go run ./cmd/academicctl migrate up`，再启动 Analytics；切换前完成旧表数据校验和一轮新旧读比对。
4. 平台 API 的 `CAMPUS_ACADEMIC_ANALYTICS_TARGET` 指向 Analytics gRPC 地址，Provider target 指向 Provider gRPC 地址；两条连接使用独立客户端证书。
5. 迁移期间保留原平台已 promote 的历史迁移文件，待新库完成切换和审计后再按数据库生命周期单独清理旧表。

## Provider 自动化发布

Provider 与 aTrust 分仓管理。aTrust 的镜像、Compose、状态卷和生命周期脚本由
独立 `campus-atrust-gateway` 仓库负责；本仓库只调用它的稳定发布入口：

1. 在 ECS 上检出 `campus-atrust-gateway`，并通过绝对路径
   `CAMPUS_ATRUST_GATEWAY_HOME` 指向仓库目录。
2. Provider 发布器以同一环境文件调用
   `$CAMPUS_ATRUST_GATEWAY_HOME/scripts/ensure-gateway.sh`。
3. 独立发布器负责创建或核对网络、复用健康网关、启动停止的网关、重启不健康网关，
   仅在完全不存在时创建网关。
4. aTrust 健康后本仓库才更新 Provider，并等待 Provider 自身健康检查成功。

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

### Compose 角色隔离与单机 Review

`COMPOSE_PROJECT_NAME` 是角色级变量：Provider 必须为
`campus-academic-<environment>-provider`，Analytics 必须为
`campus-academic-<environment>-analytics`；需要同角色多节点时只能追加节点后缀。
发布器会拒绝角色或环境不匹配的名称，避免两个 Compose 文件因位于同一目录而误用同一
默认 project、网络或 volume。

三个角色暂时部署在同一台 Review ECS 时，云效应分别调用两个发布器，并在各自环境文件
或变量组中设置：

```dotenv
# Provider 角色
COMPOSE_PROJECT_NAME=campus-academic-review-provider
CAMPUS_ACADEMIC_PROVIDER_PUBLISHED_PORT=19090
CAMPUS_ATRUST_CLIENT_NETWORK=campus-review-atrust-clients

# Analytics 角色
COMPOSE_PROJECT_NAME=campus-academic-review-analytics
CAMPUS_ACADEMIC_ANALYTICS_PUBLISHED_PORT=19091
```

容器内 gRPC 端口仍为 `9090` 和 `9091`，bootstrap 内的本机 mTLS healthcheck target
也不变。平台 API 必须经该 ECS 宿主机可达地址访问 `19090`、`19091`，不能使用跨 project
的 Docker 服务名。Provider 与 aTrust 只共享显式传入、带环境前缀的
`CAMPUS_ATRUST_CLIENT_NETWORK`；Analytics 不加入该网络。

云效运行时，低频配置文件可以指向本地控制机提前下发的角色 `current/runtime.env`；镜像
变量不得写入该文件，而应作为本次流水线参数注入：Provider 使用
`CAMPUS_ACADEMIC_PROVIDER_IMAGE` 与 `CAMPUS_ATRUST_IMAGE`，Analytics 使用
`CAMPUS_ACADEMIC_ANALYTICS_IMAGE`。云效保留 digest、release ID 和发布日志；切换主机时
复用初始化 bundle，但重新从云效选择需要发布的镜像。

生产门禁包括：

- `bootstrap.yaml` 的 `environment` 必须是 `production`、Provider 必须启用 mTLS；
- `provider-config.yaml` 的 `active_provider` 必须与初始化 bundle 及云效变量一致；当前 Production 仅支持 `ouc`，Review 额外支持 `mock`；
- OUC HTTP 代理必须固定为 `http://atrust-gateway:8888`；
- Provider 健康检查通过 `127.0.0.1:9090` 发起 mTLS gRPC，请在
  `CAMPUS_ACADEMIC_RPC_TLS_HOST_DIR` 提供 CA、健康检查客户端证书和服务端证书；
- Provider Redis TLS 文件通过 `CAMPUS_ACADEMIC_PROVIDER_REDIS_TLS_HOST_DIR`
  只读挂载，具体文件名仍由 `bootstrap.yaml` 控制；
- Provider 镜像必须使用 `@sha256:` 不可变摘要；Analytics 使用自己的镜像与发布周期；aTrust 镜像及凭据门禁由独立仓库执行；
- `CAMPUS_ATRUST_GATEWAY_HOME` 必须是宿主机绝对路径，其中的发布脚本必须可执行；
- 任一网络属性、aTrust 健康或 Provider 健康检查不符合预期都会中止发布。

脚本不会删除已有网关、状态卷或网络。回滚 Provider 镜像时仍执行相同命令，只需
将 `CAMPUS_ACADEMIC_PROVIDER_IMAGE` 改为上一版本摘要；健康的 aTrust 会被直接复用。

## 独立镜像

仓库发布两个彼此独立的制品，不能用同一个镜像变量互相替代：

```bash
docker build -f Dockerfile.provider -t campus-academic-provider:local .
docker build -f Dockerfile.analytics -t campus-academic-analytics:local .
```

ACR Tag 自动构建若只能读取仓库根目录 `Dockerfile`，则直接使用根目录复合镜像。该镜像
同时包含 `/app/academic-provider`、`/app/academic-analytics` 和 `/app/academicctl`；部署
Compose 必须显式指定对应命令。Provider 与 Analytics 可以写入两个 ACR 仓库，但相同
Git Tag 对应的二进制内容和源码版本一致。

独立 Prometheus 通过 VPC 拉取应用指标。Provider 主机默认发布
`127.0.0.1:9300`，Analytics 主机默认发布 `127.0.0.1:9301`；Production 必须把对应
`*_METRICS_BIND_ADDRESS` 设置为主机 VPC IP，并在安全组中只允许 Observability 主机访问，
禁止将指标端口绑定公网地址或对 `0.0.0.0/0` 放行。

使用专用 Dockerfile 时，Provider 镜像只包含 `academic-provider`，Analytics 镜像包含
`academic-analytics`、数据库迁移命令 `academicctl` 和 `migrations/`。使用根目录复合
Dockerfile 时，两侧 ACR 镜像包含相同的三个二进制和迁移文件；无论采用哪种构建方式，
`analytics-migrate` 与 `academic-analytics` 必须使用完全相同的 Analytics 镜像摘要。

本仓库用以下命令验证与独立 aTrust 发布器的调用契约，不需要真实 Docker：

```bash
make test-deploy
```

aTrust 自身的幂等生命周期、网络和生产镜像门禁测试在
`campus-atrust-gateway` 仓库执行 `make test`。

## Analytics 自动化发布

Analytics 部署到独立 ECS 时使用 `scripts/deploy-analytics.sh`，它只会启动
`analytics-mysql`、`analytics-redis`、`analytics-migrate` 和
`academic-analytics`，不会启动 Provider、aTrust 或 Provider Redis。首次部署前复制
`deploy/analytics.env.example` 到受控的宿主机路径，并将 bootstrap、两组 TLS 文件和
Redis 配置放在宿主机绝对路径中：

```bash
make analytics-production ANALYTICS_ENV_FILE=/etc/campus-academic/production/analytics.env
make analytics-review ANALYTICS_ENV_FILE=/etc/campus-academic/review/analytics.env
```

Review 和 Production 均应使用由镜像 tag 解析得到的
`CAMPUS_ACADEMIC_ANALYTICS_IMAGE=...@sha256:...`；Production 会拒绝非 digest。

Analytics 默认每天按 bootstrap 中的 `analytics.schedule_hour` 聚合一次。普通故障默认每
15 分钟重试，可通过 `CAMPUS_ACADEMIC_ANALYTICS_RETRY_DELAY`（例如 `1h`）调整；成绩源
为空属于尚无可发布数据，会记录跳过并等待下一次每日调度，不进行高频重试。
发布器校验 bootstrap 环境、Analytics mTLS、Analytics Redis TLS 挂载，先等待 MySQL/
Redis 健康，再用相同的 Analytics 镜像执行迁移并启动服务。

发布器不执行数据库备份、删除卷或删除容器。云效在启动迁移前必须通过外部备份门禁
（例如托管 MySQL 备份成功或运维快照成功）；备份失败时不得运行此发布入口。

`CAMPUS_ACADEMIC_ANALYTICS_REDIS_CONFIG_HOST_FILE` 是 Redis 服务端 TLS 配置；它必须
关闭明文 Redis 端口、监听 TLS 端口，并引用 `/run/secrets/analytics-redis` 下的证书。
若 Redis 配置启用客户端证书校验，环境文件中的客户端证书/私钥名称必须与 bootstrap
中的 Analytics Redis 配置一致。

最小 Redis TLS 配置如下；发布器会拒绝明文端口、错误 TLS 端口、越过 Secret 根目录
的路径，以及缺失或为空的证书文件：

```text
port 0
tls-port 6379
tls-cert-file /run/secrets/analytics-redis/server.crt
tls-key-file /run/secrets/analytics-redis/server.key
tls-ca-cert-file /run/secrets/analytics-redis/ca.crt
```

`academic-analytics` 的 Compose healthcheck 调用镜像内置的
`/app/academic-analytics healthcheck`，使用 bootstrap 中的 `127.0.0.1:9091` 与
Analytics mTLS 客户端材料完成真实 gRPC 健康检查。发布器只有在该检查返回 healthy 后
才报告完成。

## OUC 页面解析失败留样

Provider 默认将所有页面或 JSON 解析失败的完整学校响应保存到容器
`/var/lib/campus-academic/diagnostics`。Production 使用
`CAMPUS_ACADEMIC_DIAGNOSTIC_HTML_HOST_DIR` 挂载宿主机目录；该目录必须仅对
Provider 运行账户开放，禁止被 Nginx、HTTP 服务、日志采集或备份公开读取。

每次新增留样会清理修改时间超过 48 小时的旧样本。文件名不含学号或请求参数，模式为
`0600`；日志只给出样本 ID、SHA-256、长度、host/path 和失败阶段，绝不输出原始 HTML、
Cookie 或凭据。排查完成后可在该受限目录读取对应样本并复现解析器，保留期内不得外传。
