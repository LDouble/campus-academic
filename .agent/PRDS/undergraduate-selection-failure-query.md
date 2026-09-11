# Academic Provider：补充本科生落选与退选课程选课结果

**Priority:** High
**Status:** Done
**Type:** Enhancement
**Created:** 2026-09-11
**Last Updated:** 2026-09-11

## 概述

本科生教务系统将正常选课结果与退选/落选结果分成同一路径下的不同查询条件。现有 provider 只请求 `lx=xkrz`，导致“抽签落选”“个人退选”“管理员退选”等记录没有进入选课结果响应。

本任务在不增加 `provider-ouc.json` 配置项、不改变现有公开 API 的前提下，复用已配置的本科生 `selections` 路径，仅覆盖补充查询所需的查询参数，获取并合并退选/落选记录。

## 用户故事

**作为**查看本科生选课结果的用户
**我希望**看到教务系统返回的落选和退选课程记录
**以便**完整了解本学期课程选择结果，而不是只看到当前已选课程。

## 实现范围

- 在本科生 `ListCourseSelections` 流程中保留现有正常选课结果查询。
- 复用现有 `undergraduate.selections` 配置，不新增配置字段或第二个 endpoint 配置。
- 使用同一个 `xnxqid` 学期值请求退选/落选数据，补充参数包括：
  - `lx=tkrz`
  - `type=list`
  - `cxsj=tkjg`
  - `pageNum=1`
  - `pageSize=20`
  - `sf_request_type=ajax`
- 复用现有 JSON 表格解析逻辑，利用 `tklx` 的“抽签落选”“个人退选”“管理员退选”等文本归一为 `failed` 状态。
- 将补充记录与正常记录合并后返回现有 `course-selections` 契约。
- 对完全相同的教务记录去重，同时保留同一课程存在多个真实退选记录的情况，避免错误合并业务记录。
- 补充 provider 单元测试，覆盖请求参数、响应解析、结果合并、重复记录和补充请求失败时的兼容行为。

## 非范围

- 不新增或修改 `POST /api/v1/academic/course-selections`。
- 不修改小程序页面、状态枚举或展示逻辑。
- 不修改 `provider-ouc.json` 的 `selections` 配置。
- 不修改课程目录、正式课表或已选课程课表同步接口。
- 不改变研究生选课结果查询逻辑。

## 技术方案

1. 将 provider 的通用查询流程抽出可覆盖查询参数的内部入口；默认查询保持现有配置行为。
2. 本科生选课结果先执行现有 `lx=xkrz` 请求，再使用相同的认证会话和 `xnxqid` 执行退选/落选请求。
3. 两次响应均通过现有 `ParseSelections` 解析，依靠 `tklx` 字段识别失败状态。
4. 以稳定的上游记录标识和关键字段构造去重键；不因课程号相同而合并不同上游记录。
5. 补充请求采用 best-effort：补充接口失败时保留正常选课结果，避免新逻辑导致现有选课结果整体不可用；通过测试和日志保持可诊断性。

## 文件修改计划

- `internal/modules/academic/infrastructure/ouc/provider.go`：支持覆盖查询参数，补充本科生退选/落选请求并合并结果。
- `internal/modules/academic/infrastructure/ouc/provider_test.go`：增加 provider 请求与合并行为测试。
- `internal/modules/academic/infrastructure/ouc/undergraduate_parser_test.go`：必要时补充真实退选响应的解析断言。

## API 与配置

现有平台接口保持不变：

- `POST /api/v1/academic/course-selections`

本科生下游补充请求复用现有配置中的：

- `GET https://jwgl2024.ouc.edu.cn/jsxsd/xkgl/loadXsxkjgList`
- `xnxqid=<period_id>`

本次不新增 provider 配置项。

## 验收标准

- 正常 `lx=xkrz` 结果仍按现有逻辑返回。
- 退选/落选查询使用同一学期 `xnxqid`，并携带约定的退选查询参数。
- 示例响应中的“抽签落选”“个人退选”“管理员退选”记录都能返回，状态为 `failed`，并保留退选原因文本。
- 正常结果与补充结果不会产生完全重复记录。
- 补充请求失败时，正常选课结果仍可返回。
- 研究生流程、课程目录和其他学业查询不受影响。
- 改动范围内 Go 测试通过，并执行 `git diff --check`。

## 风险与注意事项

- 上游退选接口使用与正常接口相同路径但不同查询条件，不能把条件硬编码进公共配置，否则可能影响正常查询。
- 上游返回的同一课程可能有多条真实退选记录，去重必须基于上游记录标识而不是课程号。
- 两次请求应尽量复用已有会话，避免重复登录和不必要的认证压力。

---

**关联 Issue:** https://github.com/LDouble/campus-academic/issues/28
