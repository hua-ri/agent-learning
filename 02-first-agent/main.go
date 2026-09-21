package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// ---------- 数据结构：OpenAI Chat Completions 格式 ----------

// Message 对应 messages 数组里的一条消息
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // assistant 想调用的工具
	ToolCallID string     `json:"tool_call_id,omitempty"` // role=tool 时，回指哪次调用
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // 注意：是 JSON 字符串，不是对象！
}

// Tool 是给模型看的"工具说明书"
type Tool struct {
	Type     string             `json:"type"` // 固定 "function"
	Function FunctionDefinition `json:"function"`
}

type FunctionDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"` // JSON Schema
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

// ---------- 工具：实现 + 声明 ----------

// 工具 1：返回当前时间
func getCurrentTime(args map[string]any) string {
	return time.Now().Format("2006-01-02 15:04:05")
}

// 工具 2：把两个数字相加
func calculate(args map[string]any) string {
	a, _ := args["a"].(float64) // JSON 数字解析到 Go 里是 float64
	b, _ := args["b"].(float64)
	return fmt.Sprintf("%v", a+b)
}

// 注册表：工具名 -> 执行函数
var toolRegistry = map[string]func(map[string]any) string{
	"get_current_time": getCurrentTime,
	"calculate":        calculate,
}

// 声明：告诉模型有哪些工具、怎么用
var toolDefs = []Tool{
	{
		Type: "function",
		Function: FunctionDefinition{
			Name:        "get_current_time",
			Description: "获取当前的日期和时间，当用户询问现在几点、今天几号时使用",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		},
	},
	{
		Type: "function",
		Function: FunctionDefinition{
			Name:        "calculate",
			Description: "计算两个数字 a 与 b 的和",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"a": map[string]any{"type": "number", "description": "第一个加数"},
					"b": map[string]any{"type": "number", "description": "第二个加数"},
				},
				"required": []string{"a", "b"},
			},
		},
	},
}

// ---------- 调用 LLM ----------

func callLLM(messages []Message) (*ChatResponse, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}

	reqBody := ChatRequest{
		Model:    "gpt-5.4-mini",
		Messages: messages,
		Tools:    toolDefs, // 每一轮都带上工具声明
	}
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
	var chatResp ChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return nil, fmt.Errorf("解析失败: %v, 原始响应: %s", err, string(body))
	}
	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("无返回, 原始响应: %s", string(body))
	}
	return &chatResp, nil
}

// ---------- Agent 主循环：Agent 的发动机 ----------

func runAgent(userInput string) (string, error) {
	messages := []Message{{Role: "user", Content: userInput}}

	const maxIterations = 10 // 安全阀：防止无限循环
	for i := 0; i < maxIterations; i++ {
		resp, err := callLLM(messages)
		if err != nil {
			return "", err
		}
		choice := resp.Choices[0]

		// 关键：无论如何都先把 assistant 消息原样追加进历史
		messages = append(messages, choice.Message)

		// 没有要求调工具 => 这是最终答案，结束
		if choice.FinishReason != "tool_calls" || len(choice.Message.ToolCalls) == 0 {
			return choice.Message.Content, nil
		}

		// 逐个执行工具调用（一次可能有多个 = 并行调用）
		for _, call := range choice.Message.ToolCalls {
			fmt.Printf("  [调用] %s(%s)\n", call.Function.Name, call.Function.Arguments)

			var args map[string]any
			json.Unmarshal([]byte(call.Function.Arguments), &args) // arguments 是字符串，先解析

			result := "错误：未知工具 " + call.Function.Name
			if fn, ok := toolRegistry[call.Function.Name]; ok {
				result = fn(args)
			}
			fmt.Printf("  [结果] %s\n", result)

			// 把结果喂回去，tool_call_id 必须精确匹配
			messages = append(messages, Message{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    result,
			})
		}
		// 继续循环：带着工具结果再问一次模型
	}
	return "", fmt.Errorf("超过最大轮数 %d", maxIterations)
}

func main() {
	for _, q := range []string{"现在几点了？", "帮我算一下 1234 加 5678"} {
		fmt.Printf("\n用户：%s\n", q)
		answer, err := runAgent(q)
		if err != nil {
			fmt.Println("出错：", err)
			continue
		}
		fmt.Printf("Agent：%s\n", answer)
	}
}
