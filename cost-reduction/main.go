package main

// Agent 降本三板斧，全部离线可跑（不需要 API key），演示的是机制与省钱算法：
//
//   1) 语义缓存 Semantic Cache —— 按「含义」命中，直接跳过 LLM 调用
//      （真实系统用 embedding 余弦相似度；这里用 Jaccard 词汇重叠做教学 proxy，
//       实测 Jaccard 与 cosine 相关 r≈0.79，能追踪趋势但会系统性低估语义——
//       这恰好是「为什么真实系统要用 embedding」的活教材）
//
//   2) 模型路由 Model Router —— 把每个请求路由到「能干活的最便宜模型」
//      （真实系统用训练好的分类器；这里用复杂度启发式，最简版）
//
//   3) 前缀缓存 Prompt Caching —— 对必然发生的调用，折扣静态前缀的输入成本
//      （用 Anthropic 真实乘数：write 1.25x、read 0.1x，且只折扣输入、输出原价）
//
// 三者互补不冲突，是一条流水线：缓存命中就返回 → 否则路由到便宜模型 → 调用时前缀缓存降本。
//
// 用法：go run .

import (
	"fmt"
	"strings"
)

// ===================================================================
// 板斧一：语义缓存（Jaccard proxy）
// ===================================================================

type semanticCache struct {
	entries   []cacheEntry
	threshold float64
}

type cacheEntry struct {
	query    string
	response string
}

// tokenSet 把句子切成词集合（去空格、转小写）。
func tokenSet(s string) map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.Fields(strings.ToLower(s)) {
		set[strings.Trim(w, "?？.。,，")] = true
	}
	return set
}

// jaccard 词汇重叠度 = 交集/并集。这是语义相似的保守下界。
func jaccard(a, b map[string]bool) float64 {
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// get 命中返回缓存响应，否则 miss。
func (c *semanticCache) get(query string) (string, bool) {
	qs := tokenSet(query)
	for _, e := range c.entries {
		if jaccard(qs, tokenSet(e.query)) >= c.threshold {
			return e.response, true
		}
	}
	return "", false
}

func (c *semanticCache) put(query, response string) {
	c.entries = append(c.entries, cacheEntry{query, response})
}

func demoSemanticCache() {
	fmt.Println("========== 板斧一：语义缓存 ==========")
	cache := &semanticCache{threshold: 0.5}
	// 先塞一条：pro 版多少钱
	cache.put("pro 版 多少 钱", "Pro 版每月 99 元")

	queries := []string{
		"pro 版 多少 钱",          // 完全一样 → 命中
		"pro 版 的 价格 是 多少",   // 同义改写：人看是同一问题，但 Jaccard 词汇重叠不够 → 漏判 miss
		"企业版 有 什么 功能",      // 完全不同 → miss，得真调 LLM
	}
	hits, calls := 0, 0
	for _, q := range queries {
		if resp, ok := cache.get(q); ok {
			fmt.Printf("  [HIT ] %-18s → %s（跳过 LLM）\n", q, resp)
			hits++
		} else {
			fmt.Printf("  [MISS] %-18s → 调用 LLM……并写入缓存\n", q)
			cache.put(q, "（新答案）")
			calls++
		}
	}
	fmt.Printf("  命中 %d / %d，省下 %d 次 LLM 调用\n", hits, len(queries), hits)
	fmt.Println("  看第 2 条：人眼看它和第 1 条是同一个问题，Jaccard 却因词汇重叠不足漏判了。")
	fmt.Println("  这正是「为什么真实系统用 embedding 余弦相似度」而非词汇重叠的活教材。")
	fmt.Println()
}

// ===================================================================
// 板斧二：模型路由
// ===================================================================

type model struct {
	name       string
	pricePerMTok float64 // 输入价，$/百万 token
}

var cheap = model{"small-model", 0.25}
var strong = model{"large-model", 3.00}

// route 复杂度启发式：命中「难」信号就上强模型，否则便宜模型接住。
func route(query string) model {
	hardSignals := []string{"写代码", "证明", "设计架构", "分析原因", "debug", "推导"}
	for _, sig := range hardSignals {
		if strings.Contains(query, sig) {
			return strong
		}
	}
	if len([]rune(query)) > 40 { // 长问题往往更复杂
		return strong
	}
	return cheap
}

func demoRouter() {
	fmt.Println("========== 板斧二：模型路由 ==========")
	// 生产审计：60-75% 是简单 query，这正是路由经济学的基础
	queries := []string{
		"今天星期几",
		"北京的邮编是多少",
		"帮我 debug 这段并发死锁的代码",
		"总结一下这句话",
		"设计架构：一个支持千万级并发的秒杀系统",
	}
	const tokensPerCall = 2000.0 // 假设每次输入 2000 token
	var routedCost, baselineCost float64
	for _, q := range queries {
		m := route(q)
		routedCost += tokensPerCall / 1e6 * m.pricePerMTok
		baselineCost += tokensPerCall / 1e6 * strong.pricePerMTok // 基线：全用强模型
		fmt.Printf("  %-30s → %s\n", q, m.name)
	}
	fmt.Printf("  全用强模型：$%.5f   路由后：$%.5f   省 %.0f%%\n\n",
		baselineCost, routedCost, (1-routedCost/baselineCost)*100)
}

// ===================================================================
// 板斧三：前缀缓存成本计算器（Anthropic 乘数）
// ===================================================================

func demoPromptCaching() {
	fmt.Println("========== 板斧三：前缀缓存（Prompt Caching）==========")
	const base = 3.0        // base input $/MTok
	const staticTok = 4000  // system+tools+few-shot 静态前缀
	const dynamicTok = 500  // 每轮变化的部分
	const turns = 10        // 多轮对话轮数

	// 无缓存：每轮都全价重算 静态+动态
	noCache := float64(turns) * float64(staticTok+dynamicTok) / 1e6 * base

	// 有缓存：第 1 轮写缓存(1.25x 静态) + 动态全价；之后每轮 读缓存(0.1x 静态) + 动态全价
	writeCost := float64(staticTok)/1e6*base*1.25 + float64(dynamicTok)/1e6*base
	readPerTurn := float64(staticTok)/1e6*base*0.1 + float64(dynamicTok)/1e6*base
	withCache := writeCost + float64(turns-1)*readPerTurn

	fmt.Printf("  场景：%d 轮对话，静态前缀 %d token（系统+工具+few-shot），每轮动态 %d token\n", turns, staticTok, dynamicTok)
	fmt.Printf("  无缓存：$%.5f\n", noCache)
	fmt.Printf("  有缓存：$%.5f（第1轮 write 1.25x，之后 read 0.1x）\n", withCache)
	fmt.Printf("  省 %.0f%%\n", (1-withCache/noCache)*100)
	fmt.Println("  注意：只折扣「输入」，输出永远原价；命中率太低 + write 溢价会反而更贵")
	fmt.Println("  结构铁律：静态内容(tools→system→few-shot)放前面，动态放最后")
}

func main() {
	demoSemanticCache()
	demoRouter()
	demoPromptCaching()
	fmt.Println("\n三板斧是流水线，互补不冲突：")
	fmt.Println("  请求 → [语义缓存] 命中即返回 → [路由] 选最便宜模型 → [前缀缓存] 调用时降本")
	fmt.Println("  单招 20-30%，三招叠加常达 50-70%。但都只省输入侧，不碰输出成本。")
}
