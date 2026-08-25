# Campus Academic 多实例 Redis 部署

## 目标拓扑

多台 Provider 共享 Provider 专属 Redis，多台 Analytics 共享 Analytics 专属
Redis。两个 Redis 必须使用不同实例或至少不同的独立账号与逻辑边界，禁止接入
平台 API 的主 Redis。

```text
Provider-1 ─┐
Provider-2 ─┼── provider-redis.internal
Provider-3 ─┘

Analytics-1 ─┐
Analytics-2 ─┼── analytics-redis.internal
Analytics-3 ─┘
```

共享 Provider Redis 后，加密会话、查询缓存、分布式租约、全局限流、熔断状态
和会话撤销才能在所有 Provider 副本间保持一致。所有 Provider 副本还必须使用
完全相同的 `CAMPUS_ACADEMIC_PROVIDER_KEY` 和
`CAMPUS_ACADEMIC_QUERY_KEY`，密钥应由部署平台的 Secret 管理，不得写入仓库或
镜像。

## Provider Redis

所有 Provider ECS 使用同一组配置：

```dotenv
CAMPUS_ACADEMIC_PROVIDER_REDIS_ADDRESS=provider-redis.internal:6380
CAMPUS_ACADEMIC_PROVIDER_REDIS_USERNAME=provider
CAMPUS_ACADEMIC_PROVIDER_REDIS_PASSWORD=由部署平台注入
CAMPUS_ACADEMIC_PROVIDER_REDIS_DB=0
CAMPUS_ACADEMIC_PROVIDER_REDIS_TLS=true
CAMPUS_ACADEMIC_PROVIDER_REDIS_TLS_FILES_ROOT=/run/secrets/provider-redis
CAMPUS_ACADEMIC_PROVIDER_REDIS_CA_FILE=ca.pem
CAMPUS_ACADEMIC_PROVIDER_REDIS_CLIENT_CERT_FILE=
CAMPUS_ACADEMIC_PROVIDER_REDIS_CLIENT_KEY_FILE=
CAMPUS_ACADEMIC_PROVIDER_REDIS_SERVER_NAME=provider-redis.internal
```

Redis 服务只要求服务端 TLS 时，客户端证书和私钥留空；启用 Redis mTLS 时，
两个字段必须同时配置。TLS 根目录必须是容器内的绝对路径，文件名相对于该根
目录解析。

## Analytics Redis

所有 Analytics ECS 使用另一组独立配置：

```dotenv
CAMPUS_ACADEMIC_ANALYTICS_REDIS_ADDRESS=analytics-redis.internal:6380
CAMPUS_ACADEMIC_ANALYTICS_REDIS_USERNAME=analytics
CAMPUS_ACADEMIC_ANALYTICS_REDIS_PASSWORD=由部署平台注入
CAMPUS_ACADEMIC_ANALYTICS_REDIS_DB=0
CAMPUS_ACADEMIC_ANALYTICS_REDIS_TLS=true
CAMPUS_ACADEMIC_ANALYTICS_REDIS_TLS_FILES_ROOT=/run/secrets/analytics-redis
CAMPUS_ACADEMIC_ANALYTICS_REDIS_CA_FILE=ca.pem
CAMPUS_ACADEMIC_ANALYTICS_REDIS_CLIENT_CERT_FILE=
CAMPUS_ACADEMIC_ANALYTICS_REDIS_CLIENT_KEY_FILE=
CAMPUS_ACADEMIC_ANALYTICS_REDIS_SERVER_NAME=analytics-redis.internal
```

普通 Analytics Redis 客户端、Asynq 任务生产者和 Worker 使用同一套 ACL、DB
与 TLS 配置，不能只给其中一个链路启用 TLS。

## Compose 使用方式

`deploy/compose.yaml` 中的本地 Redis 已放入 `local` profile。开发环境使用：

```bash
docker compose -f deploy/compose.yaml -f deploy/compose.local.yaml --profile local up -d
```

连接外部 Redis 时不要启用该 profile；在原有数据库、成绩源和密钥配置之外，
显式注入两组 Redis 环境变量：

```bash
docker compose -f deploy/compose.yaml up -d academic-provider academic-analytics
```

使用 TLS 时，部署清单还必须将对应证书目录只读挂载到
`*_REDIS_TLS_FILES_ROOT` 指定的容器路径。生产和 Review 环境若关闭 Redis TLS，
服务会在启动配置校验阶段拒绝运行。

## 扩容与轮换

1. 在新 ECS 注入与现有副本相同的 Provider 密钥和 Provider Redis 配置。
2. 启动新 Provider，确认 Redis Ping、gRPC 健康检查和指标端点正常。
3. 将新实例加入 Nginx/NLB 后端组。
4. 等待流量稳定后摘除旧实例。
5. 密钥轮换不能逐台直接替换；当前缓存密文与摘要依赖相同密钥。应先设计双密钥
   读取窗口或清空会话与查询缓存，再统一切换。

Provider Redis 不可用时 Provider 启动会失败；运行期间 Redis 错误按现有查询降级
策略处理。Analytics Redis 不可用时 Analytics 启动失败，避免任务生产者和 Worker
使用不同队列状态。
