# Memory System Go 化实施计划

## 概述

将 Hermes Agent 的记忆系统从 Python 移植到 Go，分两步：
1. **Phase 1**：核心记忆系统（MemoryStore + MemoryProvider + MemoryManager）
2. **Phase 2**：Holographic 插件（SQLite + FTS5 + HRR 向量代数）

参考源码：`https://github.com/nousresearch/hermes-agent`

---

## Phase 1：核心记忆系统 Go 化

> 目标：实现 MEMORY.md / USER.md 的文件持久化记忆，支持 frozen snapshot、并发安全、注入扫描。
> 预估：600-700 行 Go，2-3 天

### 1.1 MemoryStore（存储引擎）

**源码参考**：`tools/memory_tool.py` — `MemoryStore` 类

**数据结构**：
```go
type MemoryStore struct {
    mu               sync.RWMutex
    memoryEntries    []string
    userEntries      []string
    memoryCharLimit  int    // 默认 2200
    userCharLimit    int    // 默认 1375
    snapshot         map[string]string // "memory" / "user" → frozen 渲染结果
}
```

**需实现的方法**：

| 方法 | 说明 | 关键点 |
|------|------|--------|
| `LoadFromDisk()` | 从 MEMORY.md / USER.md 加载，冻结 snapshot | 去重（保留首次出现） |
| `SaveToDisk(target)` | 原子写：tmpfile + `os.Rename` | 同目录确保原子性 |
| `Add(target, content)` | 追加条目 | 文件锁 + reload + 容量检查 + 去重 + 注入扫描 |
| `Replace(target, oldText, newContent)` | 子串匹配替换 | 多匹配时报错（除非完全相同） |
| `Remove(target, oldText)` | 子串匹配删除 | 同 Replace 的匹配逻辑 |
| `FormatForSystemPrompt(target)` | 返回 frozen snapshot | 不返回实时状态 |

**文件格式**：
- 条目分隔符：`\n§\n`（section sign）
- 文件路径：`{HERMES_HOME}/memories/MEMORY.md` 和 `USER.md`

**并发安全**：
- 写操作：`syscall.Flock` 文件锁 + 锁内 reload + 原子写（`os.CreateTemp` → `os.Rename`）
- 读操作（`LoadFromDisk`）：无需文件锁，原子写保证读到完整文件

### 1.2 注入扫描

**源码参考**：`tools/memory_tool.py` — `_scan_memory_content()`

```go
func ScanMemoryContent(content string) error
```

**需实现**：
- 威胁正则检测（prompt injection、数据外泄、SSH 后门等，约 12 条规则）
- 隐形 Unicode 字符检测（零宽空格等，约 10 个字符）
- 匹配到则返回 error，阻止写入

### 1.3 MemoryProvider 接口

**源码参考**：`agent/memory_provider.py` — `MemoryProvider` ABC

```go
type MemoryProvider interface {
    Name() string
    IsAvailable() bool
    Initialize(sessionID string, opts map[string]any) error
    SystemPromptBlock() string
    Prefetch(query string, sessionID string) string
    QueuePrefetch(query string, sessionID string)
    SyncTurn(userContent, assistantContent, sessionID string)
    GetToolSchemas() []ToolSchema
    HandleToolCall(toolName string, args map[string]any) (string, error)
    Shutdown()

    // 可选 hooks（提供默认空实现的嵌入 struct）
    OnTurnStart(turnNumber int, message string, kwargs map[string]any)
    OnSessionEnd(messages []Message)
    OnPreCompress(messages []Message) string
    OnMemoryWrite(action, target, content string)
    OnDelegation(task, result, childSessionID string)
}
```

**默认实现**（嵌入用）：
```go
type BaseMemoryProvider struct{}
// 所有可选 hook 的空实现
```

### 1.4 BuiltinMemoryProvider

**源码参考**：`agent/builtin_memory_provider.py`

```go
type BuiltinMemoryProvider struct {
    BaseMemoryProvider
    store              *MemoryStore
    memoryEnabled      bool
    userProfileEnabled bool
}
```

- `Name()` → `"builtin"`
- `IsAvailable()` → 始终 `true`
- `Initialize()` → 调用 `store.LoadFromDisk()`
- `SystemPromptBlock()` → 拼接 frozen snapshot
- `GetToolSchemas()` → 返回空（memory tool 由 agent loop 拦截）
- 其他方法空实现

### 1.5 MemoryManager（编排层）

**源码参考**：`agent/memory_manager.py`

```go
type MemoryManager struct {
    providers      []MemoryProvider
    toolToProvider map[string]MemoryProvider
    hasExternal    bool // 最多 1 个外部 provider
}
```

**需实现的方法**：

| 方法 | 说明 |
|------|------|
| `AddProvider(p)` | 注册 provider，builtin 始终接受，外部最多 1 个 |
| `BuildSystemPrompt()` | 收集所有 provider 的 system prompt block |
| `PrefetchAll(query, sessionID)` | 遍历 provider 调 Prefetch，容错 |
| `QueuePrefetchAll(query, sessionID)` | 遍历调 QueuePrefetch |
| `SyncAll(user, assistant, sessionID)` | 遍历调 SyncTurn |
| `GetAllToolSchemas()` | 收集所有 provider 的 tool schema，去重 |
| `HasTool(name) / HandleToolCall(name, args)` | 路由到对应 provider |
| `InitializeAll(sessionID, kwargs)` | 初始化所有 provider |
| `ShutdownAll()` | 逆序关闭 |
| 生命周期 hooks | `OnTurnStart`, `OnSessionEnd`, `OnPreCompress`, `OnMemoryWrite`, `OnDelegation` |

**上下文隔离**：
```go
func BuildMemoryContextBlock(rawContext string) string
// 用 <memory-context> 标签包裹，防止模型误解为用户输入
```

### 1.6 memory tool schema

```go
var MemoryToolSchema = ToolSchema{
    Name:        "memory",
    Description: "Save durable information to persistent memory...",
    Parameters: map[string]any{
        "action":   enum["add", "replace", "remove"],
        "target":   enum["memory", "user"],
        "content":  string,
        "old_text": string,
    },
}
```

tool handler 函数：解析 action → 调 MemoryStore 对应方法 → 返回 JSON

### 1.7 文件结构

```
pkg/memory/
├── store.go              // MemoryStore（文件读写、CRUD、snapshot）
├── store_test.go
├── scanner.go            // 注入扫描（正则 + 隐形字符）
├── scanner_test.go
├── provider.go           // MemoryProvider 接口 + BaseMemoryProvider
├── builtin_provider.go   // BuiltinMemoryProvider
├── builtin_provider_test.go
├── manager.go            // MemoryManager（编排层）
├── manager_test.go
├── tool.go               // memory tool schema + handler
└── tool_test.go
```

### 1.8 测试要点

- [ ] 并发写入（多 goroutine 同时 Add）不丢数据
- [ ] 原子写中途 crash 不破坏文件（kill 进程后文件完整）
- [ ] Frozen snapshot 会话内不变
- [ ] 字符限制拒绝超额写入
- [ ] 注入扫描阻止恶意内容
- [ ] Replace/Remove 子串匹配逻辑（多匹配、无匹配）
- [ ] Provider 容错（一个 panic 不影响另一个）

---

## Phase 2：Holographic 插件 Go 化

> 目标：实现本地 SQLite 事实存储 + FTS5 全文搜索 + HRR 向量代数检索。
> 预估：1200-1500 行 Go，3-4 天

### 2.1 HRR 向量代数

**源码参考**：`plugins/memory/holographic/holographic.py`（204 行）

```go
package hrr

// 相位向量类型
type PhaseVector []float64

func EncodeAtom(word string, dim int) PhaseVector    // SHA-256 确定性生成
func Bind(a, b PhaseVector) PhaseVector              // (a + b) % 2π
func Unbind(memory, key PhaseVector) PhaseVector     // (memory - key) % 2π
func Bundle(vectors ...PhaseVector) PhaseVector      // 复数指数圆均值
func Similarity(a, b PhaseVector) float64            // mean(cos(a - b))
func EncodeText(text string, dim int) PhaseVector    // 分词 → atom → bundle
func EncodeFact(content string, entities []string, dim int) PhaseVector

func PhasesToBytes(phases PhaseVector) []byte         // float64 → bytes
func BytesToPhases(data []byte) PhaseVector           // bytes → float64
func SNREstimate(dim, nItems int) float64             // 容量预警
```

**Go 优势**：
- 不需要 numpy，`[]float64` 切片操作即可
- `math.Cos`, `math.Atan2` 标准库
- `crypto/sha256` + `encoding/binary` 实现确定性 atom 编码
- 性能可能优于 Python + numpy（避免解释器开销）

### 2.2 SQLite 存储引擎

**源码参考**：`plugins/memory/holographic/store.py`（575 行）

**Go SQLite 选择**：`modernc.org/sqlite`（纯 Go，无 CGO）或 `github.com/mattn/go-sqlite3`（CGO）

**数据库 Schema**（与 Python 版相同）：
```sql
facts          -- fact_id, content (UNIQUE), category, tags, trust_score, hrr_vector BLOB, ...
entities       -- entity_id, name, entity_type, aliases
fact_entities  -- fact_id, entity_id (多对多)
facts_fts      -- FTS5 虚拟表 (content, tags)
memory_banks   -- bank_name (UNIQUE), vector BLOB, dim, fact_count
-- 触发器：facts_ai, facts_ad, facts_au（自动同步 FTS5）
```

```go
type FactStore struct {
    mu           sync.RWMutex
    db           *sql.DB
    defaultTrust float64  // 默认 0.5
    hrrDim       int      // 默认 1024
}
```

**需实现的方法**：

| 方法 | 说明 |
|------|------|
| `AddFact(content, category, tags)` | INSERT + 去重 + 实体提取 + HRR 向量 + 重建 bank |
| `SearchFacts(query, category, minTrust, limit)` | FTS5 MATCH + retrieval_count++ |
| `UpdateFact(factID, ...)` | 部分更新 + trust clamp [0,1] + 重新提取实体 |
| `RemoveFact(factID)` | DELETE + 重建 bank |
| `ListFacts(category, minTrust, limit)` | 按 trust 排序浏览 |
| `RecordFeedback(factID, helpful)` | trust ±0.05/0.10 非对称调整 |
| `extractEntities(text)` | 正则提取（大写短语、引号内容、AKA） |
| `resolveEntity(name)` | 按 name/aliases 查找或新建 |
| `computeHRRVector(factID, content)` | 编码 + 存 BLOB |
| `rebuildBank(category)` | 聚合该 category 所有事实向量 |

### 2.3 检索引擎

**源码参考**：`plugins/memory/holographic/retrieval.py`（594 行）

```go
type FactRetriever struct {
    store         *FactStore
    halfLife      int       // 时间衰减半衰期（天），0=禁用
    ftsWeight     float64   // 0.4
    jaccardWeight float64   // 0.3
    hrrWeight     float64   // 0.3
    hrrDim        int
}
```

**6 种检索策略**：

| 方法 | 机制 | Go 实现要点 |
|------|------|------------|
| `Search(query)` | FTS5 宽召回 → Jaccard 重排 → HRR 相似度 → trust 加权 | 标准管线 |
| `Probe(entity)` | HRR unbind 代数提取 | `Unbind(factVec, Bind(entityVec, ROLE_ENTITY))` |
| `Related(entity)` | HRR 裸 atom 结构匹配 | 与 ROLE_ENTITY / ROLE_CONTENT 比较 |
| `Reason(entities)` | 多实体 HRR 交集 | min(各实体分数)，AND 语义 |
| `Contradict()` | 实体 Jaccard 高 + 内容 HRR 相似度低 | O(n²) 比较，限制 500 条 |
| `scoreFacts(targetVec)` | 向量相似度排序 | `Similarity(target, factVec)` |

**辅助函数**：
- `tokenize(text) → set[string]` — 空格分词 + 小写 + 去标点
- `jaccardSimilarity(a, b set) → float64`
- `temporalDecay(timestamp) → float64` — `0.5^(ageDays / halfLife)`

### 2.4 HolographicMemoryProvider

**源码参考**：`plugins/memory/holographic/__init__.py`（408 行）

```go
type HolographicProvider struct {
    memory.BaseMemoryProvider
    config    HolographicConfig
    store     *FactStore
    retriever *FactRetriever
    minTrust  float64
}
```

**Tool Schemas**（2 个）：
- `fact_store` — 9 个 action（add/search/probe/related/reason/contradict/update/remove/list）
- `fact_feedback` — 2 个 action（helpful/unhelpful）

**生命周期**：
- `Initialize()` → 打开 SQLite，创建 FactStore + FactRetriever
- `Prefetch(query)` → 调 `retriever.Search(query, limit=5)`，返回格式化结果
- `OnSessionEnd()` → 可选自动提取（正则匹配用户偏好/决策模式）
- `OnMemoryWrite()` → 镜像内置记忆的 add 操作到 fact_store
- `Shutdown()` → 关闭 SQLite 连接

### 2.5 文件结构

```
pkg/memory/holographic/
├── hrr.go                // HRR 向量代数（bind/unbind/bundle/similarity）
├── hrr_test.go           // 确定性编码、代数恒等式验证
├── store.go              // FactStore（SQLite + 实体提取 + 信任评分）
├── store_test.go
├── retrieval.go           // FactRetriever（6 种检索策略）
├── retrieval_test.go
├── provider.go            // HolographicMemoryProvider
├── provider_test.go
└── schema.sql             // 可选：嵌入的建表 SQL（go:embed）
```

### 2.6 测试要点

- [ ] HRR 代数恒等式：`Unbind(Bind(a, b), a) ≈ b`（相似度 > 0.9）
- [ ] `EncodeAtom` 跨运行确定性（相同输入 → 相同向量）
- [ ] FTS5 搜索基本功能（中英文、特殊字符）
- [ ] 实体提取正则覆盖（大写短语、引号、AKA）
- [ ] 信任评分边界：clamp [0, 1]，非对称调整
- [ ] `Contradict` 在 500+ 条时不超时
- [ ] `Reason` 多实体交集 AND 语义正确
- [ ] 并发读写 SQLite（WAL 模式）
- [ ] SNR 容量预警（dim=1024, n>256 时告警）
- [ ] Provider 注册到 MemoryManager 后端到端工作

---

## 依赖总结

| 依赖 | 用途 | Phase |
|------|------|-------|
| Go 标准库 (`os`, `sync`, `regexp`, `math`, `crypto/sha256`, `encoding/binary`, `encoding/json`) | 全部 | 1 & 2 |
| `syscall` | 文件锁 `Flock` | 1 |
| `modernc.org/sqlite` 或 `github.com/mattn/go-sqlite3` | SQLite + FTS5 | 2 |

**无其他外部依赖**。

---

## 里程碑

| 阶段 | 产出 | 预估 |
|------|------|------|
| Phase 1.1 | MemoryStore + 注入扫描 + 测试通过 | Day 1 |
| Phase 1.2 | MemoryProvider 接口 + BuiltinProvider | Day 1-2 |
| Phase 1.3 | MemoryManager + memory tool + 集成测试 | Day 2-3 |
| Phase 2.1 | HRR 向量代数 + 单元测试 | Day 3-4 |
| Phase 2.2 | FactStore（SQLite + FTS5 + 实体） + 测试 | Day 4-5 |
| Phase 2.3 | FactRetriever（6 种检索策略） + 测试 | Day 5-6 |
| Phase 2.4 | HolographicProvider + 集成到 MemoryManager + 端到端测试 | Day 6-7 |
