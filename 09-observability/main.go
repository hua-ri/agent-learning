package main

import (
	"fmt"
	"strings"
	"time"
)

// Span 一个步骤的记录，可嵌套
type Span struct {
	Name      string
	Kind      string // "llm" | "tool" | "agent"
	Start     time.Time
	Duration  time.Duration
	PromptTok int    // LLM 输入 token
	CompTok   int    // LLM 输出 token
	Model     string // LLM span 才有
	Status    string // "ok" | "error"
	Children  []*Span
}

// Tracer 收集一次请求的所有 span，维护父子关系
type Tracer struct {
	root  *Span
	stack []*Span // 当前打开的 span 栈，栈顶是当前父节点
}

func NewTracer(rootName string) *Tracer {
	root := &Span{Name: rootName, Kind: "agent", Start: time.Now(), Status: "ok"}
	return &Tracer{root: root, stack: []*Span{root}}
}

// Begin 开一个子 span，成为当前父节点
func (t *Tracer) Begin(name, kind string) *Span {
	s := &Span{Name: name, Kind: kind, Start: time.Now(), Status: "ok"}
	parent := t.stack[len(t.stack)-1]
	parent.Children = append(parent.Children, s)
	t.stack = append(t.stack, s)
	return s
}

// End 结束当前 span，出栈
func (t *Tracer) End() {
	s := t.stack[len(t.stack)-1]
	s.Duration = time.Since(s.Start)
	if len(t.stack) > 1 {
		t.stack = t.stack[:len(t.stack)-1]
	}
}

// Finish 收尾根 span
func (t *Tracer) Finish() {
	t.root.Duration = time.Since(t.root.Start)
}

// Price 每百万 token 的价格（美元），输入输出分开计价
type Price struct{ InPerM, OutPerM float64 }

// 价格表（示意，以你网关实际价格为准）
var priceTable = map[string]Price{
	"gpt-5.4-mini": {InPerM: 0.15, OutPerM: 0.60},
	"gpt-5.4":      {InPerM: 2.50, OutPerM: 10.00},
	"qwen3.6-plus": {InPerM: 0.40, OutPerM: 1.20},
}

// costOf 算单个 LLM span 的成本
func costOf(s *Span) float64 {
	p, ok := priceTable[s.Model]
	if !ok {
		return 0
	}
	return float64(s.PromptTok)/1e6*p.InPerM + float64(s.CompTok)/1e6*p.OutPerM
}

// walk 递归汇总整棵 trace 的 token 和成本
func walk(s *Span) (totalTok int, totalCost float64) {
	totalTok = s.PromptTok + s.CompTok
	totalCost = costOf(s)
	for _, c := range s.Children {
		tok, cost := walk(c)
		totalTok += tok
		totalCost += cost
	}
	return
}

// printTree 缩进打印 trace 树，直观看到嵌套和每步耗时/成本
func printTree(s *Span, depth int) {
	indent := strings.Repeat("  ", depth)
	line := fmt.Sprintf("%s- [%s] %s (%v)", indent, s.Kind, s.Name, s.Duration.Round(time.Millisecond))
	if s.Kind == "llm" {
		line += fmt.Sprintf("  tokens=%d+%d  $%.5f", s.PromptTok, s.CompTok, costOf(s))
	}
	fmt.Println(line)
	for _, c := range s.Children {
		printTree(c, depth+1)
	}
}

func main() {
	tr := NewTracer("处理用户请求：调研并诊断")

	// 第 1 轮：LLM 决定调工具
	llm1 := tr.Begin("LLM 调用 1（决定调工具）", "llm")
	llm1.Model, llm1.PromptTok, llm1.CompTok = "gpt-5.4-mini", 1200, 80
	time.Sleep(40 * time.Millisecond)
	tr.End()

	// 工具：查数据库
	tool1 := tr.Begin("工具: 查数据库", "tool")
	_ = tool1
	time.Sleep(60 * time.Millisecond)
	tr.End()

	// 第 2 轮：LLM 看结果后派子 Agent
	llm2 := tr.Begin("LLM 调用 2（决定派子 Agent）", "llm")
	llm2.Model, llm2.PromptTok, llm2.CompTok = "gpt-5.4-mini", 1500, 120
	time.Sleep(40 * time.Millisecond)
	tr.End()

	// 子 Agent：内部又是一整套嵌套
	tr.Begin("子 Agent: 深度排查", "agent")
	{
		subLLM := tr.Begin("子LLM 调用", "llm")
		subLLM.Model, subLLM.PromptTok, subLLM.CompTok = "gpt-5.4", 3000, 500
		time.Sleep(50 * time.Millisecond)
		tr.End()

		tr.Begin("工具: 调外部 API", "tool")
		time.Sleep(30 * time.Millisecond)
		tr.End()
	}
	tr.End() // 结束子 Agent

	// 第 3 轮：LLM 生成最终答案
	llm3 := tr.Begin("LLM 调用 3（生成最终答案）", "llm")
	llm3.Model, llm3.PromptTok, llm3.CompTok = "gpt-5.4-mini", 2000, 300
	time.Sleep(40 * time.Millisecond)
	tr.End()

	tr.Finish()

	fmt.Println("== Trace 树 ==")
	printTree(tr.root, 0)

	totalTok, totalCost := walk(tr.root)
	fmt.Printf("\n== 总账 ==\n总耗时：%v\n总 token：%d\n总成本：$%.5f\n",
		tr.root.Duration.Round(time.Millisecond), totalTok, totalCost)

	// 简单的成本告警
	const budget = 0.01 // 单请求预算阈值（美元）
	if totalCost > budget {
		fmt.Printf("⚠ 本次请求成本 $%.5f 超过预算 $%.5f，需关注！\n", totalCost, budget)
	}
}
