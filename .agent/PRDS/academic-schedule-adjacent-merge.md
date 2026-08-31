# Academic：本科课表相邻节次合并

**Priority:** High  
**Status:** Ready for Review  
**Type:** Enhancement  
**Created:** 2026-08-31  
**Last Updated:** 2026-08-31

## Overview

本科生课表解析目前只会把节次完全相同、但周次不同的记录合并；同一课程在相同周次下被上游拆成相邻节次时，仍会返回多条课程记录。本任务补齐相邻节次的合并规则，同时保持不同周次、不同课程和非相邻节次的边界。

## User Story

**作为** 查询本科课表的学生  
**我希望** 同一课程在相同周次的相邻节次被合并为连续节次区间  
**从而** 课表显示与实际连续上课时段一致，避免出现 5–6 节和 7 节两个重复课程块。

## Requirements

1. 解析本科 HTML 课表时，先按现有课程身份合并周次，周次去重并排序。
2. 对合并后的课程记录，只有在课程身份、星期、周次集合一致且节次相邻或已经重叠时，才合并节次区间；例如 5–6 与 7 合并为 5–7。
3. 不同周次的记录不得因为节次相邻而合并；不同课程、教师、地点、校区、备注或课程编号也不得误合并。
4. 合并后的课程 ID 必须稳定且与最终节次区间一致，避免缓存键和前端详情键冲突。
5. 后端 API 契约继续使用单个连续 `start_section`/`end_section` 区间；非连续节次（如 1–4 与 7–8）仍保持独立记录。

## Files to Modify

- `internal/modules/academic/infrastructure/ouc/undergraduate_parser.go`：调整本科课程归并顺序和相邻节次合并逻辑。
- `internal/modules/academic/infrastructure/ouc/undergraduate_parser_test.go`：补充相同周次相邻节次、不同周次相邻节次、非相邻节次及身份差异的测试。

## Technical Implementation

- 保留现有课程身份字段作为合并边界。
- 先对身份相同且节次相同的记录执行周次并集。
- 再按身份和周次集合分组，排序节次区间；当下一个区间的开始节次不大于当前结束节次加一时扩展当前区间。
- 最终重新生成课程 ID，或在生成 ID 后只在最终区间稳定的阶段进行归并，确保 ID 不携带过期的起止节次。

## Testing Requirements

### Unit Tests

- 相同课程、相同周次、5–6 节与 7 节合并为 5–7 节。
- 相同课程、相同周次、1–4 节与 7–8 节不合并。
- 相同课程、不同周次、相邻节次不合并，避免丢失周次和节次对应关系。
- 相同节次、不同周次仍合并周次并集并保持排序。
- 教师、地点、课程编号等身份字段不同的记录不合并。

### Verification

- 运行 `go test ./internal/modules/academic/infrastructure/ouc`。
- 运行 `go test ./...`（如依赖和环境允许）。

---

**Implementation Notes:**

实现已完成并通过单元测试、全量测试和静态检查，当前保留在独立 Feature 分支，待代码评审。
