package main

// 微调 vs Prompt vs RAG —— 离线「决策顾问」，把选型经验编码成可执行规则。
//
// 核心主张：事实进 prompt/RAG，行为进权重。
//   - RAG：知识会变、语料大、要引用来源 → 检索注入，不动模型
//   - Fine-tune：要固定的「形式/风格/行为」，且不是为了灌新事实
//   - Prompt：需求一次性、样本少、快速验证
//
// 一条铁律（Gekhman et al. EMNLP 2024, arXiv:2405.05904）：
//   在「新事实」上微调，模型学得慢，还会线性抬高幻觉率。
//   所以「以为是行为问题、其实是想让模型记住事实」= 陷阱，必须改判 RAG。
//
// 用法：go run .   （完全离线，无需 API key）

import (
	"fmt"
	"strings"
)

// Problem 问题类型：决定第一优先级。
type Problem string

const (
	Knowledge Problem = "knowledge" // 要「知道」某些事实/文档
	Behavior  Problem = "behavior"  // 要固定的「形式/风格/格式/流程」
	Framing   Problem = "framing"   // 一次性的措辞/任务框定
)

// Profile 一个用例的画像，决策顾问的输入。
type Profile struct {
	Name            string
	Problem         Problem
	NeedsFacts      bool   // Gekhman 护栏：这个「行为」其实是想让模型记住事实吗？
	KnowledgeChange string // "often"（≤月级更新）/ "stable"（几乎不变）
	KnowledgeSize   string // "small"（<50 文档/<100K token）/ "large"（>1000 文档/>1M token）
	LabeledExamples int    // 有多少条高质量标注样本
	LowLatency      bool   // 是否对延迟极敏感（不能每次都检索）
	HighVolume      bool   // 是否高频调用（单次省钱会被放大）
}

// recommend 把选型阶梯（prompt → RAG → fine-tune → 组合）编码成规则。
func recommend(p Profile) (string, []string) {
	var reasons []string

	// —— 规则 0：Gekhman 护栏。最优先，防止最常见的踩坑。——
	if p.Problem == Behavior && p.NeedsFacts {
		reasons = append(reasons,
			"看似行为问题，实为「想让模型记住事实」——在新事实上微调学得慢且抬高幻觉（Gekhman 2024）",
			"改判：事实用检索注入，不要写进权重")
		return "RAG（不要用 fine-tune 灌事实）", reasons
	}

	// —— 规则 1：知识型问题 → RAG / CAG 分流。——
	if p.Problem == Knowledge {
		if p.KnowledgeChange == "often" {
			reasons = append(reasons, "知识会频繁变（≤月级）→ 微调无法跟上，必须用检索")
			return "RAG（检索会变的知识，模型保持不动）", reasons
		}
		if p.KnowledgeSize == "small" {
			reasons = append(reasons,
				"语料小且稳定（<50 文档 / <100K token）→ 整个塞进 context 更简单",
				"CAG（Cache-Augmented）/ 长上下文：预加载全部语料 + prompt caching，省掉检索链路")
			return "CAG / 长上下文（小而稳的语料直接全塞）", reasons
		}
		reasons = append(reasons, "语料大（>1000 文档 / >1M token）→ 塞不进 context，必须检索")
		return "RAG（大语料只能检索）", reasons
	}

	// —— 规则 2：一次性措辞/框定 → Prompt。——
	if p.Problem == Framing {
		reasons = append(reasons, "一次性/低频的措辞与任务框定 → 改 prompt 最快，无需任何训练")
		return "Prompt（改提示词即可，别上重武器）", reasons
	}

	// —— 规则 3：真·行为问题，按样本量走阶梯。——
	// Behavior 且不需要灌事实：这才是 fine-tune 的正当场景。
	switch {
	case p.LabeledExamples < 10:
		reasons = append(reasons,
			fmt.Sprintf("行为问题但只有 %d 条样本（<10）→ 数据不够微调", p.LabeledExamples),
			"先用 prompt + 少量示例把需求打磨清楚，攒够样本再谈微调")
		return "Prompt（样本不足，先跑起来攒数据）", reasons
	case p.LabeledExamples < 50:
		reasons = append(reasons,
			fmt.Sprintf("%d 条样本（10-49）→ 够 few-shot，还不够稳定微调", p.LabeledExamples),
			"Few-shot 先顶上；若延迟/成本吃紧再考虑微调把示例「烧进」权重")
		return "Few-shot Prompt（够示例，微调再等等）", reasons
	default:
		reasons = append(reasons,
			fmt.Sprintf("%d 条高质量样本（≥50）→ 微调可行", p.LabeledExamples),
			"行为/格式固定且不灌事实 → 微调把行为烧进权重，缩短 prompt、降延迟")
		rec := "Fine-tune（把稳定行为烧进权重）"
		if p.LowLatency || p.HighVolume {
			reasons = append(reasons, "叠加低延迟/高频 → 微调后 prompt 更短，单次省钱被规模放大，收益最大")
		}
		return rec, reasons
	}
}

// combineHint 对需要「知识 + 行为」双管齐下的场景，给组合建议。
func combineHint(p Profile) string {
	if p.Problem == Knowledge && p.LabeledExamples >= 50 {
		return "若既要检索知识、又要固定回答格式：RAFT（arXiv:2403.10131）——" +
			"在「带检索上下文」的样本上微调，让模型学会「怎么用检索到的内容」，RAG 与 fine-tune 叠加。"
	}
	return ""
}

func main() {
	fmt.Println("========== 微调 vs Prompt vs RAG：离线决策顾问 ==========")
	fmt.Println("规则阶梯：Gekhman 护栏 → 知识走 RAG/CAG → 一次性走 Prompt → 行为按样本量走阶梯")
	fmt.Println(strings.Repeat("-", 64))

	profiles := []Profile{
		{
			Name:            "客服机器人，答的是每周更新的产品文档",
			Problem:         Knowledge,
			KnowledgeChange: "often",
			KnowledgeSize:   "large",
		},
		{
			Name:            "内部 FAQ，就 30 篇几乎不变的政策文档",
			Problem:         Knowledge,
			KnowledgeChange: "stable",
			KnowledgeSize:   "small",
		},
		{
			Name:            "把所有回复固定成严格 JSON 格式，有 800 条标注样本",
			Problem:         Behavior,
			NeedsFacts:      false,
			LabeledExamples: 800,
			LowLatency:      true,
			HighVolume:      true,
		},
		{
			Name:            "让模型「记住我们公司 200 条内部术语的定义」",
			Problem:         Behavior, // 用户以为是行为，其实是灌事实
			NeedsFacts:      true,
			LabeledExamples: 200,
		},
		{
			Name:            "临时活动，让文案语气更活泼一点",
			Problem:         Framing,
		},
		{
			Name:            "想固定输出风格，但只攒了 8 条样本",
			Problem:         Behavior,
			NeedsFacts:      false,
			LabeledExamples: 8,
		},
	}

	for _, p := range profiles {
		rec, reasons := recommend(p)
		fmt.Printf("\n▶ 场景：%s\n", p.Name)
		fmt.Printf("  → 建议：%s\n", rec)
		for _, r := range reasons {
			fmt.Printf("     · %s\n", r)
		}
		if hint := combineHint(p); hint != "" {
			fmt.Printf("     ⊕ 组合：%s\n", hint)
		}
	}

	fmt.Println(strings.Repeat("-", 64))
	fmt.Println("一句话：事实进 RAG，行为进权重，一次性进 prompt；别用微调灌新事实。")
}
