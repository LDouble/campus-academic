# OUC Provider 运维边界

OUC 登录、CAS 跳转、门户身份换票、本科与研究生教务路由、Cookie 会话、响应解析、连接复用、缓存和降级策略全部属于本仓库。平台仓库只保存认证后的身份归属，并通过已发布的 mTLS gRPC 契约发起查询。

## 配置归属

- `deploy/provider-ouc.json` 是 OUC 路由配置的唯一受版本控制来源。
- `scripts/render-provider-config.sh` 根据环境和 Provider ID 生成宿主机只读配置。
- 上游代理、Provider Redis、会话密钥、限流和查询缓存只在本仓库部署环境维护。
- Production 禁止 Mock；Review 只有显式选择时才允许 Mock。

Provider ID 与对应配置属于同一个低频初始化单元：在本地部署状态的 `inputs.env`
中设置 `CAMPUS_PROVIDER_ACTIVE_PROVIDER`。当前运行时只实现 `ouc`，Review 额外允许
`mock`；接入新的 Provider 必须先实现并注册运行时适配器，不能只增加配置文件。
云效中的同名变量必须与初始化 bundle 一致。

学校端点、请求参数、解析夹具和故障样本不得复制回平台仓库。平台不得依赖 `ouc` 这个标识决定业务流程；它应把非 Mock Provider ID 当作不透明值。

## 发布验收

1. 校验 Provider 配置及 mTLS、Redis TLS 文件路径。
2. 启动 aTrust Gateway 并确认代理健康。
3. 启动 Provider，等待自身 gRPC healthcheck。
4. 分别验证认证、学期、课表、成绩、考试、选课和课程目录契约。
5. 再发布平台 API/Worker，并检查 RPC 客户端指标。
