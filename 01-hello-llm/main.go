package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

// Message 对应 messages 数组里的一条消息
// role 取值：system / user / assistant
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest 是发给 LLM 的请求体，遵循 OpenAI Chat Completions 格式
type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
}

// ChatResponse 只解析我们关心的字段
type ChatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"` // 结束原因，后面会重点用到
	} `json:"choices"`
}

func main() {
	apiKey := os.Getenv("OPENAI_API_KEY")
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1" // 默认走 OpenAI 官方
	}

	// 1. 构造请求体：问一个"裸 LLM 答不了"的问题
	reqBody := ChatRequest{
		Model: "gpt-5.4-mini",
		Messages: []Message{
			{Role: "user", Content: "现在几点了？请直接告诉我当前的准确时间。"},
		},
	}
	buf, _ := json.Marshal(reqBody)

	// 2. 拼 HTTP 请求
	req, _ := http.NewRequest("POST", baseURL+"/chat/completions", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey) // 密钥放在 Authorization 头

	// 3. 发送
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	// 4. 解析响应
	body, _ := io.ReadAll(resp.Body)
	var chatResp ChatResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		fmt.Println("原始响应：", string(body)) // 出错时打印原始返回，方便排查
		panic(err)
	}
	if len(chatResp.Choices) == 0 {
		fmt.Println("原始响应：", string(body))
		panic("没有返回任何 choice，检查 API_KEY / BASE_URL / model 是否正确")
	}

	// 5. 打印结果
	fmt.Println("模型回答：", chatResp.Choices[0].Message.Content)
	fmt.Println("结束原因：", chatResp.Choices[0].FinishReason)
}
