package main

import (
	"fmt"
	"strings"
)

// 复用前几篇的消息结构（精简版，够演示压缩逻辑即可）
type ToolCall struct {
	ID       string
	Function struct{ Name, Arguments string }
}

type Message struct {
	Role       string
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
}

// estimateTokens 粗略估算 token 数。
// ASCII 约 4 字符/token，非 ASCII（中文等）约 1.5 字符/token。
// 生产环境应换成官方 tokenizer（tiktoken）或用接口返回的 usage 反推。
func estimateTokens(text string) int {
	ascii, wide := 0, 0
	for _, r := range text {
		if r > 127 {
			wide++
		} else {
			ascii++
		}
	}
	return ascii/4 + wide*2/3 + 1
}

func totalTokens(messages []Message) int {
	sum := 0
	for _, m := range messages {
		sum += estimateTokens(m.Content)
		for _, tc := range m.ToolCalls {
			sum += estimateTokens(tc.Function.Arguments) + estimateTokens(tc.Function.Name)
		}
	}
	return sum
}

// CompactHistory 超过 token 预算时压缩历史：
// 保留第一条锚点 + 最近 keepRecent 条，中间压成一条摘要。
func CompactHistory(messages []Message, budget, keepRecent int) []Message {
	if totalTokens(messages) <= budget {
		return messages
	}
	if len(messages) <= keepRecent+1 {
		return messages
	}
	head := messages[:1]
	rest := messages[1:]
	if len(rest) <= keepRecent {
		return messages
	}
	dropped := rest[:len(rest)-keepRecent]
	recent := rest[len(rest)-keepRecent:]

	summary := Message{
		Role:    "system",
		Content: fmt.Sprintf("【历史摘要】此前进行了 %d 条交互（已省略细节以节省上下文）。", len(dropped)),
	}

	result := append([]Message{}, head...)
	result = append(result, summary)
	result = append(result, recent...)
	return result
}

func main() {
	// 构造一段很长的假历史：1 条系统提示 + 30 轮对话，每轮夹带一段长日志
	messages := []Message{
		{Role: "system", Content: "你是一个运维诊断助手，负责分析构建失败原因。"},
	}
	longLog := strings.Repeat("2026-09-21 ERROR 构建失败，依赖下载超时，重试中...\n", 20)
	for i := 1; i <= 30; i++ {
		messages = append(messages,
			Message{Role: "user", Content: fmt.Sprintf("第 %d 个问题：帮我看下这段日志", i)},
			Message{Role: "assistant", Content: "好的，我分析一下：\n" + longLog},
		)
	}

	before := totalTokens(messages)
	fmt.Printf("压缩前：%d 条消息，约 %d tokens\n", len(messages), before)

	const (
		budget     = 800 // token 预算（演示用，故意设小）
		keepRecent = 6   // 至少保留最近 6 条
	)
	compacted := CompactHistory(messages, budget, keepRecent)

	after := totalTokens(compacted)
	fmt.Printf("压缩后：%d 条消息，约 %d tokens\n", len(compacted), after)
	fmt.Printf("节省了约 %.0f%% 的上下文\n", float64(before-after)/float64(before)*100)

	fmt.Println("\n压缩后的消息结构：")
	for i, m := range compacted {
		preview := []rune(strings.ReplaceAll(m.Content, "\n", " "))
		if len(preview) > 30 {
			preview = append(preview[:30], []rune("...")...)
		}
		fmt.Printf("  [%d] %-9s %s\n", i, m.Role, string(preview))
	}
}
