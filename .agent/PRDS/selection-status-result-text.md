# Academic Provider：返回选课状态到 ResultText

**Priority:** Medium
**Status:** Done
**Type:** Enhancement
**Created:** 2026-09-11
**Last Updated:** 2026-09-11
**Issue:** [campus-miniapp#226](https://github.com/LDouble/campus-miniapp/issues/226)

## Overview

将 OUC 本科生选课结果中的 `xkzt/选课状态`写入现有 `CourseSelection.ResultText`，让调用方可以直接取得教务系统返回的选课状态。现阶段只提交 Provider 端，小程序端暂不修改。

## Requirements

- 保留现有 `result_text/resultText` 和 `tklx/退课类型`的优先级。
- 当上述字段为空时，从 `xkzt/选课状态`取得 `ResultText`。
- 已选、待确认、落选记录均应保留对应的原始状态文本。
- 不修改 Proto、HTTP API、状态枚举、过滤逻辑和其他 Provider。

## Files to Modify

- `internal/modules/academic/infrastructure/ouc/parser.go` - 补充 `xkzt/选课状态`到 `ResultText`的字段映射。
- `internal/modules/academic/infrastructure/ouc/undergraduate_parser_test.go` - 验证已选、待确认和落选状态的返回值。

## Testing Requirements

- `go test ./internal/modules/academic/infrastructure/ouc/...`
- `go test ./...`
- `make generate-check`
- `make vet`
- `git diff --check`

## Implementation Notes

`selectionToProto` 已经将领域模型的 `ResultText`传输到现有 gRPC 字段，因此无需修改契约或生成代码。客户端后续可直接读取现有 `result_text` 字段。
