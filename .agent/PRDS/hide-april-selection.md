# Academic Provider：屏蔽本科生 2026 年 4 月选课记录

**Priority:** High
**Status:** Done
**Type:** Bug
**Created:** 2026-09-11
**Last Updated:** 2026-09-11

## 概述

OUC 本科生选课结果补充接口返回了一条选课时间为 `2026-04-23` 的历史课程记录。该时间段记录不应出现在当前选课结果列表中，需要在 provider 层统一过滤；此前已存在的个人退选过滤规则也必须保持。

## 用户故事

**作为**查看选课结果的本科生
**我希望**列表中不出现 2026 年 4 月的历史选课记录
**以便**当前列表只展示有效范围内的选课结果。

## 实现范围

- 按 `xksj` 解析后的 `selected_at` 判断年月。
- 过滤本科生最终选课结果中年份为 2026、月份为 4 的记录。
- 正常查询和补充查询的结果都应用该过滤规则。
- 保留已有的个人退选过滤规则。
- 缺少选课时间的记录不被误过滤。

## 非范围

- 不修改公开 `course-selections` API 契约。
- 不修改 `provider-ouc.json`。
- 不修改小程序页面。
- 不改变研究生选课结果逻辑。

## 文件修改计划

- `internal/modules/academic/infrastructure/ouc/provider.go`：增加本科生 2026 年 4 月记录过滤。
- `internal/modules/academic/infrastructure/ouc/provider_test.go`：补充年月过滤单测。
- `internal/modules/academic/infrastructure/ouc/provider_integration_test.go`：补充最终结果过滤断言。

## 验收标准

- `2026-04-23` 记录不会返回给前端。
- 2026 年 3 月、5 月及其他月份记录继续返回。
- 缺少 `selected_at` 的记录继续返回。
- 个人退选记录继续被过滤。
- OUC provider 测试、全量 Go 测试和静态检查通过。

**关联 Issue:** https://github.com/LDouble/campus-academic/issues/34

---
