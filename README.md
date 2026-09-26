# task26092707

Go 实现的目录增量全文索引器。

## 设计

- **分词**：按 Unicode 字母/数字的最长连续段切分并转小写（`tokenizer.go`）。
- **倒排索引**（`index.go`，独立索引目录）：append-only 段式结构。每批变更
  序列化为一个 `seg-NNNNNN` 段文件（含删除路径集合 + 新增 postings），提交 =
  写段文件（fsync + rename）→ 原子替换 `manifest`（tmp + rename + fsync 目录）。
  崩溃时未进入 manifest 的段全部丢失，`Open` 重放 manifest 引用的段并清理孤儿段。
- **文件记录**（`store.go`，SQLite）：`files(path,size,mtime,indexed_at)` +
  `meta(last_scan_start)`。记录只在对应索引批**提交之后**才写入，因此
  「有记录 ⇒ 索引已提交」；反向不一致（索引有、记录无）通过重新索引
  （delete+add，按路径去重）收敛，恢复时重复读取不超过一个提交批次。
- **增量扫描**（`indexer.go`）：无记录 / mtime 变化 / mtime 落在
  `last_scan_start - 2s` 安全窗内的文件会被（重）读；mtime 早于上次扫描开始
  2 秒以上且未变的文件不读。安全窗覆盖「读取后同秒等长改写并还原 mtime」的情形。
- **批提交**：每至多 100 个变更文件提交一次索引（测试可调小）。

## 接口

```go
ix, _ := indexer.Open(sourceDir, indexDir, dbPath)
ix.Reindex()            // 增量扫描
paths := ix.Search(term) // 包含该词的文件相对路径
recs := ix.Files()       // SQLite 中的文件记录
```

## 测试

```sh
go test ./...
```

- `TestCrashConvergence`：增/改/删混合变更下，在两个存储写入的每个间隙及
  批次中途用子进程 `os.Exit` 模拟断电（内存真实丢失），随后重开 + 一次完整
  `Reindex`，与独立朴素全量扫描逐词比对并核对 `Files()`。
- `TestSameSecondRewrite`：同秒等长改写并还原 mtime 后，下次 `Reindex` 能搜到
  新内容；mtime 远早于扫描的稳定文件不被读取。
