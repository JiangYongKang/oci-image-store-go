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

## 4. 标签语义

- 标签只是一个"名字 → 摘要"的指针文件，可任意迁移到不同内容。
- 标签迁移**不复制、不修改、不删除**任何 blob：旧版本仍可用其摘要精确定位
  （被摘要固定的内容不会因标签迁移而改变或丢失）。
- `DeleteTag` 只删除指针，不删除内容；内容由后续 GC 按可达性回收。
- 标签名需匹配 `^[A-Za-z0-9_][A-Za-z0-9._:@-]{0,127}$`（允许 `name:tag`，
  禁止路径分隔符与目录穿越），非法返回 `ErrTagInvalid`。

## 5. 引用计数与垃圾回收

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

## 6. 配额语义

- `Options.QuotaBytes > 0` 启用配额，`<= 0` 表示不限。
- 配额统计的是 `blobs/` 下唯一内容的实际字节数（标签文件不计入）。
- 写入前在同一把变更锁内计算 `已用 + 待写`；超限返回
  `ErrQuotaExceeded`（错误文本含 used/need/quota），**不执行发布**，
  因此不会留下部分文件，已有数据继续可读。
- 去重命中（相同内容再次写入）不重复计费、不重复占空间。

## 7. 并发模型

- `Store` 并发安全。
- 同内容并发 `PutBlob` 通过内部 singleflight 合并为一次落盘，其余调用共享结果。
- `mu` 保护配额/发布一致性，`gcmu` 串行化 GC 与发布、标签变更，`pinmu` 保护 Pin 计数。
- 测试在 `-race` 下覆盖：同内容并发写、迁移期间并发读、共享层多清单并发写、
  以及 30 轮"并发拉取 vs 删标签 + GC"竞态。

## 8. 多平台发现、按条件筛选与批量解析

面向镜像预热/同步场景新增一层能力，三个入口都在原有内容寻址与校验语义之上，
不放宽任何严格性：

- `Discover(ctx, ref)`：发现一个标签/摘要下到底有哪些平台；
- `SelectPlatform(idx, sel)`：按“部分条件”在索引里择优挑一个平台（纯函数，便于单测/复用）；
- `ResolveAllPlatforms(ctx, ref)` / `ResolvePlatforms(ctx, ref, sels)`：
  一次把一个或多个平台的镜像清单与层全部准备好。

### 8.1 发现（Discover）

- `ref` 为 `sha256:...` 按摘要定位，否则按标签定位。
- 引用是**多架构索引**时：`DiscoverResult.IsIndex=true`，`Platforms` 列出每个带
  `platform` 描述的条目，包含系统、架构以及变体（variant）、系统版本（os.version）、
  系统特性（os.features）、架构特性（features）等可选信息。
- 引用是**单个平台镜像清单**时：不报错，返回 `IsIndex=false` 且 `Platforms` 恰好
  一个占位条目——其 `Descriptor` 指向清单本身（带媒体类型/大小/摘要），`Platform`
  为零值（单清单本身不携带独立平台描述）。这样调用方拿到的始终是统一结构，
  不必靠猜错误来区分“索引 vs 单镜像”。
- **顺序稳定可复现**：平台按规范键排序，键的比较顺序为
  `os → architecture → variant → os.version → os.features(排序后) → features(排序后) → 条目摘要`；
  键完全相同再回退到原始索引位置。标签引用与摘要引用得到的顺序一致。
- 索引中存在摘要非法的条目，返回 `ErrPlatformBad`（清单本身有问题）。
- 没有 `platform` 字段的条目（例如 attestation/证明材料）不计入平台清单。

### 8.2 条件语义（PlatformSelector）

`PlatformSelector` 的每个维度都是“显式给出才算约束”：

| 维度 | 构造器 | 不给（不调用） | 给出时 |
|---|---|---|---|
| 系统 OS | `WithOS` | 不参与比较 | 必须相等 |
| 架构 Architecture | `WithArchitecture` | 不参与比较 | 必须相等 |
| 系统版本 OSVersion | `WithOSVersion` | 不参与比较 | 必须相等 |
| 变体 Variant | `WithVariant` | 不参与比较 | 必须相等 |
| 系统特性 OSFeatures | `WithOSFeatures(...)` | 不参与比较 | 候选须**包含全部**给定特性 |
| 架构特性 Features | `WithFeatures(...)` | 不参与比较 | 候选须**包含全部**给定特性 |

要点：

- **没给的维度一律不参与相等判断**。例如只给 `WithOS("linux")` 时，架构、变体、
  系统版本等完全不比较；给了 `os+arch` 但没给 `variant`，则 v7/v8 候选都算命中。
- 特性是**子集语义**：要求 `sse4` 时，携带 `sse4+avx` 的候选仍然命中——
  “候选多写了几个没被要求的信息”不会因此被排除。
- 空选择器 `PlatformSelector{}` 表示不加任何条件（全部候选都满足）。

### 8.3 多命中时的确定性择优

`SelectPlatform` 在多个候选同时满足条件时，按以下顺序确定唯一结果，绝不随机：

1. **额外信息量少者优先**：候选在“未被条件约束的可选维度”上携带的信息越少越靠前。
   `variant`、`os.version` 非空各计 1；未要求的 `os.features`/`features` 按多出的
   个数计（OS/架构属身份维度，不计）。
   例：只要 `arch=arm64`、不给变体时，朴素的 `arm64/v8`（surplus=0）胜过
   带了 `os.version+os.features` 的 `arm64/v7`（surplus=2）。
2. **规范键字典序**：额外量相同则按 8.1 的规范键比较（os/arch/variant/.../摘要）。
3. **原始位置**：仍相同则取索引中更靠前的条目。

两类失败严格分开，可用 `errors.Is` 判断：

- 条件筛不到任何候选 → `ErrPlatformNoMatch`；
- 清单本身有问题（条目摘要非法等）→ `ErrPlatformBad`。

### 8.4 批量解析（ResolveAllPlatforms / ResolvePlatforms）

- `ResolveAllPlatforms`：解析发现到的**全部**平台（顺序同 Discover）。
- `ResolvePlatforms(ref, sels)`：按一批条件**逐个择优**解析，结果顺序与请求一致，
  允许重复条件。
- 每个平台条目 `ResolvedPlatform` 给出：稳定名字（`os/arch[/variant]`；单镜像为
  根摘要前 12 位）、命中的描述符与平台、镜像清单、`Layers`（保序）、`LayerCount`、
  `SizeBytes`（各层声明大小之和）。
- **逐平台隔离失败**：某个平台子清单缺失/损坏/是嵌套索引/条件无命中，只在该条目的
  `Err` 上体现（可用 `BatchResult.Failed()` 一次收集），同批其它平台结果照常可用；
  顶层 `error` 只用于“整批无法开始”的情况（标签不存在 `ErrLabelNotExist`、
  根内容缺失/损坏、根 JSON 非法、索引条目摘要非法）。
- 单平台镜像：`BatchResult.IsIndex=false`，结果恰有一个条目；用带约束的条件去
  `ResolvePlatforms` 时会得到该位置的 `ErrPlatformNoMatch`（占位条目没有平台描述），
  空条件则正常解析。

### 8.5 与标签迁移 / 删除 / GC 并发时的一致性

批量解析在内部遵循与手动拉取相同的保护协议：

1. 在 `gcmu` 冻结窗口内完成“解析标签 → 读取并校验根清单 → `Pin(root)`”，
   因此“已解析到根但还没建立保护”这种能被 GC 插入的窗口不存在。
2. Pin 之后，该根可达的整棵子图（索引条目、子清单、config、层）都不会被 GC 回收；
   标签随后被迁移或删除也不影响本次结果——**结果锚定在根摘要上**，
   不会出现“根来自版本 A、条目来自版本 B”的串版本读取。
3. 根清单与 blob 都经原子发布 + 全量校验，读者不可能读到写了一半的内容。
4. 解析结束统一 `Unpin(root)`；单个平台失败不影响保护范围，也不留半成品。

兼容范围与 `FetchPlatform` 一致：OCI 与 Docker v2 清单/索引媒体类型、省略媒体类型时
的结构嗅探、额外字段容忍；显式未知媒体类型、嵌套索引、截断 JSON 仍分别以
`ErrMediaType` / `ErrManifestBadJSON` 拒绝。

