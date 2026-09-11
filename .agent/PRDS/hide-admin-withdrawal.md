# Academic Provider：屏蔽本科生管理员退选课程

**Priority:** High
**Status:** Done
**Type:** Bug
**Created:** 2026-09-11
**Last Updated:** 2026-09-11

## 概述

OUC 本科生选课结果补充接口会返回 `tklx=管理员退选` 的历史课程记录。当前业务不需要展示此类记录，因此需要在 provider 层过滤，避免它们进入前端选课结果列表。

## 用户故事

**作为**查看选课结果的本科生
**我希望**列表中不出现管理员退选的课程
**以便**列表只展示需要关注的选课结果。

## 实现范围

- 在 OUC 本科生最终选课结果中过滤 `result_text` 包含“管理员退选”的失败记录。
- 继续应用个人退选和 2026 年 4 月记录过滤规则。
- 保留抽签落选、其他月份、正常已选和待确认记录。
- 研究生流程保持不变。

## 非范围

- 不修改公开 `course-selections` API 契约。
- 不修改 `provider-ouc.json`。
- 不修改小程序页面。
- 不改变选课结果之外的查询接口。

## 文件修改计划

- `internal/modules/academic/infrastructure/ouc/provider.go`：增加管理员退选过滤。
- `internal/modules/academic/infrastructure/ouc/provider_test.go`：补充过滤逻辑单测。
- `internal/modules/academic/infrastructure/ouc/provider_integration_test.go`：补充最终结果过滤断言。

## 验收标准

- 本科生 `result_text=管理员退选` 的记录不进入最终返回列表。
- 抽签落选记录继续返回。
- 个人退选和 2026 年 4 月过滤规则继续生效。
- 研究生流程不触发该过滤。
- OUC provider 测试、全量 Go 测试和静态检查通过。

**关联 Issue:** https://github.com/LDouble/campus-academic/issues/36

---
