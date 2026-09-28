# 存储布局、引用解析、配额与回收语义

本组件用本地文件系统模拟一个 OCI 镜像仓库，实现内容寻址存储、多架构索引解析、
标签迁移、并发拉取、配额与垃圾回收。只依赖 Go 标准库。

## 1. 磁盘布局

```
<root>/
├── blobs/sha256/<ab>/<64hex>   内容寻址 blob（只读，0444）
├── tags/<tag>                  标签文件；正文为 "sha256:...."（0644）
├── tmp/                        原子发布用临时目录（.put-*）
└── quarantine/<64hex>.<rand>   校验失败被隔离的损坏内容
```

要点：

- blob 路径由其摘要唯一确定（`sha256` 前两字节做分桶）。**相同内容永远落到同一路径，
  重复写入不产生第二份副本**。
- 所有发布动作（新建 blob、标签写入/迁移）都是 **"写临时文件 → fsync → chmod →
  rename → fsync 目录"**。rename 在同一文件系统内是原子的，因此：
  - 读者绝不会读到半写入的清单或层；
  - 标签迁移是一个原子的指针替换，读到的要么是旧版本要么是新版本；
  - 任何失败路径都删除临时文件，不留部分文件或临时状态。
- 重新 `Open` 时清理 `tmp/` 中上次崩溃残留的临时文件。

## 2. 内容寻址与完整性

- 摘要格式：`sha256:<64 位小写十六进制>`，其它算法或非法长度返回 `ErrDigestInvalid`。
- `PutBlob(declared, r)`：实际字节的 sha256 若与 `declared` 不一致，拒绝写入并返回
  可 `errors.Is(., ErrDigestMismatch)` 的错误（`*DigestError` 同时给出声明/实际摘要）。
  `declared` 留空表示由存储计算。
- **每一条读取路径都重新校验**（`GetBlob`、`Has`、`FetchPlatform`、GC 建图）：
  磁盘字节与摘要不符时：
  1. 返回 `ErrCorrupted`（与"不存在" `ErrNotExist` 可区分）；
  2. 立即把该 blob **原子移入 `quarantine/`**，使其永远不可能再作为缓存命中；
  3. 后续读取按"不存在"处理。

## 3. 清单格式与引用解析规则

支持的媒体类型（其它显式声明的类型一律 `ErrMediaType` 拒绝，绝不忽略）：

| 角色 | OCI | Docker 兼容 |
|---|---|---|
| 索引 | `application/vnd.oci.image.index.v1+json` | `application/vnd.docker.distribution.manifest.list.v2+json` |
| 镜像清单 | `application/vnd.oci.image.manifest.v1+json` | `application/vnd.docker.distribution.manifest.v2+json` |

解析规则：

1. JSON 使用严格解码：截断、非法 JSON、尾部多余值均返回 `ErrManifestBadJSON`，
   不做任何"猜测式修复"。
2. 媒体类型缺失（OCI 允许省略）时按结构确定性嗅探：含 `manifests` 为索引，
   含 `config` 为镜像清单；两者都没有返回 `ErrMediaType`。
3. **额外字段 / 未来字段被保留**（`ImageManifest.Unknown` / `ImageIndex.Unknown`），
   兼容上游演进，但不参与解析结论，因此结论始终确定。
4. `FetchPlatform(ref, platform)`：
   - `ref` 为 `sha256:...` 按摘要定位，否则按标签定位；
   - 根内容是索引时，按 `os/architecture/variant` **严格相等**选择条目；
   - 找不到平台返回 `ErrPlatformNoMatch`，**绝不静默回退到其它平台**；
   - 索引中存在摘要非法的条目返回 `ErrPlatformBad`；
   - 子内容仍是索引（嵌套索引）返回 `ErrMediaType`；
   - 选定镜像清单后，继续校验其 `config` 与所有 `layers` 存在且完好，
     任一损坏/缺失都使整次拉取失败。

## 4. 多平台发现、条件筛选与批量解析

面向镜像预热/同步场景，在单平台 `FetchPlatform` 之上提供一组只读 API：

| API | 作用 |
|---|---|
| `DiscoverPlatforms(ctx, ref)` | 列出引用包含的**全部平台描述** |
| `SelectPlatforms(ctx, ref, q)` | 按**部分条件**挑平台，返回全部命中 |
| `BatchResolve(ctx, ref, qs)` | 一次按多组条件准备各平台清单与层 |
| `ResolveAllPlatforms(ctx, ref)` | `BatchResolve` 的"全部平台"快捷方式 |

### 4.1 发现（DiscoverPlatforms）

- 引用是**多架构索引**（`Kind="index"`）：每个 `manifests` 条目对应一个
  `PlatformEntry`，基础信息（`os`/`architecture`）与可选信息（`variant`、
  `os.version`、`os.features`、`features`）原样呈现。
- 引用本身是**单个平台镜像清单**（`Kind="manifest"`）：返回**恰好一条**
  entry——其 `Platform` 为零值、`Descriptor` 指向根清单自身。单平台不是
  错误情形，调用方不需要靠错误类型去猜"这到底是不是索引"。
- **顺序稳定可复现**：索引来源严格按 `manifests` 数组的内容定义顺序返回；
  索引是内容寻址的不可变字节，同一摘要永远得到同一顺序（标签迁移不影响）。
- 索引中存在摘要非法的条目：返回 `ErrPlatformBad`（清单本身有问题）；
  根内容截断/非法 JSON、媒体类型不支持、损坏/缺失分别返回既有对应错误。

### 4.2 条件筛选（PlatformSelector）

用链式 setter 构造条件，**哪些维度被显式设置，哪些才构成约束**：

```go
q := ocistore.NewPlatformSelector().
    WithOS("linux").                 // 只约束系统
    WithArchitecture("arm64").
    WithVariant("v8").
    WithOSVersion("8.0").
    WithOSFeatures("foo").           // 候选必须含 foo；含 foo+bar 也算命中
    WithFeatures("custom")
```

条件语义：

- **未设置（没调用对应 setter）的维度完全不参与相等判断**。只给
  `WithOS("linux")` 时，`linux/amd64` 与 `linux/arm64/v8` 都会命中。
- **候选在未被约束的维度上多写信息不会被排除**：没给 `variant` 时带
  `variant=v8` 的候选照样命中；特性维度采用**子集合包含**——条件要求的每
  一个特性都必须出现，但候选携带额外特性不影响命中。
- 显式设置为空串（如 `WithVariant("")`）表示"要求该维度恰好为空"，与
  "未设置"不同，用于需要精确语义的调用方。
- 空条件（`NewPlatformSelector()` 或传 `nil`）匹配全部平台。

**多个命中时的择优规则（确定性，不随机）**：`SelectPlatforms` 不做二选一，
而是按发现顺序（即索引内容定义顺序）返回**全部**命中，每项带 `Index`；
调用方需要唯一结果时按 `Index` 取第一个即可，规则简单且可解释。
`BatchResolve` 则取多组条件命中平台的**并集**。

**两类"没结果"严格分开，绝不混成一种失败**：

| 情形 | 表现 |
|---|---|
| 清单本身有问题（根缺失/损坏、JSON 非法、媒体类型不支持、条目摘要非法） | 返回 error，可 `errors.Is` 判定 |
| 清单合法但条件一个都没命中 | `err == nil`，命中切片为空（批量里记为 `Unmatched` 条件下标） |

### 4.3 批量解析（BatchResolve / ResolveAllPlatforms）

- 输入一组 `PlatformSelector`：先发现，再对每组条件取命中平台的**并集**，
  按清单摘要**去重**（同一份子清单即使被多组条件命中也只准备一次），
  保持发现顺序。
- 每个平台独立准备：读取并校验子清单（嵌套索引按 `ErrMediaType` 拒绝）、
  校验其 config 与全部层存在且完好，并汇总 `LayerCount` 与 `TotalSize`
  （各层 `Descriptor.Size` 之和）。
- **某个平台失败不连累其它平台**：失败记录在该 `PlatformImage.Err`，
  且失败项不暴露半成品（`Manifest`/`Layers` 为空）；成功项保证 config 与
  每层都已通过完整性校验。批量过程全程只读，不发布任何临时内容。
- `BatchResult.Unmatched`（升序的条件下标）与 `PlatformImage.Err` 表达两类
  不同问题：前者是"条件没筛中"，后者是"筛中了但内容没准备好"。

### 4.4 与标签迁移 / 删除 / GC 并发时的一致性

批量解析分四个阶段，保证基于一个自洽快照、且不会读到半成品：

1. **发现**：先把 `ref`（标签或摘要）解析为根摘要。标签迁移/删除只改动名字
   指针，根摘要一旦确定就不再受标签影响；内容寻址字节不可变。
2. **受保护窗口内打 Pin**：在与 GC/发布互斥的窗口（`gcmu`，锁顺序与 GC
   一致为 `gcmu -> pinmu`）内确认根仍完好并把根 `Pin` 计数加一。此后：
   - GC 的引用图包含"被 Pin 的根 → 索引 → 子清单 → config/层"整条边，
     **解析链上的任何内容都不会被回收**，不会出现"清单在、层被删"；
   - blob/标签发布与 GC 互斥，批量不可能观察到写了一半的清单或层。
   - 若在打 Pin 前根已被回收（无标签且无 Pin 的协议边界，见第 5 节），
     立即返回可 `errors.Is(., ErrNotExist)` 的根级错误，不进入准备阶段。
3. **逐平台准备**：每平台错误隔离（见 4.3）。
4. **释放 Pin**：`defer Unpin(根)`，成功与失败路径都释放，不泄漏保护计数；
   Unpin 之后内容才可按可达性被后续 GC 回收。

注意：这层 API 只提供只读的"发现/筛选/准备"，不负责下载缺失内容；子清单或
层缺失时该平台返回包装了 `ErrNotExist`/`ErrCorrupted` 的错误，由上层拉取
流程（配合既有 `Pin/Unpin` 协议）补齐。

### 4.5 兼容范围

- 与 `FetchPlatform` 使用同一套清单解析与校验逻辑：OCI/Docker v2 媒体类型、
  无 `mediaType` 时的结构嗅探、额外字段保留、严格 JSON、嵌套索引拒绝，
  行为完全一致，原有单平台解析不受影响。
- 单平台镜像清单在三个 API 中都有统一表示（`Kind="manifest"` + 一条 entry）；
  对单平台清单使用非空条件时，因其平台为零值，仅空条件（匹配全部）能命中，
  非空条件零命中——这与"未设置的维度不参与比较"的规则一致。

## 5. 标签语义

- 标签只是一个"名字 → 摘要"的指针文件，可任意迁移到不同内容。
- 标签迁移**不复制、不修改、不删除**任何 blob：旧版本仍可用其摘要精确定位
  （被摘要固定的内容不会因标签迁移而改变或丢失）。
- `DeleteTag` 只删除指针，不删除内容；内容由后续 GC 按可达性回收。
- 标签名需匹配 `^[A-Za-z0-9_][A-Za-z0-9._:@-]{0,127}$`（允许 `name:tag`，
  禁止路径分隔符与目录穿越），非法返回 `ErrTagInvalid`。

## 6. 引用计数与垃圾回收

引用图：

- **根（roots）**：每个标签指向的摘要。
- **边（edges）**：索引 → 其中的镜像清单；镜像清单 → 其 config 与 layers。

`GC` 采用 mark-sweep：

1. 冻结变更窗口（持有 `gcmu`，与 blob 发布、标签增改互斥），扫描磁盘构建快照；
   扫描即校验，损坏内容当场隔离且不参与引用判定。
2. 以"标签根 + 所有 Pin 根"为起点沿边做可达性标记。
3. 删除所有不可达 blob（按摘要名升序，逐个删除，单个失败不影响其余）。
4. `RefCount(d)` 返回指向某 blob 的标签数与引用它的上层清单数之和。

**与并发拉取的协作（避免悬空引用 / 误删 / 数据复活）**：

- 拉取方在获取内容**之前**对根摘要（以及解析链上的依赖）调用 `Pin`，
  完成后 `Unpin`。GC 期间 Pin 集合被冻结快照，被 Pin 的整段子图都不会被回收。
- GC 与发布互斥，因此"清单刚发布、层正要发布"这类半成品图不会被 GC 观察到，
  也不会出现清单保留而层被删的悬空引用。
- 已被 GC 删除的内容没有任何残留标签/清单指向它，旧引用不可能让它"复活"。
- 注意协议边界：**没有标签、也没有 Pin 的内容，在任何时刻都可能被回收**。
  上层必须遵循"先 Pin / 先建标签引用，再写依赖"的拉取顺序。

## 7. 配额语义

- `Options.QuotaBytes > 0` 启用配额，`<= 0` 表示不限。
- 配额统计的是 `blobs/` 下唯一内容的实际字节数（标签文件不计入）。
- 写入前在同一把变更锁内计算 `已用 + 待写`；超限返回
  `ErrQuotaExceeded`（错误文本含 used/need/quota），**不执行发布**，
  因此不会留下部分文件，已有数据继续可读。
- 去重命中（相同内容再次写入）不重复计费、不重复占空间。

## 8. 并发模型

- `Store` 并发安全。
- 同内容并发 `PutBlob` 通过内部 singleflight 合并为一次落盘，其余调用共享结果。
- `mu` 保护配额/发布一致性，`gcmu` 串行化 GC 与发布、标签变更，`pinmu` 保护 Pin 计数。
- 测试在 `-race` 下覆盖：同内容并发写、迁移期间并发读、共享层多清单并发写、
  30 轮"并发拉取 vs 删标签 + GC"竞态，以及多平台批量解析与标签迁移/删除/
  GC 的真并发竞态（悬空引用、半成品、保护泄漏均被断言）。
