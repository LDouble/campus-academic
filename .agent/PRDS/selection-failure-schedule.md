# Academic Provider：展示本科生选课失败原因

**Priority:** High
**Status:** Done
**Type:** Bug
**Created:** 2026-09-11
**Last Updated:** 2026-09-11

## 概述

本科生选课结果补充查询已经将“抽签落选”“个人退选”“管理员退选”解析为 `failed`，但部分前端只直接展示 `schedule` 字段，无法看到失败原因。需要在 provider 解析阶段把失败原因追加到 `schedule`，同时保留原始上课时间。

## 用户故事

**作为**查看本科生选课结果的用户
**我希望**在课程记录的时间信息位置看到落选或退选原因
**以便**无需依赖额外字段也能理解课程未选中的原因。

## 实现范围

- 仅调整 OUC 本科生选课结果解析。
- 当记录为失败状态且存在结果文本时，将结果文本追加到 `schedule`。
- 原始 `sksj` 非空时使用 `原始时间（失败原因）` 格式。
- 原始 `sksj` 为空时，`schedule` 直接使用失败原因。
- 正常已选、待确认记录保持现状。
- 保留现有 `result_text` 字段，避免影响已有调用方。

## 非范围

- 不修改公开 `course-selections` API 契约。
- 不修改 `provider-ouc.json`。
- 不修改小程序页面或其他仓库。
- 不改变研究生选课结果解析。

## 文件修改计划

- `internal/modules/academic/infrastructure/ouc/parser.go`：生成带失败原因的 schedule。
- `internal/modules/academic/infrastructure/ouc/undergraduate_parser_test.go`：补充失败记录 schedule 断言。

## 验收标准

- “抽签落选”“个人退选”“管理员退选”都能出现在 `schedule`。
- 原始 `sksj` 存在时不丢失。
- 原始 `sksj` 为空时仍能展示失败原因。
- 非失败记录的 `schedule` 不变。
- OUC provider 测试、全量 Go 测试和静态检查通过。

## 风险与注意事项

- 当前小程序也可能展示 `result_text`，保留该字段会维持兼容，但需要避免改变正常记录的展示语义。
- 失败记录可能共享上游课程 ID，不能借此调整记录合并策略。

**关联 Issue:** https://github.com/LDouble/campus-academic/issues/30

---
