# 设计说明：索引模型、版本兼容、检索与预算装配

本文档描述标识规则、版本兼容策略、检索装配规则以及本地复现方法，
对应实现分布在 `model / source / parser / store / indexer / retrieval /
service` 七个包中。

---

## 1. 索引模型

### 1.1 文件发现（source）

- 代码来源就是**本地文件系统**。`source.Scanner` 对根目录做确定性
  `filepath.WalkDir`：结果按仓库相对路径（统一为 `/` 分隔）升序排列，
  使后续所有步骤（日志、报告、快照指纹）与遍历顺序无关。
- 默认剪枝 `.git / node_modules / .idea / .vscode`；默认收录常见源码扩展名
  （`.go .py .js ... .md .txt` 等，见 `source.DefaultIncludeExt`）。
- 符号链接、设备、套接字等非常规文件不作为源文件；**根目录缺失或目录
  不可读返回错误**，不静默忽略。扫描阶段的 `Size` 仅作参考，所有硬性
  判定以解析时重新 `stat + read` 为准。

### 1.2 解析与失败分类（parser）

每个被发现的文件恰好走向两个出口之一：`FileRecord` 或 `Failure`，
不存在静默跳过。失败类别可区分且消息带证据：

| 类别 | 触发条件 | 证据示例 |
|---|---|---|
 `unreadable`  | `stat/read` 失败且非“不存在”（权限、IO） | `read: permission denied` |
| `missing`     | 扫描与解析之间文件消失 | `file vanished between scan and parse` |
| `oversize`    | `stat` 或实际读取字节数 > `MaxFileSize`，或行数 > `MaxFileLines` | `size 300000 exceeds limit 100000 bytes` |
| `invalid_utf8` | 原始字节非合法 UTF-8（在大小检查之后） | `content is not valid UTF-8` |
| `unparseable` | Go 严格语法解析失败（`parser.AllErrors`） | `go parser: x.go:2:14: expected ')', found '{' (and 3 more errors)` |

> 先大小后编码的顺序是刻意的：避免为超大文件做 UTF-8 扫描。Go 的解析
> 建立在合法 UTF-8 之上，所以非法编码不会以“解析失败”的模糊面目出现。

记录内容：

- **FileRecord**：`path, language, size, content_hash, lines, symbols,
  references, chunks, metadata`。
- **Symbol**：Go 顶层 `func/method/type/var/const` 以及结构体字段、接口
  方法。下划线空白标识符 `_` 不抽取。
- **Reference**：AST 中每个 `*ast.Ident`（排除 package 子句本身）。
  跨文件解析按确定性优先级赋 `SymbolID`，未解析的引用保留空目标但仍可按
  名字检索（见 1.5）。
- **Chunk（片段）**：Go 以顶层声明为边界（函数/方法/类型/var/const 块，
  含其 doc comment 与结构体字段），声明之间的非空头部（package/imports）
  与尾部归为 `file` 类片段；超长声明按 `MaxChunkLines` 再切分。纯空白
  区间不产生片段；空文件保留一个片段使文件仍可被检索。非 Go 文本按
  `MaxChunkLines` 行窗口切片。每个片段携带 v2 归一 token。

### 1.3 稳定标识规则（model/ident.go）

所有标识都是输入字节的确定性 SHA-256 派生值（截断 16 字节十六进制），
**不使用随机数或时钟**：

| 标识 | 派生输入 | 稳定性含义 |
|---|---|---|
| `FileRecord.ContentHash` | 文件原始字节 | 任何路径上相同字节 ⇒ 相同哈希；增量判定与重命名检测的唯一权威（不信 mtime） |
| `Symbol.ID` | `sym | path | name | kind | line` | 同路径同符号同行号 ⇒ 跨重复索引稳定；重命名文件会带着符号“移动”（路径变 ⇒ ID 变）；同位置修改函数体 ⇒ ID 不变 |
| `Chunk.ID` | `chk | path | start:end | 文件ContentHash | 片段自身字节哈希` | 同一文件版本内稳定；**文件任何字节变化都会使全部片段 ID 变化**，旧片段在新快照中不可达 |
| `Snapshot.ID` | 全部（路径排序后的 `path||content_hash`）+ 失败列表（path、category、message 排序后） | 同一仓库状态（含相同坏输入）⇒ 同一快照 ID，与经历过哪些增量操作、与并发时序无关 |

“内容未变的条目在多次索引间标识保持稳定”的落实方式：增量索引命中
`同路径 + 同哈希` 时，**原样复用旧记录对象**（而不只是重新计算），
保证片段/符号 ID 逐字节稳定。

### 1.4 增量索引（indexer）

两遍处理，单次通过：

1. **解析遍**：重新读取每个候选文件并计算原始字节哈希，收集
   `content_hash -> [新路径]`；坏输入归入失败列表（排序后参与指纹）。
2. **重命名判定**：仅当相同字节在新旧两侧都恰好出现于一个路径，且旧路径
   已消失、新路径此前不存在时，记为一次纯重命名。要求 1:1 是为了在
   存在重复文件时不做歧义归类（那种情况按 新增+删除 报告）。
3. **归类**：`rename / added / unchanged / modified / deleted`。
   删除即不出现在新快照 map 中——检索只读新 map，故旧内容不可能被召回。
4. **引用解析**：在完整的新记录集合上一次性做确定性解析。
5. **提交**：构造全新不可变 `Snapshot`，计算指纹，`store.Save` 原子落盘
   后返回；发布（指针交换）由 service 完成。

同一快照重复索引：全部文件进 `unchanged`，报告分类可能只反映“无变化”，
`Snapshot.ID` 相同。

### 1.5 跨文件引用解析（parser/resolve.go）

纯名字匹配，保守且确定。同名符号的优先级：

1. **同文件优先**；同文件内按（行号升序、类别优先级、SymbolID）。
2. 否则全局：类别优先级 `func < type < method < var < const < field`，
   再按（定义路径、行号、SymbolID）。

解析只依赖记录集合本身、与输入顺序无关；重复索引得到相同归属。
找不到定义的名字 `SymbolID` 为空，仍作为可检索引用保留。

---

## 2. 存储格式与版本兼容（store）

### 2.1 磁盘布局与原子性

```
<store-dir>/index.json      唯一已提交信封
<store-dir>/index.json.tmp  暂存（写完 fsync 后 rename 覆盖）
```

写入 = 写临时文件 → `Sync()` → `rename()`。读者要么读到旧完整文件，
要么读到新完整文件，永远不会读到半个 JSON。进程在写中途崩溃只可能留下
`.tmp`（下次写入先截断重建），不污染已提交文件。

### 2.2 信封与拒绝策略

信封顶层：`id, store_format, index_rules, root, created_at, files,
failures`；文件层：`path, language, size, content_hash, lines, symbols,
references, chunks, metadata`。

- 解码使用 `DisallowUnknownFields`（顶层和文件层同理），**未知字段直接
  报错**，例如 `json: unknown field "mystery"`。
- 版本窗口：`store_format` 与 `index_rules` 都必须落在
  `[MinSupported(1), Current(2)]`；超出范围返回
  `ErrUnsupportedVersion` 并在消息中给出实际值与支持区间——**不静默
  降级**。
- 加载时做完整性校验：由文件哈希与失败列表重算 `Snapshot.ID`，v2 数据
  若与存储的 id 不一致即判定损坏并拒绝；v1 数据无 id 字段，迁移后赋予
  重算身份。

### 2.3 v1 → v2 的确定迁移路径

当前版本：`store_format=2`、`index_rules=2`。v1 与 v2 的结构（文件/符号/
引用/片段）相同，差异在**分词规则**：

- v1：按非字母数字切分 + 小写，且只保留长度 ≥ 2 的 token，不拆驼峰；
- v2：见 3.1。

迁移（`parser.MigrateV1ToV2`）在加载时执行：

1. 用 v2 `Tokenize` 对每个片段的**已存储 Content** 重新生成 token；
   文件字节哈希、SymbolID、ChunkID 均不因迁移改变（其定义未变）；
2. 文件 metadata 标注 `migrated_from=v1`；
3. 重算快照指纹并把版本标记置为 2，下次 `Save` 以 v2 重写。

加载 v1 数据时若未提供迁移器，直接拒绝（“v1 data requires a migrator
but none configured”）。片段缺少 Content 导致无法重归一化时迁移报错，
而不是带着半新半旧的 token 继续服务。迁移行为由
`store.TestV1MigrationRetokenizes` 用手工 v1 信封固定下来。

---

## 3. 检索与预算装配（retrieval）

### 3.1 查询归一

查询与片段使用同一套 v2 `Tokenize`：非字母数字切分；标识符边界拆分
（`GetUserName2FA → get/user/name/2/fa`，`HTTPServer → http/server`，
字母⇄数字边界拆分）；小写；丢弃空 token、>64 rune 的噪声 token、
>8 位纯数字 token；按首次出现去重。因此大小写与命名风格差异不影响命中。

### 3.2 打分（逐项可解释）

```
score = 3.00 * 标题命中次数
      + 1.50 * 查询中被命中的不同 token 数
      + 0.25 * token 在片段中的总出现次数
      + 1.00 * declaration 片段加成
      + 0.01 * 符号密度（len(symbolIDs)/len(tokens)*100）
```

每个分数项都可在 `Breakdown` 中核对（结果日志打印命中词与分数）。
平局键：分数降序 → 路径升序 → 起始行升序 → ChunkID 升序。
同一快照、同一查询永远产出同一答案。

### 3.3 预算与确定性取舍

预算字段（0 取默认值）：

| 字段 | 默认 | 含义 |
|---|---|---|
| `MaxCandidates` | 400 | 参与计算的片段数上限。片段先按 (路径, 起始行, ID) 固定排序，再截断；截断事件记录为 `candidate_cap` |
| `MaxResults` | 10 | 返回条目上限；名次之后的命中逐条记录 `max_results`（含名次、分数、上限值） |
| `MaxChunkChars` | 4000 | 单片段内容字节上限；按行边界截断并追加显式 `…[truncated …]` 标记（标记计入预算，返回值绝不超过上限）。连解释标记都放不下时记 `max_chunk_chars`，该条不予收录 |
| `MaxTotalChars` | 20000 | 已收录内容字节总和硬上限；放不下的下一名记 `max_total_chars`（含所需字节数与剩余预算） |

关键语义：

- 截断只发生在**按名次装配阶段**，任何舍弃都进入 `Outcome.Truncated`，
  带 `reason / chunk_id / score / detail`，不是静默把结果切短。
- **存在命中但预算一条也容纳不下**时返回
  `ErrBudgetUnsatisfiable`（消息含命中数与实际预算值），调用方必须显式
  处理；查询归一化为零 token 是参数错误；无命中是合法的空结果（Notes 中
  说明判定依据）。
- 输出带 `Notes`：原始/归一化查询、生效预算、命中片段数、返回数、
  已用字符数，便于复现判定过程。

---

## 4. 并发与可见性（service）

- `*model.Snapshot` 创建后不再修改。`Service` 以 `atomic.Pointer[Snapshot]`
  持有“当前快照”，只在索引**落盘成功之后**整体交换。
- 检索开始时取一次快照指针，整次请求都基于该不可变对象：索引并发提交
  不影响进行中的检索；读者不可能看到半更新或新旧混合。
- 索引写以 CAS 标志 + 互斥锁串行化；并发的第二个 `Reindex` 立即得到
  `ErrIndexing`（HTTP 层映射为 409），不做交叉写入。
- 存储写入本身也是原子的（temp + fsync + rename），两层保证：即使绕过
  service 直接使用 indexer+store，读者同样不会读到半截文件。
- `service` 包的 `TestConcurrentIndexAndSearchNeverMixed` 在 `-race`
  下交替覆盖写（新增/删除两个文件版本）与多路检索，并校验读者观察到的
  每个快照 id 都是写者已发布的 id、且结果内容不跨版本混杂。

---

## 5. 符号关系追踪（relation）

`relation` 包在已发布的不可变快照上回答两类“顺着符号找关系”的问题，
只使用本地索引，不引入任何外部服务。对应 service 方法
`References / Impact`、CLI 子命令 `refs / impact`、HTTP 端点
`/refs?name=`、`/impact?files=`。

### 5.1 按符号查引用（References）

输入一个符号名字，输出该名字的**全部**出现位置，并严格分成两类，绝不
混在一起：

- `resolved`：索引期已绑定到某个定义的出现（`Reference.SymbolID` 非空），
  每条带目标定义的位置与改名稳定键（`target_key`）；
- `unresolved`：名字出现了但全仓库没有任何定义可绑定（`SymbolID` 为空），
  只带出现位置。

归属规则（同名多个定义时）：**查询不重做归属判定**，只报告索引期
`parser.ResolveReferences` 已经定下的结果——同文件定义优先，否则按类别
优先级 `func < type < method < var < const < field`，再按（定义路径、
行号、SymbolID）。因此同一份快照上重复查询、或以不同顺序发起查询，
结果逐字节一致（测试里用两次查询的 JSON 逐字节比较固定这一点）。
定义列表本身按（路径、行号、类别、ID）固定排序输出。

### 5.2 按改动文件算影响面（Impact）

输入一组改动过的文件（仓库相对路径，归一化、去重、排序后处理，与调用
顺序无关），输出：

- `changed`：**每个**输入文件的明确归类，不静默丢弃：
  - `indexed`：在索引中，其定义的符号作为影响源；
  - `failed`：在索引失败列表中，原样带出失败类别与证据（如
    `invalid_utf8`）——非常见后缀但落在收录扩展名内、内容又不是普通
    文本的文件落在这里；
  - `unknown`：不在索引中（扩展名被扫描规则排除、被剪枝、或根本不
    存在），说明原因并标明“不贡献符号级影响”。
- `impacted`：直接受影响文件。判定规则：其它文件中的某条引用在索引期
  被解析到**改动文件中定义的符号**。每条影响带引用链
  `defines: 改动文件 -> 符号` + `references: 受影响文件:行 -> 符号`，
  链上带定义位置、改名稳定键与该文件内出现次数。
- `suspected`：疑似受影响，单独列出。判定规则：某出现的名字在改动文件
  中也有定义，但索引期被绑定到了**其它未改动文件**的同名定义（多定义
  全局兜底）；编辑改动文件可能改变归属排序，故列出但不计入直接影响。
  绑定到**同文件**定义的出现可证明不受影响（同文件恒优先），不列出。

### 5.3 改名场景下的兼容范围

- 符号有两个身份：路径敏感的 `Symbol.ID`（`sym|path|name|kind|line`，
  改名即变）与改名稳定的 `StableSymbolKey`
  （`symkey|文件ContentHash|name|kind|line`）。纯重命名/挪目录保持文件
  字节不变 ⇒ ContentHash、行号、类别都不变 ⇒ **Key 不变**。
- 兼容范围：凡被索引器判定为**纯重命名**（内容字节完全相同、旧路径
  消失、新路径此前不存在、1:1 对应）的情形，符号的 Key 连续，引用关系
  在改名后的快照中照常解析（引用解析是纯名字匹配，与路径无关），不会
  断掉或变成未解析；`refs` 输出里能看到“同一个 Key、新的路径与新的
  路径敏感 ID”。
- 边界：**内容有改动**的“改名”不在此列（按 新增+删除 报告，符号视为
  新定义）；内容完全相同的**重复文件**在同一快照内会同 Key（此时需
  结合路径区分）；Key 只保证“同内容同位置符号”的连续性，不表达语义
  等价。

### 5.4 上限与取舍规则

`relation.Limits`（0 取默认值），所有上限都先按确定规则排序再截断，
截断事件写入 `Truncated`（含原因与数量），不静默丢：

| 字段 | 默认 | 含义 |
|---|---|---|
| `MaxDefinitions` | 64 | 一个名字纳入输出的定义数；超出按（路径、行号、类别、ID）截断，记 `max_definitions` |
| `MaxReferences` | 500 | resolved / unresolved 每类各保留的出现数；超出按（路径、行、列）截断，记 `max_references` |
| `MaxImpacted` | 200 | impacted / suspected 每类各保留的文件数；超出按路径序截断，记 `max_impacted` |
| `MaxChainEdges` | 8 | 每个受影响文件保留的引用链数（一条链对应一个依赖符号）；超出按符号名序截断，记 `max_chain_edges` |

同一文件对同一符号的多次出现合并为一条链，最早出现位置作为证据、
总次数写入链的 detail，避免大文件刷爆链数上限。

---

## 6. 本地复现指南

```bash
go build ./...
go vet ./...
go test ./... -v          # 关键输入与判定依据见各测试 t.Logf 输出
go test -race ./...       # 并发用例竞态检测
```

推荐手工演练（全过程离线）：

```bash
go run ./cmd/codesearch index  -root ./some-repo -store /tmp/ix
go run ./cmd/codesearch search -store /tmp/ix -query "your symbol"
# 修改/重命名/删除文件后再次 index，观察 + ~ - > 分类与 snapshot id
# 制造坏输入：printf 'bad: \xff' > x.go（invalid_utf8）、
# 超大文件（oversize）、语法错误文件（unparseable）

# 符号关系追踪（第 5 节）
go run ./cmd/codesearch refs   -store /tmp/ix -name YourSymbol
go run ./cmd/codesearch impact -store /tmp/ix -files pkg/a.go,pkg/b.go
# 纯改名演练：mv pkg/a.go pkg/b.go && 再次 index 后 refs 同一符号，
# 观察 key 不变、路径敏感 id 变化、引用仍然 resolved

go run ./cmd/codesearch serve -root ./some-repo -store /tmp/ix
#   GET /refs?name=YourSymbol   GET /impact?files=pkg/a.go,pkg/b.go
```

失败原因可区分性可直接对照 `index` 输出的 `! [category] path: message
(size=n)`；版本拒绝行为可用任意 JSON 编辑器把
`index.json` 的 `store_format` 改成 99 或加一个未知字段后再次
`search`/启动 `serve` 观察明确报错。
