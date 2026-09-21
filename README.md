# ocistore — 本地 OCI 镜像内容存储与引用解析

用本地文件系统模拟镜像仓库的 Go 组件，提供内容寻址存储、多架构清单解析、
标签迁移、并发安全拉取、完整性校验、引用计数式垃圾回收与存储配额。

- Module：`github.com/highcumontoa/oci-image-store-go`
- 仅依赖 Go 标准库；摘要算法为 `sha256`。

---

## 1. 存储布局

以 `Open(root, Options{QuotaBytes: ...})` 打开的目录结构如下：

```
<root>/
├── blobs/
│   └── sha256/
│       ├── <hex>                 # blob 本体，内容寻址，落盘后只读(0444)
│       └── <hex>.meta            # blob 元数据 JSON：mediaType / size
├── tags/
│   └── t<hex(tag-name)>          # 标签指针 JSON：{digest, mediaType, size}
├── leases/                       # 外部持久租约（每行一个 digest，可选）
├── quarantine/
│   └── sha256-<hex>              # 校验失败被隔离的内容
├── tmp/                          # 写入过程临时文件（崩溃后重开自动清理）
└── state.json                    # 已用字节数（可由 blobs/ 重建并纠正）
```

关键性质：

- **blob 路径即摘要**：`blobs/sha256/<hex>`。文件先完整写入 `tmp/`，
  经摘要校验与配额检查后，通过 `fsync + rename` **原子发布**到最终路径，
  读取方永远不会观察到半写入内容。
- **标签与内容分离**：标签只是一个指向摘要的小 JSON 指针。标签迁移只改
  指针文件，绝不修改或删除摘要固定的 blob。
- **元数据一致性**：`.meta` 与本体大小不符即判定为损坏（`presenceCorrupt`），
  不会被当作“不存在”而静默忽略。

---

## 2. 引用与解析规则

### 2.1 引用形式

- 标签：任意非空、不含 NUL 的名称（如 `app:v1`、`latest`）。
- 摘要：严格的 `sha256:<64-hex>`，由 `ParseDigest` 校验算法与编码。

`GetManifest(ref)` / `ResolvePlatform(ref, plat)` 同时接受标签与摘要；
显式按摘要写入时，**声明摘要必须等于实际内容摘要**，否则返回
`ErrDigestMismatch`。

### 2.2 多架构索引解析

`ResolvePlatform` 的解析链：

1. 解析起点（标签 -> 当前指针，或直接是摘要），读取并校验索引/清单；
2. 起点若为单平台清单，直接返回；
3. 起点若为索引，按 **OCI 平台匹配规则** 从 `manifests[]` 中精确选择：
   - `architecture`、`os` 必须相等（空字段表示不约束）；
   - 请求给出的 `variant`、`os.version` 必须相等；
   - 请求的 `features` / `os.features` 必须被候选全部包含；
   - **绝不静默回退**到其它平台（例如 `arm64/v7` 不会回退到 `v8`）。
4. 对选中的平台清单逐跳做“声明摘要/长度 == 实际内容”校验后才返回。

### 2.3 可区分的失败原因

所有错误都可用 `errors.Is` 判类，用 `errors.As` 取细节：

| 哨兵错误 | 触发场景 | 细节类型 |
| --- | --- | --- |
| `ErrNotFound` | 标签/摘要不存在、Pin 缺失内容 | — |
| `ErrDigestMismatch` | 声明摘要与实际不符、内容被截断/篡改 | `*DigestMismatchError{Op,Declared,Actual}` |
| `ErrManifestTruncated` | 清单 JSON 截断、尾随垃圾、子描述符非法 | — |
| `ErrPlatformNotFound` | 索引中没有匹配平台 | `*PlatformNotFoundError{Wanted,Available}` |
| `ErrUnsupportedMediaType` | 未知/不受支持的清单媒体类型 | `*UnsupportedMediaTypeError{MediaType}` |
| `ErrQuotaExceeded` | 写入会超过配额 | `*QuotaExceededError{Quota,Used,Required}` |
| `ErrInvalidRef` | 标签名/摘要格式非法 | — |
| `ErrLeaseReleased` | 对已释放租约再 Pin | — |

### 2.4 上游清单格式兼容

- 接受 OCI（`application/vnd.oci.image.index.v1+json`、
  `...manifest.v1+json`）与 Docker v2（`manifest.list.v2+json`、
  `manifest.v2+json`）。
- JSON 中的**未知额外字段一律容忍**，不影响已知字段解析结论。
- **未知媒体类型明确拒绝**（`ErrUnsupportedMediaType`），不会被忽略或猜测。
- 采用严格 JSON 解码：截断 JSON、顶层文档之后的尾随数据都按
  `ErrManifestTruncated` 拒绝。

---

## 3. 完整性校验

- 所有读取（`GetBlob` / `GetManifest` / 解析链）都基于内容寻址复核：
  - 入口处比对 `.meta` 声明长度与磁盘大小（捕获截断）；
  - 读到 EOF 时对流式内容做 SHA256，比对声明摘要（捕获等长篡改）。
- 一旦判定损坏：
  1. 立即返回 `ErrDigestMismatch`；
  2. 将 blob **移动到 `quarantine/`**，使其不再按原摘要命中缓存
     （`Exists`/`Stat`/`Get` 都不会再返回它）；
  3. 重新写入正确内容可“修复”该摘要（原子覆盖落盘）。

---

## 4. 并发语义

组件内部有两层同步：

- `commitMu`：串行化 blob 的“去重检查 → rename → 配额记账”提交区间。
  并发写入**相同内容只有一份落盘**，用量只计一次；字节落地（spool）在
  锁外进行，不牺牲吞吐。
- “常规操作 / GC”门闩（`gcMu` + 条件变量）：
  - 常规读写（Put/Get/Tag/Resolve/Pin）持共享读门闩，彼此并行；
  - `GetBlob` 返回的 `ReadCloser` 在 **Close 前持续持有门闩**，覆盖整个
    流式读取区间，GC 不会在读取途中删除 blob；
  - GC 先登记“排空意图”，等待所有在途操作完成（等待期间释放门闩让读者
    收尾，避免丢失唤醒/自死锁），再取排他锁执行回收。

由此保证：并发写同一标签/同一层结果一致；无半写入清单；读者要么看到
完整旧版要么看到完整新版；GC 与并发拉取不会产生悬空引用、误删或数据复活。

### 租约（Lease）

`ResolvePlatform` 内部创建租约并 Pin 解析链路上的内容，返回的
`ResolvedManifest.Release()` 之前这些内容受 GC 保护；调用方应在拉完
config/layers 后再 `Release()`。也可用 `Store.NewLease()` 手动固定内容。

---

## 5. 配额与垃圾回收语义

### 5.1 配额

- `Options.QuotaBytes` 为 0 表示不限；否则仅统计 blob 本体字节
  （`.meta`、标签、状态文件不计入）。
- 提交采用“**预订 → 落盘 → 确认/回滚**”：在状态锁内先检查并预留配额，
  任何落盘步骤失败都回滚预留并删除临时文件。
- 超限时返回 `ErrQuotaExceeded`，**已有数据保持可用**，不遗留临时文件、
  不泄漏配额计数。
- 去重写入（相同内容）不额外占用配额，即使配额已满也幂等成功。

### 5.2 垃圾回收（标记—清除）

`GC()` 以以下集合为**可达根**做闭包追踪：

1. 所有标签指针指向的内容；
2. 进程内所有未释放 `Lease` Pin 的内容；
3. `leases/` 中的持久租约（可选）。

从根沿清单引用展开：索引 → 子清单；镜像清单 → config + 各 layer。
不在可达集合中的 blob（连同 `.meta`）被删除并回收费量。

- 删除标签**不会**立即删内容；只有不再被**任何**清单/根引用时才回收。
- 被多个清单共享的层，在仍有任一清单可达时保留。
- GC 与拉取互斥，活跃租约固定的内容不回收；租约释放后再次 GC 才回收。
- 已删除内容不会因残留指针“复活”：对已删摘要 `Tag`/读取返回
  `ErrNotFound`；只有重新写入相同内容才能合法地重新引入该摘要。
- GC 中若遇到无法解析的清单，会显式报错而非猜测引用闭包，避免误删。

---

## 6. 本地验证方法

```bash
# 构建与静态检查
go build ./...
go vet ./...

# 全量单元测试
go test ./... -v

# 竞态检测 + 重复执行（并发/回收竞态用例）
go test ./... -race -count=10
```

测试覆盖（均打印“输入 / 判定依据 / 最终状态”日志，用 `-v` 查看）：

- 重复写入去重、标签迁移、摘要固定内容不变、标签生命周期；
- 摘要不匹配（声明错误、等长篡改、截断）、损坏隔离与重写修复；
- 平台精确选择、平台缺失拒绝、变体不回退、子清单摘要不符、截断清单；
- 未知媒体类型拒绝、额外字段容忍、Docker v2 兼容、尾随 JSON 拒绝；
- 配额边界、超限拒绝且保留已有数据、失败无临时残留、GC 回收配额；
- 并发写同一 blob/同一标签、并发读者无半写入、GC 与并发拉取竞态；
- 租约防 GC、租约释放语义、数据不复活、关闭重开持久化。

---

## 7. 最小使用示例

```go
s, _ := ocistore.Open("./.cache/oci", ocistore.Options{QuotaBytes: 512 << 20})
defer s.Close()
ctx := context.Background()

// 写入多架构镜像并打标签
idxDesc, _ := s.PutManifest(ctx, "app:v1", ocistore.MediaTypeOCIIndex, indexJSON)

// 解析到具体平台（租约保护解析链路）
rm, err := s.ResolvePlatform(ctx, "app:v1", ocistore.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"})
if err != nil { /* errors.Is 区分 ErrPlatformNotFound / ErrDigestMismatch / ... */ }
defer rm.Release()

for _, layer := range rm.Layers {
    rc, _, err := s.GetBlob(ctx, layer.Digest) // 读取期完整性校验
    // ... io.Copy 后 rc.Close()
}

// 删除标签后，不可达内容在 GC 时才回收
_ = s.DeleteTag(ctx, "app:v1")
report, _ := s.GC(ctx)
```
