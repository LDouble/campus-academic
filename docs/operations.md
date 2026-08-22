# 运维说明

1. 为 Provider 和 Analytics 分配独立 Redis 实例或至少独立 DB/ACL。
2. 为 Analytics 创建独立写库用户；成绩源使用只读账号，启动 preflight 会拒绝带写权限或 `WITH GRANT OPTION` 的账号。
3. 先执行 `go run ./cmd/academicctl migrate up`，再启动 Analytics；切换前完成旧表数据校验和一轮新旧读比对。
4. 平台 API 的 `CAMPUS_ACADEMIC_ANALYTICS_TARGET` 指向 Analytics gRPC 地址，Provider target 指向 Provider gRPC 地址；两条连接使用独立客户端证书。
5. 迁移期间保留原平台已 promote 的历史迁移文件，待新库完成切换和审计后再按数据库生命周期单独清理旧表。
