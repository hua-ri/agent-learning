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

// ---------- 数据结构：OpenAI Chat Completions 格式 ----------

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

// ---------- 工具抽象：声明 + 执行绑在一起 ----------

// ToolSpec 把"给模型的声明"和"实际执行的函数"绑在一起，加工具只改一处
type ToolSpec struct {
	Definition Tool
	Run        func(args map[string]any) (string, error)
}

// 沙箱根目录：只允许读这个目录下的文件（防呆：防止读到 /etc/passwd 之类）
const sandboxRoot = "./workspace"

// 真实工具 1：读文件（带防呆 + 输出控制 + 可操作错误）
func readFile(args map[string]any) (string, error) {
	name, _ := args["path"].(string)
	if name == "" {
		return "错误：缺少 path 参数", nil
	}
	clean := filepath.Clean(filepath.Join(sandboxRoot, name))
	if !strings.HasPrefix(clean, filepath.Clean(sandboxRoot)) {
		return "错误：只允许访问 workspace 目录内的文件", nil
	}
	data, err := os.ReadFile(clean)
	if err != nil {
		return fmt.Sprintf("读取失败：%s。请确认文件是否存在。", err), nil
	}
	const maxLen = 2000
	content := string(data)
	if len(content) > maxLen {
		content = content[:maxLen] + fmt.Sprintf("\n...（文件过长，共 %d 字节，已截断）", len(data))
	}
	return content, nil
}

// 真实工具 2：HTTP GET（带输出截断）
func httpGet(args map[string]any) (string, error) {
	url, _ := args["url"].(string)
	if url == "" {
		return "错误：缺少 url 参数", nil
	}
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Sprintf("请求失败：%s", err), nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4000)) // 最多读 4000 字节
	return fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, string(body)), nil
}

var tools = []ToolSpec{
	{
		Definition: Tool{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "read_file",
				Description: "读取 workspace 目录下的文本文件内容。当需要查看某个文件时使用。",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{
							"type":        "string",
							"description": "相对于 workspace 目录的文件路径，如 notes.txt",
						},
					},
					"required": []string{"path"},
				},
			},
		},
		Run: readFile,
	},
	{
		Definition: Tool{
			Type: "function",
			Function: FunctionDefinition{
				Name:        "http_get",
				Description: "对指定 URL 发起 HTTP GET 请求并返回响应内容。当需要获取网页或接口数据时使用。",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"url": map[string]any{"type": "string", "description": "完整的 URL，需以 http:// 或 https:// 开头"},
					},
					"required": []string{"url"},
				},
			},
		},
		Run: httpGet,
	},
}

// 构造给模型的工具声明列表
func toolDefinitions() []Tool {
	defs := make([]Tool, len(tools))
	for i, t := range tools {
		defs[i] = t.Definition
	}
	return defs
}

// 按名字查找并执行工具
func dispatch(name string, args map[string]any) string {
	for _, t := range tools {
		if t.Definition.Function.Name == name {
			result, err := t.Run(args)
			if err != nil {
				return fmt.Sprintf("[工具执行异常] %s", err)
			}
			return result
		}
	}
	return fmt.Sprintf("错误：未知工具 %s", name)
}

// ---------- 调用 LLM ----------

func callLLM(messages []Message) (*ChatResponse, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	reqBody := ChatRequest{Model: "gpt-5.4-mini", Messages: messages, Tools: toolDefinitions()}
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

// ---------- Agent 主循环 ----------

func runAgent(userInput string) (string, error) {
	messages := []Message{{Role: "user", Content: userInput}}
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
			result := dispatch(call.Function.Name, args)
			fmt.Printf("  [结果] %s\n", truncate(result, 120))
			messages = append(messages, Message{Role: "tool", ToolCallID: call.ID, Content: result})
		}
	}
	return "", fmt.Errorf("超过最大轮数 %d", maxIterations)
}

// 仅用于控制台日志的显示截断，不影响喂给模型的内容
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func main() {
	// 准备一个演示文件
	_ = os.MkdirAll(sandboxRoot, 0755)
	_ = os.WriteFile(filepath.Join(sandboxRoot, "notes.txt"), []byte("这是一份笔记：\n1. Agent 的核心是循环\n2. 工具设计决定能力上限"), 0644)

	for _, q := range []string{"读一下 notes.txt 里写了什么，并帮我总结一句话"} {
		fmt.Printf("\n用户：%s\n", q)
		answer, err := runAgent(q)
		if err != nil {
			fmt.Println("出错：", err)
			continue
		}
		fmt.Printf("Agent：%s\n", answer)
	}
}
