package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// 这个 demo 把 RAG 和 Skills 放进同一个 Agent，看它自己路由：
//   search_docs = RAG 替身：从离线小库按关键词捞「事实」（declarative）
//   load_skill  = Skills   ：读某个 skill 的完整操作说明「手艺」（procedural）
// 三个 query 分别打向：纯事实 / 纯手艺 / 两者都要。复用第 02、11 篇的循环。
//
// 注意：search_docs 用关键词匹配假装检索，只为演示「捞事实」这个动作、离线可跑。
// 真正的向量检索见第 06 篇，真正的渐进式披露见第 11 篇。

// ---------- OpenAI Chat Completions 数据结构（同第 02 篇）----------

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

// ---------- 工具一：search_docs = RAG 替身（捞事实）----------

// docs 是一个离线「知识库」。真实场景这里是 embedding + 向量库 ANN 召回。
var docs = map[string]string{
	"退款时限": "退款政策：下单 7 天内可无理由退款，超过 7 天需人工审批。",
	"会员等级": "会员分为普通/银卡/金卡，金卡退款不受 7 天限制。",
}

func searchDocs(args map[string]any) string {
	q, _ := args["query"].(string)
	var hits []string
	for k, v := range docs {
		if strings.Contains(q, k) || strings.Contains(v, q) || strings.Contains(q, "会员") && k == "会员等级" {
			hits = append(hits, v)
		}
	}
	if len(hits) == 0 {
		return "（没检索到相关文档）"
	}
	return strings.Join(hits, "\n")
}

// ---------- 工具二：load_skill = Skills（拿手艺）----------

// 与第 11 篇同款：读某个 skill 的完整 SKILL.md 正文。
func loadSkill(args map[string]any) string {
	name, _ := args["name"].(string)
	body, err := os.ReadFile(filepath.Join("skills", name, "SKILL.md"))
	if err != nil {
		return "错误：没有名为 " + name + " 的 skill"
	}
	return string(body)
}

var toolRegistry = map[string]func(map[string]any) string{
	"search_docs": searchDocs,
	"load_skill":  loadSkill,
}

var toolDefs = []Tool{
	{Type: "function", Function: FunctionDefinition{
		Name:        "search_docs",
		Description: "检索知识库里的事实（退款政策、会员规则等）。当你缺的是一个「事实」——某个信息是什么、政策怎么规定——时调用。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "要检索的关键词或问题"},
			},
			"required": []string{"query"},
		},
	}},
	{Type: "function", Function: FunctionDefinition{
		Name:        "load_skill",
		Description: "读取某个 skill 的完整操作说明（怎么一步步把某类活儿做对）。当你缺的是「手艺/流程」而不是事实时调用。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "skill 名字，取自可用 skills 目录"},
			},
			"required": []string{"name"},
		},
	}},
}

// ---------- 调用 LLM（同第 02 篇）----------

func callLLM(messages []Message) (*ChatResponse, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	reqBody := ChatRequest{Model: "gpt-5.4-mini", Messages: messages, Tools: toolDefs}
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

// ---------- Agent 主循环：系统提示同时给出「可检索的文档」和「可用的 skill」----------

func runAgent(userInput string) (string, error) {
	sys := "你是一个助手，手上有两类外部资源：\n" +
		"1) 知识库（用 search_docs 检索事实）：退款政策、会员等级规则。\n" +
		"2) skills（用 load_skill 读操作流程）：\n" +
		"   - commit-message: 写规范的 Git commit message\n" +
		"   - refund-triage: 判断一笔退款能不能退的处理流程\n" +
		"判断你缺的是「事实」就检索，缺的是「手艺/流程」就读 skill，两者都缺就都用。"
	messages := []Message{
		{Role: "system", Content: sys},
		{Role: "user", Content: userInput},
	}
	const maxIterations = 10
	for i := 0; i < maxIterations; i++ {
		resp, err := callLLM(messages)
		if err != nil {
			return "", err
		}
		choice := resp.Choices[0]
		messages = append(messages, choice.Message)
		if choice.FinishReason != "tool_calls" || len(choice.Message.ToolCalls) == 0 {
			return choice.Message.Content, nil
		}
		for _, call := range choice.Message.ToolCalls {
			fmt.Printf("  [调用] %s(%s)\n", call.Function.Name, call.Function.Arguments)
			var args map[string]any
			json.Unmarshal([]byte(call.Function.Arguments), &args)
			result := "错误：未知工具 " + call.Function.Name
			if fn, ok := toolRegistry[call.Function.Name]; ok {
				result = fn(args)
			}
			preview := result
			if len(preview) > 60 {
				preview = preview[:60] + "…"
			}
			fmt.Printf("  [结果] %s\n", strings.ReplaceAll(preview, "\n", " "))
			messages = append(messages, Message{Role: "tool", ToolCallID: call.ID, Content: result})
		}
	}
	return "", fmt.Errorf("超过最大轮数 %d", maxIterations)
}

func main() {
	queries := []string{
		"金卡会员退款有 7 天限制吗？",                 // 纯事实 → 只走 search_docs
		"帮我写一条这次改动的 commit message：修复 token 过期后未刷新导致的 401", // 纯手艺 → 只走 load_skill
		"客户下单第 9 天要退款，是金卡，帮我判断能不能退并说明依据",       // 两者都要
	}
	for _, q := range queries {
		fmt.Printf("\n用户：%s\n", q)
		ans, err := runAgent(q)
		if err != nil {
			fmt.Println("出错：", err)
			continue
		}
		fmt.Printf("Agent：%s\n", ans)
	}
}
