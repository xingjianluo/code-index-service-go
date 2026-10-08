# codesearch — 本地代码仓库索引与上下文检索服务

一个完全在本地运行的代码仓库索引与上下文检索服务。**代码来源用本地文件系统模拟**：
不依赖任何外部服务、数据库或真实凭据；索引以单个带版本号的 JSON 文件落盘，
检索在内存中的不可变快照上进行。

能力概览：

- **解析与归一**：扫描本地目录，解析 Go 源码（AST）与通用文本文件，抽取
  文件、符号（func/method/type/var/const/field）、引用关系与可检索片段。
  不可读、非法 UTF-8、超出大小/行数上限、无法解析的输入都**不会被静默跳过**，
  而是进入可区分的失败类别（`unreadable / invalid_utf8 / oversize /
  unparseable / missing`），并参与快照指纹计算。
- **增量索引**：基于“路径 + 原始字节 SHA-256”判定新增/修改/删除/重命名；
  同一路径内容不变时记录与其全部派生标识保持稳定；修改会替换旧片段，
  删除/覆盖后的旧内容不可被召回；纯重命名（内容字节完全相同、旧路径消失、
  新路径此前不存在、1:1 对应）单独报告。
- **版本演进**：磁盘信封带 `store_format` 与 `index_rules` 双版本号；
  未知字段（`DisallowUnknownFields`）与不受支持的版本被**明确拒绝**；
  旧版（v1）数据有确定的迁移路径（用 v2 规则重新分词），不静默降级。
- **可解释检索 + 预算装配**：打分构成逐项输出，排序完全确定；在
  `MaxResults / MaxTotalChars / MaxChunkChars / MaxCandidates` 预算下按名次
  确定性取舍，每条舍弃都记录原因；有命中但预算无法容纳任何结果时返回
  `ErrBudgetUnsatisfiable`，绝不静默截断成空/无效结果。
- **符号关系追踪**：按符号查引用（resolved/unresolved 严格分列，同名多定义
  归属采用索引期固定规则，重复查询结果一致）；按改动文件算影响面（每条影响
  带引用链，改动文件逐个明确归类：indexed/failed/skipped/unknown）；纯改名
  不改内容时符号凭改名稳定键（StableSymbolKey）保持连续，引用关系不断；
  所有上限确定性截断并记录原因。
- **非常见后缀/非文本文件不凭空消失**：扩展名不在收录集合的文件（如
  `.dat/.tmpl/.inc`）在扫描时记入快照 `skipped` 列表（内容从未读取）；
  内容不是可分析文本的文件进入 `failures`（如 `invalid_utf8`）。查引用时
  这些文件在 `unanalyzed` 清单中逐个列出并写明类别与原因；算影响面时
  它们分别归类为 `skipped`/`failed`，与“路径根本不存在”的 `unknown`
  一眼可分。
- **并发一致**：快照不可变，服务用原子指针发布；检索永远看到完整的旧快照
  或完整的新快照，不会读到半更新或新旧混合状态；索引写串行化。
- **可复现测试**：覆盖增量/重命名、异常或缺字段输入、版本差异、并发读写；
  测试日志打印关键输入、归一结果与判定依据。

## 组件划分

| 包 | 职责 |
|---|---|
| `model` | 核心数据模型、版本常量、稳定标识/指纹规则 |
| `source` | 本地文件系统扫描（模拟代码来源），确定性顺序，坏目录显式报错，扩展名未收录的文件进入显式排除清单 |
| `parser` | 读取校验与失败分类、Go AST 解析、文本归一、v2 分词、跨文件引用解析、v1→v2 迁移 |
| `store` | 版本化信封、未知字段/版本拒绝、临时文件 fsync+rename 原子写入、完整性校验 |
| `indexer` | 两遍式增量索引（解析→1:1 重命名判定→增删改归类→引用解析→原子提交） |
| `retrieval` | 确定性打分排序、预算装配、可解释截断、零准入显式失败 |
| `relation` | 按符号查引用（resolved/unresolved 分列 + 未分析文件 unanalyzed 清单）、按改动文件算影响面（带引用链，indexed/failed/skipped/unknown 归类）、改名稳定键、确定性上限取舍 |
| `service` | 不可变快照的原子发布、读写并发编排、索引串行化 |
| `cmd/codesearch` | `index` / `search` / `refs` / `impact` / `serve` 本地命令行入口 |

## 快速开始

```bash
# 构建
go build ./...

# 对一个本地目录建立索引（默认存储到 ./.codesearch-index/index.json）
go run ./cmd/codesearch index -root ./path/to/repo -store ./tmpidx -max-size 2097152

# 检索
go run ./cmd/codesearch search -store ./tmpidx -query "Greeting" \
  -max-results 10 -max-chars 20000 -max-candidates 400

# 按符号查引用（resolved / unresolved 分列）
go run ./cmd/codesearch refs -store ./tmpidx -name Greeting

# 按改动文件算影响面（带引用链，改动文件逐个归类）
go run ./cmd/codesearch impact -store ./tmpidx -files pkg/a.go,pkg/b.go

# 本地 HTTP 服务（仅监听 127.0.0.1）
go run ./cmd/codesearch serve -root ./path/to/repo -store ./tmpidx -addr 127.0.0.1:8080
#   GET /healthz   当前快照 id
#   POST /reindex  增量重建并原子发布（并发调用返回 409）
#   GET /search?q=...  GET /refs?name=...  GET /impact?files=a.go,b.go
```

`index` 输出中 `+ ~ - > !` 分别表示新增、修改、删除、重命名、失败；
失败行带方括号类别与具体证据（大小、解析器位置/文本）。

## 本地验证

```bash
# 全量单测（含关键输入与判定日志）
go test ./... -v

# 非常见后缀/非文本文件的归类专项（引用侧 unanalyzed、影响面侧
# skipped/failed/unknown 区分、确定性）
go test ./relation/ -run 'Unanalyzed|Distinguishes' -v

# 竞态检测（重点并发用例在 service 包）
go test -race ./...

# 静态检查
go vet ./...
```

测试均可在无网络、无凭据的环境复现；所有仓库内容都在 `t.TempDir()` 内现造，
版本差异用例（`store.TestV1MigrationRetokenizes`）手工构造 v1 信封磁盘文件，
不依赖任何历史产物。

## 进一步阅读

- [docs/DESIGN.md](docs/DESIGN.md)：索引模型与标识规则、版本兼容策略、
  检索与预算装配规则、符号关系追踪（判定规则/改名兼容范围/上限取舍）、
  失败语义与并发模型的详细说明。
