package main

// GraphRAG 的核心机制，完全离线演示（不需要 API key）：
//
// 一句话：朴素向量 RAG 把每个 chunk 单独存向量，「连不起点」——当答案需要跨多个
// chunk、沿着共享实体一路跳时，top-k 检索会漏掉那个「关键词对不上、但逻辑上相关」
// 的 chunk。GraphRAG 先抽实体+关系建成知识图谱，再沿关系做多跳遍历，把点连起来。
//
// 本 demo 用一个经典的多跳问题演示这个差异：
//   问：「张三所在公司的竞争对手是谁创立的?」
//   答：李四
//   推理链：张三 --任职--> Acme --竞品--> Globex --创立者--> 李四
//
// 真实系统里「抽实体+关系」由 LLM 逐 chunk 完成（这也是 GraphRAG 索引贵的根源，
// 每 chunk 要 4-6 次 LLM 调用）。这里用手写三元组冒充抽取结果，聚焦机制本身。
//
// 用法：go run .

import (
	"fmt"
	"sort"
	"strings"
)

// ===================================================================
// 语料：3 个 chunk。注意答案分散在不同 chunk，且彼此关键词不重叠。
// ===================================================================

type chunk struct {
	id   string
	text string
}

var corpus = []chunk{
	{"c1", "张三 是 Acme 公司的 CTO，负责技术架构。"},
	{"c2", "Acme 公司在市场上的主要竞争对手是 Globex。"},
	{"c3", "Globex 是一家由 李四 创立的初创公司。"},
}

// ===================================================================
// 板块一：朴素向量 RAG（用关键词重叠冒充向量相似度）
// ===================================================================

// tokenSet 切词成集合。
func tokenSet(s string) map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.Fields(s) {
		set[strings.Trim(w, "，。、？?")] = true
	}
	return set
}

// overlap 关键词重叠数（教学 proxy，真实系统是 embedding 余弦相似度）。
func overlap(a, b map[string]bool) int {
	n := 0
	for w := range a {
		if b[w] {
			n++
		}
	}
	return n
}

// naiveRAG 返回与 query 关键词重叠最高的 topK 个 chunk。
func naiveRAG(query string, topK int) []chunk {
	qs := tokenSet(query)
	type scored struct {
		c     chunk
		score int
	}
	var ranked []scored
	for _, c := range corpus {
		ranked = append(ranked, scored{c, overlap(qs, tokenSet(c.text))})
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	var out []chunk
	for i := 0; i < topK && i < len(ranked); i++ {
		if ranked[i].score > 0 {
			out = append(out, ranked[i].c)
		}
	}
	return out
}

func demoNaive(query string) {
	fmt.Println("========== 朴素向量 RAG（top-2 关键词检索）==========")
	fmt.Printf("问题：%s\n", query)
	hits := naiveRAG(query, 2)
	fmt.Println("检索到的 chunk：")
	for _, c := range hits {
		fmt.Printf("  [%s] %s\n", c.id, c.text)
	}
	// 判断答案 chunk c3 是否被检索到
	got := false
	for _, c := range hits {
		if c.id == "c3" {
			got = true
		}
	}
	fmt.Printf("是否包含答案所在的 c3（李四）？ %v\n", got)
	fmt.Println("→ c3「Globex 由李四创立」和问题里的词几乎不重叠，被漏掉了。")
	fmt.Println("  朴素 RAG 看不见「张三→公司→竞品→创立者」这条跨 chunk 的链。")
	fmt.Println()
}

// ===================================================================
// 板块二：GraphRAG —— 抽实体+关系建图，沿关系多跳遍历
// ===================================================================

// triple 一条「主语 --关系--> 宾语」，模拟 LLM 从 chunk 抽出的知识。
type triple struct {
	subj, rel, obj, src string // src=来源 chunk，用于溯源引用
}

// 模拟 LLM 逐 chunk 抽取的结果（真实系统这一步烧钱：每 chunk 4-6 次 LLM 调用）。
var graph = []triple{
	{"张三", "任职于", "Acme", "c1"},
	{"Acme", "竞争对手", "Globex", "c2"},
	{"Globex", "创立者", "李四", "c3"},
}

// neighbors 返回从实体 e 出发、经 rel 关系能到达的所有实体（含来源）。
func neighbors(e, rel string) []triple {
	var out []triple
	for _, t := range graph {
		if t.subj == e && t.rel == rel {
			out = append(out, t)
		}
	}
	return out
}

// multiHop 按给定的关系路径，从起点实体逐跳遍历。这就是 GraphRAG「local search」
// 的核心：以实体为锚点，沿关系边走，把散落各 chunk 的事实串成一条推理链。
func multiHop(start string, relPath []string) (string, []string) {
	cur := start
	var trail []string
	for _, rel := range relPath {
		ns := neighbors(cur, rel)
		if len(ns) == 0 {
			return "", trail
		}
		next := ns[0]
		trail = append(trail, fmt.Sprintf("%s --%s--> %s（来源 %s）", next.subj, next.rel, next.obj, next.src))
		cur = next.obj
	}
	return cur, trail
}

func demoGraph(query string) {
	fmt.Println("========== GraphRAG（实体锚定 + 多跳遍历）==========")
	fmt.Printf("问题：%s\n", query)
	fmt.Println("知识图谱（由 LLM 逐 chunk 抽取，此处用手写三元组模拟）：")
	for _, t := range graph {
		fmt.Printf("  %s --%s--> %s\n", t.subj, t.rel, t.obj)
	}
	// 从问题里识别锚点实体「张三」，规划关系路径。
	// 真实系统由 LLM 做实体识别 + 路径规划；这里直接给出以聚焦「多跳」这个机制。
	fmt.Println("\n锚点实体：张三；遍历路径：任职于 → 竞争对手 → 创立者")
	answer, trail := multiHop("张三", []string{"任职于", "竞争对手", "创立者"})
	fmt.Println("推理链：")
	for i, step := range trail {
		fmt.Printf("  第%d跳  %s\n", i+1, step)
	}
	fmt.Printf("→ 答案：%s\n", answer)
	fmt.Println("  每一跳都带来源 chunk，天然可溯源引用——这是 GraphRAG 的附赠好处。")
	fmt.Println()
}

// ===================================================================
// 板块三：Global search 的直觉 —— 社区 + 摘要
// ===================================================================

func demoGlobal() {
	fmt.Println("========== Global search 直觉：社区检测 + 摘要 ==========")
	fmt.Println("上面的「多跳」是 local search（回答具体事实问题）。")
	fmt.Println("面对「整个语料在讲什么主题」这类全局问题，local search 无从下手：")
	fmt.Println("  GraphRAG 的答案是：用 Leiden 算法把图切成层级化「社区」，")
	fmt.Println("  给每个社区预生成摘要，全局问题就 map-reduce 这些社区摘要。")
	fmt.Println("本例的图是一个连通体，可概括为一条社区摘要：")
	fmt.Println("  「围绕 Acme 与其竞品 Globex 的公司关系网，涉及高管张三与创始人李四」")
	fmt.Println("  ——朴素 RAG 检索几个 chunk 拼不出这种全局主题概括。")
	fmt.Println()
}

func main() {
	const query = "张三 所在 公司 的 竞争对手 是 谁 创立 的"

	demoNaive(query)
	demoGraph(query)
	demoGlobal()

	fmt.Println("小结：")
	fmt.Println("  · 朴素向量 RAG 漏掉了关键词对不上的 c3 → 答不出多跳问题")
	fmt.Println("  · GraphRAG 沿实体关系跳三步把点连起来 → 答对且可溯源")
	fmt.Println("  · 代价：建图要逐 chunk 调 LLM 抽实体，索引成本比纯 embedding 高 20-100 倍")
	fmt.Println("  · 决策：先用朴素 RAG，命中「多跳/连点/全局主题」瓶颈时再上 GraphRAG")
}
