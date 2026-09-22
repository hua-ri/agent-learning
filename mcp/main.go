package main

// 手搓 MCP client，并接到第 02 篇的 Agent 循环上：
//   1) 以子进程方式拉起 server（stdio transport）
//   2) initialize 握手 → notifications/initialized
//   3) tools/list 发现工具 → 转成 OpenAI function calling 的工具声明
//   4) Agent 循环里模型要调工具时，走 tools/call 打到 MCP server，再把结果喂回
//
// 不设 OPENAI_API_KEY 也能跑：会先做一次「离线自检」，只走 MCP 握手+列表+调用，
// 证明协议层通了；有 key 才继续跑完整的 Agent 循环。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

// ---------- MCP client：一问一答的 JSON-RPC over stdio ----------

type mcpClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	nextID int
}

// startServer 把 server 当子进程拉起来，接管它的 stdin/stdout 当作协议管道。
func startServer() (*mcpClient, error) {
	cmd := exec.Command("go", "run", "./server")
	cmd.Stderr = os.Stderr // server 的日志透传到我们的 stderr
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &mcpClient{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}, nil
}

// call 发一个带 id 的请求，同步读回它的响应。
func (c *mcpClient) call(method string, params any) (json.RawMessage, error) {
	c.nextID++
	req := map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	c.stdin.Write(append(b, '\n')) // 一行一个报文
	line, err := c.stdout.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(line, &resp)
	if resp.Error != nil {
		return nil, fmt.Errorf("MCP 错误 %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}

// notify 发一个通知（无 id，不等回复）。
func (c *mcpClient) notify(method string) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	c.stdin.Write(append(b, '\n'))
}

func (c *mcpClient) initialize() error {
	_, err := c.call("initialize", map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "demo-client", "version": "0.1.0"},
	})
	if err != nil {
		return err
	}
	c.notify("notifications/initialized") // 告诉 server：握手完成，可以干活了
	return nil
}

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func (c *mcpClient) listTools() ([]mcpToolDef, error) {
	res, err := c.call("tools/list", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []mcpToolDef `json:"tools"`
	}
	json.Unmarshal(res, &out)
	return out.Tools, nil
}

func (c *mcpClient) callTool(name string, args map[string]any) (string, error) {
	res, err := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", err // 协议错误（如未知工具）
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	json.Unmarshal(res, &out)
	var sb strings.Builder
	for _, ct := range out.Content {
		sb.WriteString(ct.Text)
	}
	return sb.String(), nil
}

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

// toOpenAITools 把 MCP 的工具声明直接翻成 OpenAI function calling 的格式。
// 注意 inputSchema 和 OpenAI 的 parameters 都是 JSON Schema，几乎一一对应。
func toOpenAITools(mts []mcpToolDef) []Tool {
	var out []Tool
	for _, mt := range mts {
		out = append(out, Tool{Type: "function", Function: FunctionDefinition{
			Name:        mt.Name,
			Description: mt.Description,
			Parameters:  mt.InputSchema,
		}})
	}
	return out
}

func callLLM(messages []Message, toolDefs []Tool) (*ChatResponse, error) {
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

// runAgent：第 02 篇的循环，唯一区别是工具来自 MCP、执行也打回 MCP。
func runAgent(userInput string, mcpTools []mcpToolDef, client *mcpClient) (string, error) {
	toolDefs := toOpenAITools(mcpTools)
	messages := []Message{
		{Role: "system", Content: "你是一个助手，可以调用工具。工具来自一个 MCP server。"},
		{Role: "user", Content: userInput},
	}
	for i := 0; i < 10; i++ {
		resp, err := callLLM(messages, toolDefs)
		if err != nil {
			return "", err
		}
		choice := resp.Choices[0]
		messages = append(messages, choice.Message)
		if choice.FinishReason != "tool_calls" || len(choice.Message.ToolCalls) == 0 {
			return choice.Message.Content, nil
		}
		for _, tc := range choice.Message.ToolCalls {
			var args map[string]any
			json.Unmarshal([]byte(tc.Function.Arguments), &args)
			fmt.Printf("  [MCP tools/call] %s(%s)\n", tc.Function.Name, tc.Function.Arguments)
			result, err := client.callTool(tc.Function.Name, args)
			if err != nil {
				result = err.Error()
			}
			fmt.Printf("  [MCP 结果] %s\n", result)
			messages = append(messages, Message{Role: "tool", ToolCallID: tc.ID, Content: result})
		}
	}
	return "", fmt.Errorf("超过最大轮数")
}

func main() {
	client, err := startServer()
	if err != nil {
		fmt.Println("启动 server 失败：", err)
		return
	}
	defer client.cmd.Process.Kill()

	// —— 握手 + 发现工具 ——
	if err := client.initialize(); err != nil {
		fmt.Println("initialize 失败：", err)
		return
	}
	mcpTools, err := client.listTools()
	if err != nil {
		fmt.Println("tools/list 失败：", err)
		return
	}
	fmt.Printf("从 MCP server 发现 %d 个工具：\n", len(mcpTools))
	for _, t := range mcpTools {
		fmt.Printf("  - %s: %s\n", t.Name, t.Description)
	}

	// —— 离线自检：不走 LLM，直接调一次工具，证明 MCP 协议层已打通 ——
	demo, err := client.callTool("get_weather", map[string]any{"city": "上海"})
	if err != nil {
		fmt.Println("离线自检失败：", err)
		return
	}
	fmt.Printf("\n[离线自检] 直接 tools/call get_weather(上海) → %s\n", demo)

	if os.Getenv("OPENAI_API_KEY") == "" {
		fmt.Println("\n(未设置 OPENAI_API_KEY，跳过 Agent 循环——但握手/发现/调用已验证 MCP 协议打通)")
		return
	}

	// —— 完整 Agent 循环：把 MCP 工具喂给 LLM，让它自己决定调哪个 ——
	fmt.Println("\n用户：上海现在天气怎么样？顺便告诉我现在几点")
	ans, err := runAgent("上海现在天气怎么样？顺便告诉我现在几点", mcpTools, client)
	if err != nil {
		fmt.Println("出错：", err)
		return
	}
	fmt.Printf("\nAgent：%s\n", ans)
}
