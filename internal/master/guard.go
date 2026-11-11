package master

import "sort"

// DeleteGuard 实现删除保护（设计文档 §7）：
//   - 一张表要连续两次完整拉取都缺失才删除；
//   - 这次的表数少于现有表数的 75% 时，认为拉取异常，一张都不删、也不计数；
//   - 一次最多删除 MaxPerRound 张。
//
// 待删计数只在内存里：重启后丢失只会推迟删除，不会误删。
type DeleteGuard struct {
	MaxPerRound int
	missing     map[string]int
}

// NewDeleteGuard 用默认的每次最多 10 张。
func NewDeleteGuard() *DeleteGuard { return &DeleteGuard{MaxPerRound: 10, missing: map[string]int{}} }

// Decide 给出这次要删除的表。existing 是数据仓库里现有的表，produced 是这次生成的表。
func (g *DeleteGuard) Decide(existing []string, produced map[string]bool) []string {
	if len(existing) > 0 && len(produced)*4 < len(existing)*3 {
		return nil
	}
	var del []string
	seen := map[string]bool{}
	for _, t := range existing {
		seen[t] = true
		if produced[t] {
			delete(g.missing, t)
			continue
		}
		g.missing[t]++
		if g.missing[t] >= 2 {
			del = append(del, t)
		}
	}
	for t := range g.missing {
		if !seen[t] {
			delete(g.missing, t) // 已经不在仓库里了
		}
	}
	sort.Strings(del)
	if len(del) > g.MaxPerRound {
		del = del[:g.MaxPerRound]
	}
	for _, t := range del {
		delete(g.missing, t)
	}
	return del
}
