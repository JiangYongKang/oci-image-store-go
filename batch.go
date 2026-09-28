package ocistore

import (
	"context"
	"fmt"
)

// ResolvedPlatform 是批量解析中单个平台的结果。成功时 Err==nil；
// 某个平台失败只体现在该条目的 Err 上，不影响同批其它平台已解析好的结果。
type ResolvedPlatform struct {
	// Name 是该条目的稳定名字：命中平台为 os/arch[/variant]；
	// 单平台镜像（无平台描述）为根摘要前 12 位。
	Name string
	// Selector 是命中该条目所用的条件（仅“全部平台”模式为零值）。
	Selector PlatformSelector
	Info     PlatformInfo
	Image    *ResolvedImage
	// Layers 是该镜像清单声明的全部层（保序）；LayerCount 为层数；
	// SizeBytes 为各层描述符声明大小之和。
	Layers     []Descriptor
	LayerCount int
	SizeBytes  int64
	Err        error
}

// Succeeded 报告该平台是否解析成功。
func (p *ResolvedPlatform) Succeeded() bool { return p.Err == nil }

// BatchResult 是一次批量解析的完整结果：
// 引用指向单平台镜像清单时 IsIndex=false，Platforms 恰有一个条目。
type BatchResult struct {
	Root      Digest
	IsIndex   bool
	Platforms []ResolvedPlatform
}

// Failed 返回批量中解析失败的平台条目（保持结果顺序），
// 便于一次看清“哪些平台没解析成功”。
func (r *BatchResult) Failed() []ResolvedPlatform {
	if r == nil {
		return nil
	}
	var out []ResolvedPlatform
	for _, p := range r.Platforms {
		if p.Err != nil {
			out = append(out, p)
		}
	}
	return out
}

// resolveOneManifest 解析单个平台条目：取子清单 -> 拒绝嵌套索引 ->
// 校验 config 与全部层存在且完好 -> 汇总层与总大小。
// 任何失败都包装为该平台自身的错误，不留下半成品。
func (s *Store) resolveOneManifest(info PlatformInfo) (*ResolvedImage, []Descriptor, int64, error) {
	mdata, err := s.blobData(info.Descriptor.Digest)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("read manifest %s: %w", info.Descriptor.Digest, err)
	}
	isIndex, _, mani, err := parseManifest(info.Descriptor.MediaType, mdata)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("parse child %s: %w", info.Descriptor.Digest, err)
	}
	if isIndex {
		return nil, nil, 0, fmt.Errorf("%w: nested index at %s", ErrMediaType, info.Descriptor.Digest)
	}
	res := &ResolvedImage{Manifest: mani, ManifestDescriptor: info.Descriptor}

	var total int64
	for _, l := range mani.Layers {
		total += l.Size
	}
	// 校验引用内容存在且完好；任一失败即该平台失败。
	for _, rd := range mani.referencedDigests() {
		if _, herr := s.blobData(rd); herr != nil {
			return nil, nil, 0, fmt.Errorf("referenced content %s: %w", rd, herr)
		}
	}
	return res, mani.Layers, total, nil
}

// platformName 给出平台条目的稳定名字。
func platformName(p Platform, root Digest) string {
	if p.OS == "" && p.Architecture == "" && p.Variant == "" {
		h := root.Hex()
		if len(h) > 12 {
			h = h[:12]
		}
		return h
	}
	return p.String()
}

// resolveBatch 是批量解析的公共实现。
//
// 一致性语义（与标签迁移/删除/GC 并发时）：
//   - 进入时持有 gcmu 解析标签并读取、校验根清单，随后立即 Pin(root)；
//     “解析到根”与“建立保护”之间不存在能被 GC 插入的窗口；
//   - Pin 之后整棵可达子图（索引条目、子清单、config、层）都不会被 GC 回收，
//     标签随后被迁移或删除也不影响本次结果——结果锚定在根摘要上；
//   - 根清单原子发布、blobData 全量校验，读者不可能读到写了一半的内容；
//   - 单个平台失败仅记录在该条目上，不触碰其它条目，也不产生半成品。
func (s *Store) resolveBatch(ctx context.Context, ref string, selectors []PlatformSelector, all bool) (*BatchResult, error) {
	// 冻结窗口：解析标签 -> 读根 -> 建立 Pin，一步完成。
	s.gcmu.Lock()
	root, err := s.resolveRef(ref)
	if err != nil {
		s.gcmu.Unlock()
		return nil, err
	}
	data, err := s.blobDataGCLocked(root)
	if err != nil {
		s.gcmu.Unlock()
		return nil, err
	}
	if err := s.Pin(root); err != nil {
		s.gcmu.Unlock()
		return nil, err
	}
	s.gcmu.Unlock()
	defer func() { _ = s.Unpin(root) }()

	disc, err := discoverFromRoot(root, data)
	if err != nil {
		return nil, err // 根 JSON 非法、媒体类型不支持、索引条目摘要非法：清单本身有问题
	}
	out := &BatchResult{Root: root, IsIndex: disc.IsIndex}

	// 确定本次要处理的（平台条目, 对应条件）列表。
	type job struct {
		info PlatformInfo
		sel  PlatformSelector
		// matchErr 非空表示该条件在索引中无命中（逐平台失败，不连累其它条件）；
		// 索引整体合法性已在 discoverFromRoot 校验，这里只可能是 ErrPlatformNoMatch。
		matchErr error
	}
	var jobs []job
	switch {
	case disc.IsIndex && all:
		for _, info := range disc.Platforms { // 已确定性排序
			jobs = append(jobs, job{info: info})
		}
	case disc.IsIndex:
		for _, sel := range selectors {
			info, merr := SelectPlatform(disc.Index, sel)
			if merr != nil {
				jobs = append(jobs, job{info: PlatformInfo{Platform: platformFromSelector(sel)}, sel: sel, matchErr: merr})
			} else {
				jobs = append(jobs, job{info: info, sel: sel})
			}
		}
	default: // 单平台镜像：占位条目是否满足各条件由匹配规则判定
		only := disc.Platforms[0]
		if all {
			jobs = append(jobs, job{info: only})
		} else {
			for _, sel := range selectors {
				jobs = append(jobs, job{info: only, sel: sel})
			}
		}
	}

	for _, jb := range jobs {
		entry := ResolvedPlatform{
			Name:     platformName(jb.info.Platform, root),
			Selector: jb.sel,
			Info:     jb.info,
		}
		// 无命中或单平台镜像不满足该条件：记录为该平台的失败，不解析内容。
		switch {
		case jb.matchErr != nil:
			entry.Err = fmt.Errorf("platform %s: %w", selectorString(jb.sel), jb.matchErr)
			out.Platforms = append(out.Platforms, entry)
			continue
		case !all && !disc.IsIndex && !platformMatches(jb.info.Platform, jb.sel):
			entry.Err = fmt.Errorf("platform %s: %w", selectorString(jb.sel), ErrPlatformNoMatch)
			out.Platforms = append(out.Platforms, entry)
			continue
		}
		img, layers, total, rerr := s.resolveOneManifest(jb.info)
		if rerr != nil {
			entry.Err = fmt.Errorf("platform %s: %w", entry.Name, rerr)
		} else {
			entry.Image = img
			entry.Layers = layers
			entry.LayerCount = len(layers)
			entry.SizeBytes = total
		}
		out.Platforms = append(out.Platforms, entry)
	}
	return out, nil
}

// platformFromSelector 仅用于无命中条目展示：从条件拼一个平台身份。
func platformFromSelector(sel PlatformSelector) Platform {
	var p Platform
	if sel.OS != nil {
		p.OS = *sel.OS
	}
	if sel.Architecture != nil {
		p.Architecture = *sel.Architecture
	}
	if sel.Variant != nil {
		p.Variant = *sel.Variant
	}
	return p
}

// ResolveAllPlatforms 发现引用下的全部平台并一次性解析：
// 每个平台返回其镜像清单、全部层、层数与各层总大小；
// 单个平台失败只体现在该条目（见 BatchResult.Failed）。
// 引用为单平台镜像清单时，结果恰有一个平台条目。
func (s *Store) ResolveAllPlatforms(ctx context.Context, ref string) (*BatchResult, error) {
	return s.resolveBatch(ctx, ref, nil, true)
}

// ResolvePlatforms 按一批部分条件逐个择优（择优规则见 SelectPlatform）并解析。
// 条件顺序即结果顺序；某条件无命中或解析失败只产生该位置的失败条目，
// 不连累其它条件。
func (s *Store) ResolvePlatforms(ctx context.Context, ref string, selectors []PlatformSelector) (*BatchResult, error) {
	return s.resolveBatch(ctx, ref, selectors, false)
}
