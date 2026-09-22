package main

// 三种 Agent 推理范式，跑在同一个任务上，让你看清它们控制流的差别：
//   ReAct            Thought → Action → Observation 一个循环滚到底
//   Plan-and-Execute 先让 planner 一次性出计划，executor 再逐步执行
//   Reflexion        actor 解题 → evaluator 打分 → 失败就写反思、带着反思重试
//
// 共用同一套工具（一个计算器）和同一个 LLM 客户端。
// 任务：一道需要分步计算的应用题，答案可被程序精确校验——
// 这正好给 Reflexion 提供了「成败信号」，能演示它的自省重试。
//
// 需要 OPENAI_API_KEY 才能跑（和第 02 篇一致）。用法：
//   go run .            # 依次跑三种范式做对比
//   go run . react      # 只跑某一种：react | plan | reflexion

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// ---------- 共用：OpenAI Chat Completions（同第 02 篇）----------

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string             `json:"type"`
	Function FunctionDefinition `json:"function"`
}

type FunctionDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []Tool    `json:"tools,omitempty"`
}

type ChatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
}

func callLLM(messages []Message, tools []Tool) (*ChatResponse, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	reqBody := ChatRequest{Model: "gpt-5.4-mini", Messages: messages, Tools: tools}
	buf, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest("POST", baseURL+"/chat/completions", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var cr ChatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return nil, fmt.Errorf("解析失败: %v, 原始响应: %s", err, body)
	}
	if len(cr.Choices) == 0 {
		return nil, fmt.Errorf("无返回, 原始响应: %s", body)
	}
	return &cr, nil
}

// ---------- 共用工具：一个计算器 ----------

var calcTool = Tool{Type: "function", Function: FunctionDefinition{
	Name:        "calculate",
	Description: "计算一个算术表达式，支持 + - * / 和括号，例如 (6800*0.85)-500",
	Parameters: map[string]any{
		"type":       "object",
		"properties": map[string]any{"expr": map[string]any{"type": "string", "description": "算术表达式"}},
		"required":   []string{"expr"},
	},
}}

// calc 一个极小的表达式求值器（够 demo 用）。
func calc(expr string) (float64, error) {
	p := &parser{s: strings.ReplaceAll(expr, " ", "")}
	v := p.expr()
	if p.pos != len(p.s) {
		return 0, fmt.Errorf("无法解析: %s", expr)
	}
	return v, nil
}

type parser struct {
	s   string
	pos int
}

func (p *parser) expr() float64 {
	v := p.term()
	for p.pos < len(p.s) && (p.s[p.pos] == '+' || p.s[p.pos] == '-') {
		op := p.s[p.pos]
		p.pos++
		if op == '+' {
			v += p.term()
		} else {
			v -= p.term()
		}
	}
	return v
}

func (p *parser) term() float64 {
	v := p.factor()
	for p.pos < len(p.s) && (p.s[p.pos] == '*' || p.s[p.pos] == '/') {
		op := p.s[p.pos]
		p.pos++
		if op == '*' {
			v *= p.factor()
		} else {
			v /= p.factor()
		}
	}
	return v
}

func (p *parser) factor() float64 {
	if p.pos < len(p.s) && p.s[p.pos] == '(' {
		p.pos++
		v := p.expr()
		if p.pos < len(p.s) && p.s[p.pos] == ')' {
			p.pos++
		}
		return v
	}
	start := p.pos
	for p.pos < len(p.s) && (p.s[p.pos] >= '0' && p.s[p.pos] <= '9' || p.s[p.pos] == '.') {
		p.pos++
	}
	v, _ := strconv.ParseFloat(p.s[start:p.pos], 64)
	return v
}

// runCalc 执行一次工具调用，返回喂回模型的文本。
func runCalc(argsJSON string) string {
	var a struct {
		Expr string `json:"expr"`
	}
	json.Unmarshal([]byte(argsJSON), &a)
	v, err := calc(a.Expr)
	if err != nil {
		return "计算错误：" + err.Error()
	}
	return fmt.Sprintf("%s = %g", a.Expr, v)
}

// 任务与标准答案：笔记本 6800 元打 85 折，再叠加 500 元券，几件？这里买 3 件。
// 正确答案 = (6800*0.85-500)*3 = 15810
const task = "一台笔记本原价 6800 元，先打 85 折，再用一张 500 元的优惠券。我要买 3 台，一共要付多少钱？只回一个数字。"

const wantAnswer = 15810.0

// ---------- 范式一：ReAct —— 一个循环滚到底 ----------

func runReAct() (string, error) {
	fmt.Println("\n========== ReAct：Thought → Action → Observation 循环 ==========")
	messages := []Message{
		{Role: "system", Content: "你是一个会用工具的助手。需要算数时调用 calculate 工具，不要心算。"},
		{Role: "user", Content: task},
	}
	tools := []Tool{calcTool}
	for i := 0; i < 8; i++ {
		resp, err := callLLM(messages, tools)
		if err != nil {
			return "", err
		}
		choice := resp.Choices[0]
		messages = append(messages, choice.Message)
		if choice.FinishReason != "tool_calls" || len(choice.Message.ToolCalls) == 0 {
			return choice.Message.Content, nil // 模型给出最终答案，循环结束
		}
		for _, tc := range choice.Message.ToolCalls {
			out := runCalc(tc.Function.Arguments)
			fmt.Printf("  [Action] calculate(%s)\n  [Observation] %s\n", tc.Function.Arguments, out)
			messages = append(messages, Message{Role: "tool", ToolCallID: tc.ID, Content: out})
		}
	}
	return "", fmt.Errorf("超过最大轮数")
}

// ---------- 范式二：Plan-and-Execute —— 先出计划，再逐步执行 ----------

type plan struct {
	Steps []string `json:"steps"`
}

func runPlanExecute() (string, error) {
	fmt.Println("\n========== Plan-and-Execute：Planner 定计划，Executor 逐步执行 ==========")
	// 第一步：Planner 一次性把任务拆成有序步骤（不执行，只规划）。
	planResp, err := callLLM([]Message{
		{Role: "system", Content: `你是规划器。把用户任务拆成有序的计算步骤，只输出 JSON：{"steps":["第一步...","第二步..."]}，不要执行。`},
		{Role: "user", Content: task},
	}, nil)
	if err != nil {
		return "", err
	}
	var pl plan
	json.Unmarshal([]byte(extractJSON(planResp.Choices[0].Message.Content)), &pl)
	fmt.Println("  [Plan] 规划器产出：")
	for i, s := range pl.Steps {
		fmt.Printf("    %d. %s\n", i+1, s)
	}

	// 第二步：Executor 拿着计划逐步执行，前一步结果串进下一步。
	tools := []Tool{calcTool}
	var scratch string
	for i, step := range pl.Steps {
		messages := []Message{
			{Role: "system", Content: "你是执行器。只执行当前这一步，需要算数就调 calculate。已知前序结果：" + scratch},
			{Role: "user", Content: fmt.Sprintf("第 %d 步：%s", i+1, step)},
		}
		resp, err := callLLM(messages, tools)
		if err != nil {
			return "", err
		}
		choice := resp.Choices[0]
		if choice.FinishReason == "tool_calls" && len(choice.Message.ToolCalls) > 0 {
			tc := choice.Message.ToolCalls[0]
			out := runCalc(tc.Function.Arguments)
			fmt.Printf("  [Execute 第%d步] calculate(%s) → %s\n", i+1, tc.Function.Arguments, out)
			scratch += out + "; "
		} else {
			fmt.Printf("  [Execute 第%d步] %s\n", i+1, choice.Message.Content)
			scratch += choice.Message.Content + "; "
		}
	}
	return "最终累计结果：" + scratch, nil
}

// ---------- 范式三：Reflexion —— 失败就写反思，带着反思重试 ----------

func runReflexion() (string, error) {
	fmt.Println("\n========== Reflexion：Actor 解题 → Evaluator 打分 → 失败写反思重试 ==========")
	var reflections []string // episodic memory：历次失败的反思
	for trial := 1; trial <= 3; trial++ {
		// Actor：带着历史反思重新解题。
		sys := "你是解题者。直接给出最终数字答案，只回数字。"
		if len(reflections) > 0 {
			sys += "\n以下是你之前失败的反思，务必吸取：\n- " + strings.Join(reflections, "\n- ")
		}
		resp, err := callLLM([]Message{
			{Role: "system", Content: sys},
			{Role: "user", Content: task},
		}, nil)
		if err != nil {
			return "", err
		}
		answer := resp.Choices[0].Message.Content
		got := parseNumber(answer)
		fmt.Printf("  [Trial %d] Actor 答案：%s\n", trial, answer)

		// Evaluator：用程序精确校验（这就是「成败信号」）。
		if math.Abs(got-wantAnswer) < 0.5 {
			fmt.Printf("  [Evaluator] 正确！\n")
			return answer, nil
		}
		fmt.Printf("  [Evaluator] 错误（应为 %g）\n", wantAnswer)

		// Self-Reflection：让模型对自己的失败生成语言反思，存进记忆。
		refResp, err := callLLM([]Message{
			{Role: "system", Content: "你是反思器。针对下面这次错误答案，用一句话指出可能哪一步算错了、下次该怎么改。"},
			{Role: "user", Content: fmt.Sprintf("题目：%s\n我的错误答案：%s\n正确答案：%g", task, answer, wantAnswer)},
		}, nil)
		if err != nil {
			return "", err
		}
		refl := refResp.Choices[0].Message.Content
		fmt.Printf("  [Reflection] %s\n", refl)
		reflections = append(reflections, refl)
	}
	return "", fmt.Errorf("重试用尽仍未解对")
}

// ---------- 小工具 ----------

func extractJSON(s string) string {
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i >= 0 && j > i {
		return s[i : j+1]
	}
	return s
}

func parseNumber(s string) float64 {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' || r == '.' {
			b.WriteRune(r)
		}
	}
	v, _ := strconv.ParseFloat(b.String(), 64)
	return v
}

func main() {
	if os.Getenv("OPENAI_API_KEY") == "" {
		fmt.Println("未设置 OPENAI_API_KEY。三种范式都要真实调用 LLM 才能演示。")
		fmt.Println("设置后运行：go run .            # 三种范式对比")
		fmt.Println("           go run . react      # 单跑一种：react|plan|reflexion")
		return
	}

	mode := "all"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}

	var err error
	switch mode {
	case "react":
		_, err = runReAct()
	case "plan":
		_, err = runPlanExecute()
	case "reflexion":
		_, err = runReflexion()
	default:
		if _, err = runReAct(); err == nil {
			if _, err = runPlanExecute(); err == nil {
				_, err = runReflexion()
			}
		}
	}
	if err != nil {
		fmt.Println("出错：", err)
	}
}
