package main

import (
	"fmt"
	"sort"
	"strings"
)

// MemoryItem 一条情景记忆：一个"问题 -> 解决方案"的案例
type MemoryItem struct {
	Signature string  // 问题特征（用于检索）
	Problem   string  // 问题描述
	Solution  string  // 解决方案（结论）
	Score     float64 // 质量分：只有高分记忆才值得存和用
}

type MemoryStore struct {
	items []MemoryItem
}

// Add 写入一条记忆，质量分过低直接拒绝（垃圾进，垃圾出）
func (s *MemoryStore) Add(item MemoryItem) bool {
	if item.Score < 0.6 {
		return false
	}
	s.items = append(s.items, item)
	return true
}

// toWordSet 把文本切成特征集合。
// 中文没有空格，无法按词切，这里用「字符二元组（bigram）」做最简切分：
// "依赖超时" -> {依赖, 赖超, 超时}。同时保留 ASCII 单词。
// 这是一种朴素但对中文有效的关键词特征方法。
func toWordSet(text string) map[string]bool {
	set := map[string]bool{}

	// 1. ASCII 单词（英文/数字）
	lower := strings.ToLower(text)
	for _, w := range strings.FieldsFunc(lower, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	}) {
		if len(w) >= 2 {
			set[w] = true
		}
	}

	// 2. 中文字符二元组
	var cjk []rune
	for _, r := range text {
		if r >= 0x4e00 && r <= 0x9fff {
			cjk = append(cjk, r)
		}
	}
	for i := 0; i+1 < len(cjk); i++ {
		set[string(cjk[i:i+2])] = true
	}
	return set
}

// similarity 关键词重叠度（0~1），最简相似度
func similarity(a, b string) float64 {
	setA, setB := toWordSet(a), toWordSet(b)
	if len(setA) == 0 || len(setB) == 0 {
		return 0
	}
	overlap := 0
	for w := range setA {
		if setB[w] {
			overlap++
		}
	}
	return float64(overlap) / float64(len(setA))
}

// Search 检索与 query 最相似的 topK 条记忆
func (s *MemoryStore) Search(query string, topK int) []MemoryItem {
	type scored struct {
		item MemoryItem
		sim  float64
	}
	var ranked []scored
	for _, it := range s.items {
		sim := similarity(query, it.Signature+" "+it.Problem)
		if sim > 0 {
			ranked = append(ranked, scored{it, sim})
		}
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].sim > ranked[j].sim })

	var result []MemoryItem
	for i := 0; i < len(ranked) && i < topK; i++ {
		result = append(result, ranked[i].item)
	}
	return result
}

// buildSystemPromptWithMemory 检索相关记忆并拼进系统提示
func buildSystemPromptWithMemory(store *MemoryStore, task string) string {
	base := "你是一个运维诊断助手。"
	memories := store.Search(task, 2)
	if len(memories) == 0 {
		return base
	}
	var sb strings.Builder
	sb.WriteString(base)
	sb.WriteString("\n\n以下是过去解决过的相似案例，供你参考：\n")
	for i, m := range memories {
		sb.WriteString(fmt.Sprintf("案例%d：\n  问题：%s\n  解决：%s\n", i+1, m.Problem, m.Solution))
	}
	return sb.String()
}

func main() {
	store := &MemoryStore{}

	// 预置几条历史经验（模拟飞轮已经转了一段时间）
	store.Add(MemoryItem{
		Signature: "构建 依赖 下载 超时", Problem: "CI 构建失败，日志显示依赖下载超时",
		Solution: "配置国内镜像源，并把超时时间从 30s 调到 120s", Score: 0.9,
	})
	store.Add(MemoryItem{
		Signature: "数据库 死锁 并发", Problem: "接口偶发 500，日志有 deadlock found",
		Solution: "两个事务加锁顺序不一致导致死锁，统一按主键升序加锁", Score: 0.85,
	})
	store.Add(MemoryItem{
		Signature: "内存 溢出 OOM", Problem: "服务运行几小时后 OOM 被杀",
		Solution: "存在 goroutine 泄漏，未关闭的 channel 导致，加了 context 取消", Score: 0.8,
	})
	// 这条质量分太低，会被拒绝
	rejected := store.Add(MemoryItem{Signature: "随便", Problem: "不确定", Solution: "重启试试", Score: 0.3})
	fmt.Printf("低质量记忆是否被拒绝：%v\n\n", !rejected)

	// 新任务来了，检索相似历史
	newTask := "构建挂了，看着像是依赖拉不下来超时了"
	fmt.Printf("新任务：%s\n", newTask)

	hits := store.Search(newTask, 2)
	fmt.Printf("\n检索到 %d 条相似历史：\n", len(hits))
	for i, h := range hits {
		fmt.Printf("  %d. %s\n", i+1, h.Problem)
	}

	fmt.Println("\n注入记忆后的系统提示：")
	fmt.Println("----------------------------------------")
	fmt.Println(buildSystemPromptWithMemory(store, newTask))
	fmt.Println("----------------------------------------")

	// 提示：关键词检索认不出"内存溢出"和"OOM"以外的同义词，
	// 比如查"付款失败"匹配不到"支付异常"——这正是第 06 篇 RAG 要解决的。
}
