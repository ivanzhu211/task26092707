# task26092707
## 目录增量全文索引器（Go）

`Open(sourceDir, indexDir, dbPath)` / `Reindex()` / `Search(term)` / `Files()`。

设计要点：

- **两个存储，无共同事务**。倒排索引在 `indexDir`（gob 段文件 + manifest），文件记录在 SQLite。
- **原子提交**：每批（≤100 个变更文件）写一个段文件（tmp+fsync+rename），再原子替换 manifest（rename+目录 fsync）。崩溃时未提交批次整体丢失，已提交批次完整保留；`Open` 只重放 manifest 引用的段，孤儿段被忽略清理，绝不全量重建。
- **崩溃收敛**：索引的文档表（路径、大小、mtime、索引时间）随每次提交持久化，是恢复的事实来源。`Reindex` 用它对比磁盘计算增/改/删；每次扫描结束时让 SQLite 精确镜像索引文档表（仅元数据 diff），修复任何崩溃留下的分歧。
- **1 秒 mtime**：mtime 一律截断到秒。mtime ≥ 上次扫描开始时间 − 2 秒的文件，即使 (size, mtime) 与记录一致也重读，以捕获「读取后同秒等长改写并恢复 mtime」；更老的未触碰文件绝不重读。
- **分词**：按 Unicode 字母/数字切分并转小写；只收 UTF-8 合法的 `.txt` 文件。

测试：

```sh
go test ./...
```

- `TestCrashConvergence`：增/改/删混合下，在两个存储写入的每个间隙及批次中途模拟崩溃（panic 真正丢弃内存状态，重开仅靠磁盘），随后一次完整 `Reindex` 与朴素全量实现逐词对照，并核对 `Files()`。
- `TestSameSecondRewrite`：文件被读取后、扫描结束前被等长改写并恢复 mtime，断言下次 `Reindex` 搜到新内容，且 mtime 远早于扫描的稳定文件未被重读。
