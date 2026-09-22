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

// 这一篇的主角是「渐进式披露」：一个 skill 是一个装着 SKILL.md 的文件夹，
// 我们不把所有 skill 的正文都塞进上下文，而是分三级按需加载：
//   1) Discovery ：启动时只扫 name + description，建一份"目录"注入系统提示
//   2) Activation：模型判断某个 skill 相关时，调 load_skill 才读它的正文
//   3) Execution ：正文若指向 references/ 里的文件，用 read_reference 按需再读
// 复用第 02 篇的 Agent 循环，把 skill 加载做成两个工具挂上去。

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

// ---------- 第 1 级 Discovery：扫描 skills/，只读 frontmatter ----------

type Skill struct {
	Name        string
	Description string
	Dir         string // skill 文件夹路径，正文和 references 都在这里面
}

const skillsRoot = "skills"

// discoverSkills 遍历 skills/ 下每个子目录的 SKILL.md，只解析 YAML frontmatter。
// 关键：这里绝不读正文——正文可能几千 token，Discovery 只花 name+description 的钱。
func discoverSkills(root string) ([]Skill, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var skills []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		name, desc, ok := parseFrontmatter(filepath.Join(dir, "SKILL.md"))
		if !ok {
			continue // 没有合法 frontmatter 的目录直接跳过
		}
		skills = append(skills, Skill{Name: name, Description: desc, Dir: dir})
	}
	return skills, nil
}

// parseFrontmatter 手搓一个极简 YAML frontmatter 解析器（只认 name / description）。
// 生产里会用成熟库，这里为了不藏黑盒，手写让你看清「元数据」到底怎么来的。
func parseFrontmatter(path string) (name, desc string, ok bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}
	text := string(raw)
	if !strings.HasPrefix(text, "---") {
		return "", "", false
	}
	// 取第一段 --- 与第二段 --- 之间的内容
	rest := text[3:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", "", false
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		k, v, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch strings.TrimSpace(k) {
		case "name":
			name = strings.TrimSpace(v)
		case "description":
			desc = strings.TrimSpace(v)
		}
	}
	return name, desc, name != "" && desc != ""
}

// catalog 把所有 skill 的 name+description 拼成注入系统提示的"目录"。
func catalog(skills []Skill) string {
	var sb strings.Builder
	for _, s := range skills {
		fmt.Fprintf(&sb, "- %s: %s\n", s.Name, s.Description)
	}
	return sb.String()
}

// ---------- 第 2、3 级工具：Activation（读正文）+ Execution（按需读引用）----------

var skillIndex = map[string]Skill{} // name -> Skill，供工具查

// loadSkill = Activation：读某个 skill 的完整 SKILL.md 正文。
func loadSkill(args map[string]any) string {
	name, _ := args["name"].(string)
	s, ok := skillIndex[name]
	if !ok {
		return "错误：没有名为 " + name + " 的 skill"
	}
	body, err := os.ReadFile(filepath.Join(s.Dir, "SKILL.md"))
	if err != nil {
		return "错误：读取 SKILL.md 失败: " + err.Error()
	}
	return string(body)
}

// readReference = Execution：读 skill 目录下的引用文件（references/xxx.md）。
// 安全要点：把路径限制在该 skill 目录内，挡掉 ../../etc/passwd 这种路径穿越。
func readReference(args map[string]any) string {
	name, _ := args["skill"].(string)
	rel, _ := args["path"].(string)
	s, ok := skillIndex[name]
	if !ok {
		return "错误：没有名为 " + name + " 的 skill"
	}
	base, _ := filepath.Abs(s.Dir)
	target, _ := filepath.Abs(filepath.Join(s.Dir, rel))
	if !strings.HasPrefix(target, base+string(os.PathSeparator)) {
		return "错误：拒绝越界访问 " + rel // 路径穿越，挡掉
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return "错误：读取失败: " + err.Error()
	}
	return string(data)
}

var toolRegistry = map[string]func(map[string]any) string{
	"load_skill":     loadSkill,
	"read_reference": readReference,
}

var toolDefs = []Tool{
	{Type: "function", Function: FunctionDefinition{
		Name:        "load_skill",
		Description: "读取某个 skill 的完整操作说明（SKILL.md 正文）。当你判断某个 skill 与当前任务相关时调用它。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "skill 的名字，取自可用 skills 目录"},
			},
			"required": []string{"name"},
		},
	}},
	{Type: "function", Function: FunctionDefinition{
		Name:        "read_reference",
		Description: "读取某个 skill 目录下的引用文件（如 references/EXAMPLES.md）。只有 SKILL.md 正文让你读、且当前任务确实需要时才调用。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"skill": map[string]any{"type": "string", "description": "skill 名字"},
				"path":  map[string]any{"type": "string", "description": "相对该 skill 目录的路径，如 references/EXAMPLES.md"},
			},
			"required": []string{"skill", "path"},
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

// ---------- Agent 主循环：系统提示里带上 skill 目录 ----------

func runAgent(userInput, skillCatalog string) (string, error) {
	sys := "你是一个助手。你手上有下面这些 skill（只给了名字和用途），" +
		"当任务匹配某个 skill 时，先用 load_skill 读它的完整说明，再严格按说明执行；" +
		"说明里若让你读引用文件，用 read_reference 按需读。可用 skills：\n" + skillCatalog
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
	// —— 第 1 级 Discovery：启动只扫元数据 ——
	skills, err := discoverSkills(skillsRoot)
	if err != nil {
		fmt.Println("扫描 skills/ 失败：", err)
		return
	}
	for _, s := range skills {
		skillIndex[s.Name] = s
	}
	cat := catalog(skills)

	// 直观感受"渐进式披露"省了多少：目录 vs 全部正文
	catBytes := len(cat)
	fullBytes := 0
	for _, s := range skills {
		if b, e := os.ReadFile(filepath.Join(s.Dir, "SKILL.md")); e == nil {
			fullBytes += len(b)
		}
	}
	fmt.Printf("发现 %d 个 skill；目录占 %d 字节，全部正文占 %d 字节——启动只装目录，省下 %d 字节的上下文。\n\n",
		len(skills), catBytes, fullBytes, fullBytes-catBytes)
	fmt.Printf("可用 skills（注入系统提示的目录）：\n%s\n", cat)

	queries := []string{
		"帮我根据这次改动写一条 commit message：把 Qdrant 的 upsert 从 POST 改成了 PUT，修好了写入失败",
	}
	for _, q := range queries {
		fmt.Printf("用户：%s\n", q)
		ans, err := runAgent(q, cat)
		if err != nil {
			fmt.Println("出错：", err)
			continue
		}
		fmt.Printf("\nAgent：\n%s\n", ans)
	}
}
