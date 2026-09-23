# 本地代码仓库索引与上下文检索服务

一个只依赖本地文件系统与 Go 标准库（无外部服务、无凭据）的代码索引服务：把一个
本地目录当作"代码来源"，解析源文件、归一为可检索片段、做增量索引，并在给定的
上下文预算内返回**可解释**的检索结果。

- 解析失败不会被静默跳过：不可读、超大、非法编码、不支持的类型、语法错误各自归入
  可区分的失败类别，作为快照的一等成员持久化。
- 增量索引正确处理新增 / 修改 / 删除 / 重命名；已删除或被覆盖的旧内容不会再被召回。
- 索引写入与检索读取并发进行时，读者只能看到某一个**完整**快照，不会看到半更新或
  新旧混合状态。
- 存储格式与索引规则分别带版本号；未知字段与不受支持的版本一律被明确拒绝，不做
  静默降级；v0 数据有确定的迁移路径。

## 1. 组件划分

| 包 | 职责 |
| --- | --- |
| `internal/model` | 核心数据模型：`File` / `Symbol` / `Reference` / `Chunk` / `Failure` / `Snapshot`，以及全部标识与哈希规则 |
| `internal/parser` | 读取与校验单个文件，按类型解析为 `model.File`，或产出带类别的 `model.Failure` |
| `internal/indexer` | 仓库遍历、与上一快照比对、重命名检测、生成不可变的确定性快照 |
| `internal/storage` | 快照的本地持久化（原子替换）、版本门禁、未知字段拒绝、v0→v1 迁移 |
| `internal/retrieval` | 在快照上做确定性打分检索，按预算取舍并记录每个被丢弃候选的原因 |
| `internal/service` | 编排三者；用原子指针交换发布快照，读写并发安全 |
| `cmd/indexctl` | 本地 CLI：`index` / `status` / `query` / `check` |

## 2. 索引模型

一次索引产出一个不可变 `Snapshot`：

```
Snapshot
 ├─ format_version / rule_version / generation
 ├─ files:    path -> File
 ├─ failures: path -> Failure       （失败不是日志，而是快照成员）
 └─ renames:  newPath -> oldPath    （本轮检测到的重命名）

File   = 内容元数据 + Symbols[] + References[] + Chunks[]
Chunk  = 自包含可检索片段（函数/方法体为 symbol 片段，其余行按 40 行窗口切成 text 片段）
```

- Go 文件用标准库 `go/parser` 解析：函数/方法/类型/变量/常量记为符号；除声明位置外
  的每个标识符出现记为引用；函数与方法体各自成片段，未覆盖的行（package、import、
  类型声明等）合并为文本片段，保证全文可检索。
- 文本/Markdown/`.mod` 文件不抽符号，按行窗口在空行边界处切分为文本片段。
- 解析前统一做 CRLF→LF 归一化，避免换行符扰动哈希。
- 所有切片均按确定顺序（位置或 ID）排序，保证同一棵树的任意两次索引逐字节等价。

## 3. 标识规则（稳定 ID）

所有 ID 都只依赖**内容与结构**，不依赖路径或索引时间，因此内容不变的条目在多次索引
之间 ID 保持稳定，重命名也不改变文件身份。

| ID | 计算方式（SHA-256 截断 16 位 hex，带类型前缀） |
| --- | --- |
| 文件 `FileID` | `sha256(归一化内容)` —— 与路径无关，同内容不同路径是同一身份 |
| 符号 `SymbolID` | `sha256(fileHash + kind + receiver + name + line + column)` |
| 引用 `RefID` | `sha256(fileHash + name + line + column)` |
| 片段 `ChunkID` | `sha256(fileHash + kind + symbol + startLine + endLine + content)` |
| 失败 `FailureID` | `sha256(path + category)` —— 同一路径同一失败类别稳定 |

增量规则：

- 哈希相同的文件**原样复用**旧 `File` 对象（连指针都不变），天然保证内部所有 ID 稳定。
- 哈希变化 → 新文件 ID 与新片段集，旧片段随旧快照一起丢弃。
- 路径消失且内容哈希在某个新路径上出现 → 记为重命名（newPath→oldPath），按排序后
  一一配对，结果确定；重命名不计为 add/delete。
- `generation` 仅在可见内容真正变化时 +1（新增/更新/删除/重命名/失败记录变化）；
  对同一棵树重复索引不会空涨。

## 4. 失败分类

| 类别 | 触发条件 |
| --- | --- |
| `unreadable` | stat/read 失败、文件消失、非常规文件（含符号链接，不跟随） |
| `oversized` | 大小超过上限（默认 256 KiB） |
| `invalid_encoding` | 内容不是合法 UTF-8（记录首个非法字节偏移） |
| `unsupported_type` | 扩展名没有注册解析器 |
| `parse_error` | 类型受支持但内容无法解析（Go 语法错误，带位置） |

失败记录带稳定 ID、可读原因与大小提示；失败文件被修复后转为健康条目，反之亦然。

## 5. 存储格式与版本兼容策略

快照目录：

```
<index-dir>/snapshot.json   # 快照本体（JSON，缩进）
<index-dir>/manifest.json   # format/rule/generation
```

写入采用"写临时文件 + `rename`"原子替换快照与清单；崩溃只可能留下上一个完整版本
或新版本，不会产生半个文件。服务内发布同样是单次原子指针交换。

两个独立版本号：

- `format_version`：**磁盘布局**版本（数组→映射之类）。
- `rule_version`：**索引规则**版本（符号/片段/ID 的产生方式）。

兼容规则（严格，禁止静默降级）：

1. **v0 → v1**：v0 为 `{"version":0, "files":[...], "failures":[...]}` 的无规则版本
   数组布局；加载时执行确定性迁移——严格解码（未知字段即拒绝）、按 path 重键、
   用现有哈希规则回填缺失 ID、回填片段的 `file_id/path`、失败按 path+类别重新发 ID、
   加盖 format/rules = v1。同一份 v0 字节永远迁移出相同快照。
2. **v1/v1**：正常读取。
3. **更高的 format 版本**：读路径（`Load`/检索）拒绝，错误为
   `unsupported index version`，并给出明确文案。
4. **缺失或不同的 rule 版本**：读路径拒绝并返回 `MigrationRequired`（包装
   `ErrUnsupportedVersion`）。
5. **任何未知 JSON 字段**：`DisallowUnknownFields` 直接拒绝，绝不忽略后继续使用。
6. **写路径的重建语义**：`Reindex` 在磁盘索引命中版本门禁（3/4/5）时不会失败，
   而是按既定迁移路径**从源码全量重建**（无法从不兼容谱系续写 generation，故新谱系
   从 generation 1 开始）；读路径仍然严格拒绝，绝不会凭空降级。与版本无关的真实
   I/O 错误不会触发重建，而是原样上抛，避免静默覆盖损坏数据。

## 6. 检索与预算装配规则

查询先做确定性分词（小写、字母数字与下划线、去重保序）。对每个候选片段：

| 信号 | 分值 |
| --- | --- |
| 符号名命中查询词（每词） | +15 |
| 规范化后的完整短语在内容中出现 | +30 |
| 内容中查询词出现次数（每次） | +2 |
| 路径命中查询词（每词，只计一次） | +10 |

排序：分数降序 → path → 起始行 → chunk ID。因此任意两次相同查询结果完全一致；
每个命中都带 `MatchedTerms` 与逐步加分说明 `Why`。

预算（零值取默认：20 片段 / 4000 tokens / 最多扫描 10000 片段）：

- 按排序结果顺序装配，累计估算 token（`model.EstimateTokens`，确定性的词数+行数估算）。
- 装不进 token 预算的候选 → 记入 `Dropped`，原因 `token_budget`；
  超过片段数上限 → `chunk_budget`；扫描数触顶 → 停止扫描并标记 `scan_cap`。
- `Result.TruncatedBy` 标明首个受限资源；`hits + dropped == totalHits` 恒成立，
  任何被舍弃的内容都可解释，绝不静默截断成一个"看起来正常"的短结果。
- 空查询（无有效词）或尚未建索引时返回空结果 / 明确错误，而不是倒出整个索引。

## 7. 并发语义

- 快照构造全程在本地暂存区完成，构造结束才原子发布；读者无锁，永远读到某一个完整快照。
- 进行中的检索始终用它开始时拿到的快照，即使期间发生了重索引。
- 写者之间互斥串行化；`Reindex` 先持久化（磁盘原子替换）再发布内存快照。
- 竞争条件由 `go test -race` 下的并发测试守护：40 轮交替改写 + 4 个并发读者，
  断言不出现两代内容共存、空片段集等半更新状态。

## 8. 本地验证方法

无需任何外部依赖（需要 Go 1.25+）：

```bash
# 全部单测（建议带竞态检测）
go test ./... -race -v

# CLI 端到端
go run ./cmd/indexctl -repo <某目录> -index ./.index index
go run ./cmd/indexctl -repo <某目录> -index ./.index status
go run ./cmd/indexctl -repo <某目录> -index ./.index query "你的查询词"
go run ./cmd/indexctl -repo <某目录> -index ./.index check
```

`check` 会校验所有 ID 都能由内容重算得到、映射键一致、片段 ID 无重复。

测试覆盖对应关系：

| 需求 | 测试 |
| --- | --- |
| 符号/引用/片段抽取、各类失败、ID 稳定性 | `internal/parser/parser_test.go` |
| 增改删重命名、失败不跳过、重复索引一致、generation 不空涨 | `internal/indexer/indexer_test.go` |
| 往返持久化、未知字段拒绝、未来版本拒绝、v0 迁移 | `internal/storage/store_test.go` |
| 确定性排序、解释、预算取舍有记录 | `internal/retrieval/retrieval_test.go` |
| 并发索引/检索一致性、重启后删除内容不召回 | `internal/service/service_test.go`（`-race`） |

所有测试日志均打印关键输入（文件、查询、版本载荷）与判定依据（类别、分值、
增量分类、加载结果），失败时原因可区分、可复现。
