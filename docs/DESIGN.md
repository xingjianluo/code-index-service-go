# 设计说明：索引模型、版本兼容、检索与预算装配、符号关系查询

本文档描述标识规则、版本兼容策略、检索装配规则、符号关系（引用/影响
面）判定规则以及本地复现方法，对应实现分布在 `model / source / parser
/ store / indexer / retrieval / graph / service` 八个包中。

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
| `excluded`    | 常规文件但扩展名不在收录集合（`source.DefaultIncludeExt`） | `extension ".weird" not in indexed set` |

> `excluded` 是刻意的“看得见的不分析”：未知后缀的文件（包括非常见
> 后缀的文本、以及二进制资源）在扫描阶段被记录为 `ExcludedFile`，由
> indexer 转成失败条目进入快照与指纹。它们不产出符号/引用，但在引用
> 查询与影响面结果中以 `unanalyzable` 身份显式出现，不会凭空消失。
> 扩展名在收录集合内但内容不是合法 UTF-8 的二进制文件走
> `invalid_utf8`，同样显式归类。

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
| `Symbol.Key` | `skey | name | kind | line | 文件ContentHash` | **路径无关**：纯重命名/移动目录（字节不变）⇒ Key 不变，“同一条符号”跨改名可辨认；任何内容编辑 ⇒ 新 Key。同文件内同名同类别符号由行号区分（内容相同 ⇒ 行号相同，不破坏改名连续性） |
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
- 版本窗口：`store_format` 必须落在 `[MinSupported(1), Current(2)]`，
  `index_rules` 必须落在 `[MinSupported(1), Current(3)]`；超出范围返回
  `ErrUnsupportedVersion` 并在消息中给出实际值与支持区间——**不静默
  降级**。
- 加载时做完整性校验：由文件哈希与失败列表重算 `Snapshot.ID`，v2 数据
  若与存储的 id 不一致即判定损坏并拒绝；v1 数据无 id 字段，迁移后赋予
  重算身份。

### 2.3 确定迁移路径（v1 → v2 → v3）

当前版本：`store_format=2`、`index_rules=3`。加载时由
`parser.MigrateToCurrent` 按固定步骤链式迁移：

1. **v1 → v2**（`parser.MigrateV1ToV2`）：v1 与 v2 结构相同，差异在
   **分词规则**。用 v2 `Tokenize` 对每个片段的**已存储 Content** 重新
   生成 token；文件字节哈希、SymbolID、ChunkID 均不改变；metadata 标注
   `migrated_from=v1`。
2. **v2 → v3**：v3 为每个符号补充路径无关的 `Symbol.Key`，由记录中已有
   的 `(name, kind, line, contentHash)` 确定性重算
   （`model.StableSymbolKey`）；SymbolID、ChunkID、快照指纹均不变，
   因此迁移是**纯增量补字段**，不改变任何既有标识。

迁移后版本标记置为当前值，下次 `Save` 以新规则重写。加载旧数据时若未
提供迁移器，直接拒绝；片段缺少 Content 导致无法重归一化时迁移报错，
而不是带着半新半旧的数据继续服务。迁移行为由
`store.TestV1MigrationRetokenizes`（手工 v1 信封，覆盖重分词与符号
Key 回填）固定下来。

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

## 4. 符号关系查询（graph）

`graph` 包在**单个不可变快照**上回答两类“顺着符号找关系”的问题，
全部基于本地已有索引，不引入任何外部服务。所有枚举顺序确定
（路径/行/列，再按 ID），因此同一快照 + 同一查询 ⇒ 同一答案，
与查询先后、重复次数无关。

### 4.1 按符号查引用（References）

输入一个符号名，输出分三类、互不混淆：

- **Definitions**：该名字的全部定义，按归属优先级排序
  （`parser.LessSymbolPrecedence`：类别优先级
  `func<type<method<var<const<field`，再按路径、行号、SymbolID），
  每个定义带 `RefCount`（指到它的已解析引用数）与路径无关的 `Key`。
- **Resolved**：`SymbolID` 非空的引用点——确实指向某个定义。
  归属规则与索引期解析完全一致：**同文件定义优先**（行号升序、类别
  优先级、SymbolID），否则取全局第一（类别优先级、路径、行号、
  SymbolID）。同名多定义时哪条引用归哪个定义由这条固定规则决定，
  未胜出的定义 `RefCount=0`，仍完整列出。
- **Unresolved**：名字匹配但没能定位到任何定义的引用点（例如调用了
  未定义/未索引的函数）。它们只按名字命中，绝不混入 Resolved。

另外附 **Unanalyzable**：快照中所有失败/被排除文件（`excluded`、
`invalid_utf8` 等，见 1.2）。这些文件里可能存在该名字的引用，但索引
无从得知——显式列出（带上限），而不是静默缺席。

### 4.2 影响面分析（Impact）

输入一组改动过的文件路径（归一化：去空白、统一 `/`、去重、排序），
输出“重点回归范围”：

1. **输入分类**：每个输入路径被归入 已索引文件 / 类型化失败
   （unanalyzable，Notes 说明类别与原因）/ 快照中不存在
   （`UnknownInputs`，显式报告不丢弃）。
2. **反向 BFS**：从改动文件出发，沿“引用 → 定义”边反向传播——凡
   引用了已达文件内符号的文件，成为下一层受影响文件，深度上限
   `MaxDepth`（默认 2）。
3. **链路可解释**：每个受影响文件/符号都带 `Via` 边链（从改动文件到
   它的每一条 `引用方:行:列 -名字-> 定义方`），不是只给一堆名字。
   受影响符号的深度 0 表示“定义在改动文件中且被外部引用”。
4. **确定性**：文件与符号按路径/行号入队，消费边按
   （引用路径， 行， 列）排序遍历；输入文件的给出顺序不影响结果
   （有对应单测）。

### 4.3 改名场景下的兼容范围

- **纯重命名/移动目录**（字节完全相同、1:1、旧路径消失、新路径此前
  不存在）：`Symbol.Key` 不变 ⇒ 同一条符号跨改名被认出；引用在每次
  索引后全量重解析 ⇒ 指向新路径下的新 SymbolID，引用关系不断、不失效；
  影响面链路的 `ToSymbolKey` 与改名前一致。
- **改名 + 内容改动**：内容哈希变化 ⇒ 新 Key，按“不同的符号”对待
  （这是有意的：内容变了，符号语义可能已变）。
- **重复内容文件**（多份相同字节）：不满足 1:1，不做改名归类
  （按 新增+删除 报告），Key 相同但分属不同文件记录。

### 4.4 上限与取舍规则（graph.Budget）

| 字段 | 默认 | 含义 |
|---|---|---|
| `MaxDefinitions` | 50 | 单次 refs 列出的定义数，超出按优先级序截断（`max_definitions`） |
| `MaxReferences` | 500 | Resolved / Unresolved 各自的上限，超出按 （路径，行，列） 序截断（`max_references`） |
| `MaxAffectedFiles` | 200 | 影响面文件数上限，超出按确定性 BFS 顺序舍弃（`max_affected_files`） |
| `MaxAffectedSymbol` | 200 | 影响面符号数上限（`max_affected_symbols`） |
| `MaxDepth` | 2 | 影响面传播深度；改动文件本身深度 0 |
| `MaxEdges` | 2000 | BFS 遍历的引用边总数硬上限，触顶即停（`max_edges`） |
| `MaxUnanalyzable` | 100 | Unanalyzable 列表上限（`max_unanalyzable`） |

所有取舍都写入 `Truncated`（reason + 具体数量说明），舍弃的是确定
顺序下的后缀，不是任意子集；`Notes` 记录生效预算与各项计数，便于
复现判定过程。

---

## 5. 并发与可见性（service）

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
# 超大文件（oversize）、语法错误文件（unparseable）、
# 未知后缀文件 echo x > notes.weird（excluded）

# 符号关系查询：
go run ./cmd/codesearch refs   -store /tmp/ix -name Helper
#   def 行带 key= 与 refs=；ref resolved/unresolved 分列；! 行为 unanalyzable
go run ./cmd/codesearch impact -store /tmp/ix -files a.go,b.go -max-depth 2
#   每个受影响文件带 via[i] 引用链；? 行为快照中不存在的输入
# 改名连续性演练：mv a.go sub/a.go 后重新 index，再 refs -name Helper，
#   对比前后 key= 相同、resolved 数不变

go run ./cmd/codesearch serve -root ./some-repo -store /tmp/ix
#   GET /refs?name=Helper   GET /impact?files=a.go,b.go
```

失败原因可区分性可直接对照 `index` 输出的 `! [category] path: message
(size=n)`；版本拒绝行为可用任意 JSON 编辑器把
`index.json` 的 `store_format` 改成 99 或加一个未知字段后再次
`search`/启动 `serve` 观察明确报错。
