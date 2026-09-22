package main

// 面向 Agent 的 Prompt Engineering：结构化输出实战。
// 同一个任务——把一条客服工单抽取成结构化字段（优先级/类别/是否需人工）——
// 用三种方式要 JSON，让你看清区别：
//
//   A. 裸提示词要 JSON        只在 prompt 里说"输出 JSON"，模型可能夹带解释、```json 围栏
//   B. strict json_schema     约束解码，模型在解码层就吐不出违反 schema 的 token，100% 合法
//   C. reason-first 字段顺序   schema 里把 reasoning 字段放在结论字段之前，
//                             逼模型先推理再下结论，规避"格式税"（先填结论再补理由 → 推理质量下降）
//
// 需要 OPENAI_API_KEY。用法：go run .

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ResponseFormat 承载 Structured Outputs：strict json_schema。
type ResponseFormat struct {
	Type       string     `json:"type"`
	JSONSchema *JSONSchema `json:"json_schema,omitempty"`
}

type JSONSchema struct {
	Name   string         `json:"name"`
	Strict bool           `json:"strict"`
	Schema map[string]any `json:"schema"`
}

type ChatRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
}

type ChatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
}

func callLLM(req ChatRequest) (string, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	req.Model = "gpt-5.4-mini"
	buf, _ := json.Marshal(req)
	httpReq, _ := http.NewRequest("POST", baseURL+"/chat/completions", bytes.NewReader(buf))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var cr ChatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return "", fmt.Errorf("解析失败: %v, 原始响应: %s", err, body)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("无返回, 原始响应: %s", body)
	}
	return cr.Choices[0].Message.Content, nil
}

const ticket = "工单：付款已扣但订单显示未支付，我下午三点前必须发货，急！已经打了两次客服电话没人接。"

// isValidJSON 判断一段文本是不是"干净的 JSON"（不含围栏/解释）。
func isValidJSON(s string) bool {
	var v any
	return json.Unmarshal([]byte(s), &v) == nil
}

func main() {
	if os.Getenv("OPENAI_API_KEY") == "" {
		fmt.Println("未设置 OPENAI_API_KEY。本 demo 演示三种结构化输出方式，需真实调用 LLM。")
		fmt.Println("设置后运行：go run .")
		return
	}

	// ---------- A. 裸提示词要 JSON：能用，但不保证干净 ----------
	fmt.Println("========== A. 裸提示词要 JSON ==========")
	out, err := callLLM(ChatRequest{Messages: []Message{
		{Role: "system", Content: "你是工单分类助手。把工单抽取成 JSON，含 priority(high/medium/low)、category、needs_human(bool)。"},
		{Role: "user", Content: ticket},
	}})
	if err != nil {
		fmt.Println("出错：", err)
		return
	}
	fmt.Printf("原始输出：\n%s\n是干净 JSON 吗？%v（裸提示常夹带 ```json 围栏或解释，需要自己剥）\n\n", out, isValidJSON(out))

	// ---------- B. strict json_schema：约束解码，保证 100% 合法 ----------
	fmt.Println("========== B. strict json_schema（Structured Outputs）==========")
	schemaB := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"priority":    map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}, "description": "紧急程度"},
			"category":    map[string]any{"type": "string", "description": "问题类别，如 支付/物流/账号"},
			"needs_human": map[string]any{"type": "boolean", "description": "是否需要转人工"},
		},
		"required":             []string{"priority", "category", "needs_human"},
		"additionalProperties": false,
	}
	out, err = callLLM(ChatRequest{
		Messages: []Message{
			{Role: "system", Content: "你是工单分类助手。"},
			{Role: "user", Content: ticket},
		},
		ResponseFormat: &ResponseFormat{Type: "json_schema", JSONSchema: &JSONSchema{Name: "ticket_triage", Strict: true, Schema: schemaB}},
	})
	if err != nil {
		fmt.Println("出错：", err)
		return
	}
	fmt.Printf("原始输出：\n%s\n是干净 JSON 吗？%v（约束解码：模型在解码层就吐不出违反 schema 的 token）\n\n", out, isValidJSON(out))

	// ---------- C. reason-first 字段顺序：先推理再下结论，规避"格式税" ----------
	fmt.Println("========== C. reason-first 字段顺序（规避格式税）==========")
	// 关键：reasoning 字段排在 priority/needs_human 之前。
	// 因为模型是从左到右生成的，字段顺序 = 思考顺序。先填结论会让它"没想清就下判断"。
	schemaC := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"reasoning":   map[string]any{"type": "string", "description": "先分析工单里的关键信号（紧急词、金额风险、情绪）"},
			"priority":    map[string]any{"type": "string", "enum": []string{"high", "medium", "low"}},
			"category":    map[string]any{"type": "string"},
			"needs_human": map[string]any{"type": "boolean"},
		},
		"required":             []string{"reasoning", "priority", "category", "needs_human"},
		"additionalProperties": false,
	}
	out, err = callLLM(ChatRequest{
		Messages: []Message{
			{Role: "system", Content: "你是工单分类助手。"},
			{Role: "user", Content: ticket},
		},
		ResponseFormat: &ResponseFormat{Type: "json_schema", JSONSchema: &JSONSchema{Name: "ticket_triage_cot", Strict: true, Schema: schemaC}},
	})
	if err != nil {
		fmt.Println("出错：", err)
		return
	}
	fmt.Printf("原始输出：\n%s\n（reasoning 在前 → 模型先推理再下结论，判断更稳）\n", out)
}
