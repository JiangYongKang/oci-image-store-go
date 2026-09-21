# oci-image-store-go

本地文件系统模拟的 OCI 镜像内容存储与引用解析组件。支持：

- 按标签 / 摘要写入与读取；**相同内容去重**（一份副本）；标签可原子迁移，
  被摘要固定的内容不随标签迁移改变或丢失。
- 多架构索引 → 平台清单的**严格**解析；缺平台、清单截断、摘要不符分别以
  可区分错误拒绝，不回退其它平台。
- 内容寻址完整性校验：任何读取路径摘要不符都报错，并把损坏内容隔离，
  不再作为缓存命中。
- 并发安全（singleflight 去重、原子 rename 发布），竞态检测测试通过。
- 标签根 + 清单引用边的可达性 GC，配合 `Pin/Unpin` 支持并发拉取，
  无悬空引用、误删、数据复活。
- 可配置字节配额，超限明确拒绝且不留临时状态，已有数据保持可用。
- 兼容 OCI 与 Docker v2 媒体类型、容忍额外字段；未知媒体类型明确拒绝。

仅依赖 Go 标准库（Go 1.25+）。

## 快速上手

```go
package main

import (
    "bytes"
    "context"
    "fmt"

    "github.com/highcumontoa/oci-image-store-go"
)

func main() {
    ctx := context.Background()
    s, err := ocistore.Open(ocistore.Options{
        RootDir:    "./.oci-store",
        QuotaBytes: 512 << 20, // 0 表示不限
    })
    if err != nil { panic(err) }
    defer s.Close()

    // 1) 写 blob（声明摘要；留空则由存储计算）
    data := []byte("...config/layer/manifest bytes...")
    d := ocistore.ComputeDigest(data)
    d, size, err := s.PutBlob(ctx, d, bytes.NewReader(data))

    // 2) 给清单打标签（之后可迁移到别的摘要）
    _ = s.PutTag(ctx, "myrepo/app:v1", d)
    cur, _ := s.ResolveTag("myrepo/app:v1")

    // 3) 多架构：从标签解析到具体平台（严格匹配，不回退）
    img, err := s.FetchPlatform(ctx, "myrepo/app:v1",
        ocistore.Platform{OS: "linux", Architecture: "amd64"})
    fmt.Println(img.Manifest.Layers)

    // 4) 并发拉取保护：先 Pin 再取，完成后 Unpin
    _ = s.Pin(cur)
    defer s.Unpin(cur)

    // 5) 删标签 + 垃圾回收
    _ = s.DeleteTag(ctx, "myrepo/app:v1")
    res, _ := s.GC(ctx) // 仅回收不可达且未 Pin 的 blob
    fmt.Println(res.Removed, res.Bytes)
}
```

## 错误分类

全部可用 `errors.Is` 判定，互不混淆：

| 错误 | 触发场景 |
|---|---|
| `ErrDigestInvalid` | 摘要格式非法 |
| `ErrDigestMismatch` | 写入时声明摘要 ≠ 实际内容（`*DigestError` 给出两者） |
| `ErrCorrupted` | 读取/GC 时磁盘字节 ≠ 摘要，内容被隔离 |
| `ErrNotExist` | 按摘要读取的内容不存在（或已被隔离/回收） |
| `ErrLabelNotExist` | 解析/删除一个不存在的标签 |
| `ErrTagInvalid` | 标签名非法 |
| `ErrManifestBadJSON` | 清单截断或不是合法 JSON |
| `ErrMediaType` | 显式声明了不支持的媒体类型 / 嵌套索引 / 无法推断类型 |
| `ErrPlatformNoMatch` | 索引中无请求平台（不回退） |
| `ErrPlatformBad` | 索引条目摘要非法等坏描述符 |
| `ErrQuotaExceeded` | 写入将使占用超过配额 |

详细设计见 [`docs/STORAGE.md`](docs/STORAGE.md)。

## 本地验证

```bash
# 全量单测（含重复写入、标签迁移、摘要不匹配、平台缺失、截断、
# 未知媒体类型、配额、损坏隔离、并发读写、GC 与拉取竞态）
go test ./... -v

# 竞态检测 + 重复执行以稳定复现/排除竞态
go test -race ./...
go test -race -count=10 ./...

# 静态检查
go vet ./...
```

测试日志对关键用例打印三要素：**输入**（内容/摘要/平台/操作）、
**判定依据**（计数、选中的描述符、错误分类）、**最终状态**（blob 数、字节数、
标签解析结果、隔离/回收结果）。
