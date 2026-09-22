package main

// 一个从零手搓的 MCP server：JSON-RPC 2.0 over stdio，不依赖任何 SDK。
// 目的是让你看清 MCP 在线上到底传了什么字节。
//
// 铁律：stdout 是 JSON-RPC 通道，任何日志一律走 stderr——
// 往 stdout 打一行 log，就会把协议流搞脏，client 直接解析失败。
//
// 这里实现三个方法，构成一个最小可用的 MCP server：
//   initialize                  能力协商（我支持 tools）
//   tools/list                  把工具清单给 client
//   tools/call                  真正执行某个工具
// 外加一个通知 notifications/initialized（握手完成，无需回复）。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// ---------- JSON-RPC 2.0 三种报文：请求 / 响应 / 错误 ----------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // 通知（notification）没有 id
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ---------- 工具：一个函数 + 一份 JSON Schema 声明（和第 03 篇同构）----------

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

var tools = []mcpTool{
	{
		Name:        "get_weather",
		Description: "查询某个城市的当前天气",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city": map[string]any{"type": "string", "description": "城市名"},
			},
			"required": []string{"city"},
		},
	},
	{
		Name:        "get_time",
		Description: "返回服务器当前时间",
		InputSchema: map[string]any{"type": "object", "additionalProperties": false},
	},
}

// runTool 执行工具，返回 (文本结果, 是否为执行错误)。
func runTool(name string, args map[string]any) (text string, isErr bool, unknown bool) {
	switch name {
	case "get_weather":
		city, _ := args["city"].(string)
		if city == "" {
			return "错误：city 不能为空", true, false // 工具执行错误：喂回模型让它自纠
		}
		return fmt.Sprintf("%s：晴，24°C", city), false, false
	case "get_time":
		return time.Now().Format("2006-01-02 15:04:05"), false, false
	default:
		return "", true, true // 未知工具：走协议错误
	}
}

// ---------- stdio 主循环：一行一个 JSON-RPC 报文 ----------

func writeResp(w *bufio.Writer, resp rpcResponse) {
	resp.JSONRPC = "2.0"
	b, _ := json.Marshal(resp)
	w.Write(b)
	w.WriteByte('\n')
	w.Flush()
}

func main() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // 放大缓冲，容纳大消息
	out := bufio.NewWriter(os.Stdout)

	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue // 不是合法 JSON，忽略
		}
		isNotification := len(req.ID) == 0 // 没有 id 的是通知，不回复

		switch req.Method {
		case "initialize":
			// 能力协商：告诉 client 我说的协议版本、我支持 tools
			writeResp(out, rpcResponse{ID: req.ID, Result: map[string]any{
				"protocolVersion": "2025-11-25",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "demo-server", "version": "0.1.0"},
			}})
		case "notifications/initialized":
			// 握手完成的通知，无需回复
		case "tools/list":
			writeResp(out, rpcResponse{ID: req.ID, Result: map[string]any{"tools": tools}})
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			json.Unmarshal(req.Params, &p)
			text, isErr, unknown := runTool(p.Name, p.Arguments)
			if unknown {
				// 未知工具 → JSON-RPC 协议错误（-32602）
				writeResp(out, rpcResponse{ID: req.ID, Error: &rpcError{Code: -32602, Message: "Unknown tool: " + p.Name}})
				continue
			}
			// 正常/执行错误都放进 result.content，用 isError 区分
			writeResp(out, rpcResponse{ID: req.ID, Result: map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
				"isError": isErr,
			}})
		default:
			if !isNotification {
				writeResp(out, rpcResponse{ID: req.ID, Error: &rpcError{Code: -32601, Message: "Method not found: " + req.Method}})
			}
		}
	}
	fmt.Fprintln(os.Stderr, "server: stdin 关闭，退出")
}
