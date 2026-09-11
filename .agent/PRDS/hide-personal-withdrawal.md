# Academic Provider：屏蔽本科生个人退选课程

**Priority:** High
**Status:** Done
**Type:** Bug
**Created:** 2026-09-11
**Last Updated:** 2026-09-11

## 概述

OUC 本科生选课结果补充接口会返回“抽签落选”“个人退选”“管理员退选”等历史记录。当前业务只需要向前端展示抽签落选和管理员退选，学生主动退选的记录不应出现在选课结果列表中。

## 用户故事

**作为**查看选课结果的本科生
**我希望**列表中不出现自己主动退选的课程
**以便**列表只保留需要关注的落选或管理退选结果。

## 实现范围

- 在 OUC 本科生 `ListCourseSelections` 流程中过滤 `result_text` 为“个人退选”的记录。
- 保留“抽签落选”“管理员退选”及其他正常、待确认记录。
- 过滤发生在 provider 层，现有前端和公开 API 无需修改。
- 研究生选课结果逻辑保持不变。

## 非范围

- 不修改 `provider-ouc.json`。
- 不修改公开 `course-selections` API 契约。
- 不修改小程序页面。
- 不过滤研究生或其他学业查询中的同名文本。

## 文件修改计划

- `internal/modules/academic/infrastructure/ouc/provider.go`：过滤本科生个人退选记录。
- `internal/modules/academic/infrastructure/ouc/provider_test.go`：补充过滤逻辑单测。
- `internal/modules/academic/infrastructure/ouc/provider_integration_test.go`：补充端到端过滤断言。

## 验收标准

- 本科生 `result_text=个人退选` 的记录不进入最终返回列表。
- 抽签落选、管理员退选和正常已选记录继续返回。
- 研究生流程不触发该过滤。
- 补充查询失败时，原有 best-effort 行为保持不变。
- OUC provider 测试、全量 Go 测试和静态检查通过。

**关联 Issue:** https://github.com/LDouble/campus-academic/issues/32

---
