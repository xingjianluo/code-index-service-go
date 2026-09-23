# code-index-service-go

本地可运行的代码仓库索引与上下文检索服务：以本地文件系统模拟代码来源，实现
解析归一、增量索引、并发一致的检索装配，以及存储/规则版本的兼容与迁移。
只依赖 Go 标准库，无需任何外部服务或凭据。

## 快速开始

```bash
go test ./... -race          # 全部单测（含并发竞态）
go run ./cmd/indexctl -repo . -index ./.index index
go run ./cmd/indexctl -repo . -index ./.index query "your query"
go run ./cmd/indexctl -repo . -index ./.index status
go run ./cmd/indexctl -repo . -index ./.index check
```

## 代码结构

- `internal/model` — 数据模型与稳定 ID 规则
- `internal/parser` — 文件解析、归一与失败分类
- `internal/indexer` — 仓库遍历、增量比对、重命名检测
- `internal/storage` — 原子持久化、版本门禁、v0→v1 迁移
- `internal/retrieval` — 可解释打分与预算装配
- `internal/service` — 并发安全的快照发布与编排
- `cmd/indexctl` — 本地 CLI
- `docs/README.md` — 索引模型、标识规则、版本兼容、检索预算与验证方法的完整说明
