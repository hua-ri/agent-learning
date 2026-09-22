package main

// 多模态 Agent 的两个「落到可照做」的工程核心，完全离线（不需要 API key）：
//
//   1) 图片 token 成本计算器 —— 图片不是「一张 = 一点点钱」，而是按分块(tile)烧 token。
//      高清大图能吃掉上千 token。这里用 OpenAI 和 Anthropic 的真实分块算法算给你看，
//      并演示「降采样」这个最重要的省钱杠杆：把长边缩到阈值，成本砍 40-70% 而不掉精度。
//
//   2) OCR vs 视觉模型 路由 —— 不是所有文档都该丢给贵的视觉模型。干净的印刷体用便宜
//      OCR，OCR 置信度低或版式复杂(表格/手写/多栏)再升级到视觉模型。这是生产里的默认姿态。
//
// 为什么离线做这两个：真正的视觉推理要调模型，但「怎么控成本、怎么路由」是纯工程决策，
// 恰恰是最容易算错账、最该先想清楚的部分。
//
// 用法：go run .

import (
	"fmt"
	"math"
	"strings"
)

// ===================================================================
// 板块一：图片 token 成本计算器
// ===================================================================

// openAITokens 按 OpenAI 的分块算法算一张图消耗的 token（经典 GPT-4o 口径）。
//
// detail=low：无论多大，缩到 512x512，固定 85 token。
// detail=high/auto：
//   1) 先等比缩放使之放进 2048x2048
//   2) 若最短边 > 768，再缩放使最短边 = 768
//   3) 数 512x512 的分块数 tiles
//   4) 成本 = 85(基座) + 170 * tiles
func openAITokens(w, h int, detail string) int {
	if detail == "low" {
		return 85
	}
	// 步骤 1：放进 2048x2048
	if w > 2048 || h > 2048 {
		scale := 2048.0 / math.Max(float64(w), float64(h))
		w = int(float64(w) * scale)
		h = int(float64(h) * scale)
	}
	// 步骤 2：最短边缩到 768
	shortest := math.Min(float64(w), float64(h))
	if shortest > 768 {
		scale := 768.0 / shortest
		w = int(float64(w) * scale)
		h = int(float64(h) * scale)
	}
	// 步骤 3：数 512 分块（向上取整）
	tilesW := int(math.Ceil(float64(w) / 512.0))
	tilesH := int(math.Ceil(float64(h) / 512.0))
	tiles := tilesW * tilesH
	// 步骤 4
	return 85 + 170*tiles
}

// claudeTokens 按 Anthropic 的口径估算：先把长边降采样到 1568，
// 再按 ceil(w/384) * ceil(h/384) * ~170 估算。
func claudeTokens(w, h int) int {
	longest := math.Max(float64(w), float64(h))
	if longest > 1568 {
		scale := 1568.0 / longest
		w = int(float64(w) * scale)
		h = int(float64(h) * scale)
	}
	blocksW := int(math.Ceil(float64(w) / 384.0))
	blocksH := int(math.Ceil(float64(h) / 384.0))
	return blocksW * blocksH * 170
}

func demoImageCost() {
	fmt.Println("========== 板块一：图片 token 成本计算器 ==========")
	type shot struct {
		name string
		w, h int
	}
	shots := []shot{
		{"手机截图 1179x2556", 1179, 2556},
		{"4K 文档扫描 3840x2160", 3840, 2160},
		{"小缩略图 400x300", 400, 300},
	}
	fmt.Printf("%-24s %14s %14s %12s\n", "图片", "OpenAI(high)", "OpenAI(low)", "Claude")
	for _, s := range shots {
		fmt.Printf("%-24s %14d %14d %12d\n",
			s.name, openAITokens(s.w, s.h, "high"), openAITokens(s.w, s.h, "low"), claudeTokens(s.w, s.h))
	}
	fmt.Println()

	// 降采样杠杆：注意两家都会「自动封顶」分辨率（OpenAI 封到 2048/768，Claude 封到 1568）。
	// 所以真正的省钱空间是：主动缩到「你任务够用的分辨率」，低于 provider 的封顶线。
	fmt.Println("降采样杠杆（4K 文档扫描 3840x2160，OpenAI high）：")
	full := openAITokens(3840, 2160, "high")
	// 主动缩到长边 1024（文本清晰的文档通常够用），会实打实减少分块数
	scale := 1024.0 / 3840.0
	dw, dh := int(3840*scale), int(2160*scale)
	down := openAITokens(dw, dh, "high")
	fmt.Printf("  原图 high：%d token（provider 内部已封到 2048/768，你不缩它也就这么多）\n", full)
	fmt.Printf("  主动缩到 %dx%d 后：%d token\n", dw, dh, down)
	fmt.Printf("  → 省 %.0f%%。关键：缩到「低于 provider 封顶」才真省；文本清晰的图这样做几乎不掉精度。\n", (1-float64(down)/float64(full))*100)
	fmt.Println("  铁律：detail=auto 默认按 high 计费；批量管线不显式控制会悄悄烧钱。")
	fmt.Println()
}

// ===================================================================
// 板块二：OCR vs 视觉模型 路由
// ===================================================================

type doc struct {
	name          string
	ocrConfidence float64 // OCR 引擎给出的平均置信度 0-1
	hasTable      bool    // 含表格
	handwritten   bool    // 含手写
	multiColumn   bool    // 多栏版式
}

// routeDoc 生产默认姿态：干净印刷体走便宜 OCR；置信度低或版式复杂 → 升级视觉模型。
func routeDoc(d doc) (string, string) {
	const confThreshold = 0.80 // OCR 平均置信度低于此 → 视觉模型更划算
	if d.handwritten {
		return "视觉模型", "含手写，传统 OCR 常崩（可能到 60-75%），视觉模型 >98%"
	}
	if d.hasTable || d.multiColumn {
		return "视觉模型", "含表格/多栏版式，OCR 会打乱阅读顺序，视觉模型保结构"
	}
	if d.ocrConfidence < confThreshold {
		return "视觉模型", fmt.Sprintf("OCR 置信度 %.2f < %.2f，OCR 会静默出错、下游 token 反而更贵", d.ocrConfidence, confThreshold)
	}
	return "OCR", fmt.Sprintf("干净印刷体 + OCR 置信度 %.2f 达标，用便宜 OCR 即可", d.ocrConfidence)
}

func demoOCRRouting() {
	fmt.Println("========== 板块二：OCR vs 视觉模型 路由 ==========")
	docs := []doc{
		{"标准打印发票", 0.96, false, false, false},
		{"手写病历单", 0.50, false, true, false},
		{"含复杂表格的财报 PDF", 0.88, true, false, false},
		{"扫描质量差的旧合同", 0.62, false, false, false},
		{"两栏学术论文", 0.90, false, false, true},
	}
	for _, d := range docs {
		route, why := routeDoc(d)
		fmt.Printf("  %-22s → %-8s（%s）\n", d.name, route, why)
	}
	fmt.Println("  策略：默认便宜 OCR，置信度阈值 ~0.80，只把低置信/版式复杂的升级到视觉模型。")
	fmt.Println("  隐性成本：OCR 失败是「静默」的——乱码往下游一路放大，比视觉模型还贵。")
	fmt.Println()
}

// ===================================================================
// 板块三：结构化抽取的骨架（image → schema JSON）
// ===================================================================

func demoExtractionPattern() {
	fmt.Println("========== 板块三：结构化抽取的生产骨架 ==========")
	fmt.Println("主流模式：图片 in → schema 约束的 JSON out，一次视觉调用，零训练数据。")
	fmt.Println("骨架（真实调用需视觉模型，此处只展示决策要点）：")
	fmt.Println(strings.TrimSpace(`
  1) 用 json_schema (strict) 约束输出，字段设 nullable，别锁死枚举——给模型「不确定」的出口
  2) 加数值/逻辑护栏：如「明细行之和必须等于总额」，跑校验而非盲信
  3) 校验失败 → 升级更强模型或转人工队列（两级路由）
  4) 成本路由：单页发票用 Flash/Haiku 级便宜模型约几厘钱，只在校验失败时升级`))
	fmt.Println("  印刷体发票这套能到 >98% 字段准确率；手写/劣质扫描会掉到 85-95%，务必留校验闸。")
	fmt.Println()
}

func main() {
	demoImageCost()
	demoOCRRouting()
	demoExtractionPattern()

	fmt.Println("小结（多模态 Agent 的工程铁律）：")
	fmt.Println("  · 图片按分块烧 token，高清大图上千 token；降采样是第一省钱杠杆")
	fmt.Println("  · detail=auto 默认 high 计费，批量管线要显式控制")
	fmt.Println("  · 默认便宜 OCR，低置信/复杂版式再升级视觉模型")
	fmt.Println("  · 抽取用 schema 约束 + 逻辑护栏 + 校验失败两级升级")
}
