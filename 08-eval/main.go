package main

import (
	"fmt"
	"strings"
)

// TestCase 一道评测题
type TestCase struct {
	ID       string
	Input    string   // 喂给 Agent 的输入
	Expected string   // 期望答案（规则评测用）
	Keywords []string // 期望答案必须包含的关键词（宽松匹配用）
}

// Result 一道题的评测结果
type Result struct {
	ID     string
	Pass   bool
	Score  float64
	Detail string
}

// scoreByRule 规则判分：期望答案完全匹配得 1 分；
// 否则按关键词命中比例给分，命中全部关键词也算通过。
func scoreByRule(expected string, keywords []string, actual string) Result {
	if strings.TrimSpace(actual) == strings.TrimSpace(expected) {
		return Result{Pass: true, Score: 1.0, Detail: "精确匹配"}
	}
	if len(keywords) == 0 {
		return Result{Pass: false, Score: 0, Detail: "不匹配且无关键词"}
	}
	hit := 0
	for _, kw := range keywords {
		if strings.Contains(actual, kw) {
			hit++
		}
	}
	score := float64(hit) / float64(len(keywords))
	return Result{
		Pass:   score >= 1.0, // 命中全部关键词才算过
		Score:  score,
		Detail: fmt.Sprintf("关键词命中 %d/%d", hit, len(keywords)),
	}
}

// crudeSimilarity 一个粗糙的字符重叠相似度，仅用于模拟 LLM-judge 的打分输出
func crudeSimilarity(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	setB := map[rune]bool{}
	for _, r := range rb {
		setB[r] = true
	}
	if len(ra) == 0 {
		return 0
	}
	hit := 0
	for _, r := range ra {
		if setB[r] {
			hit++
		}
	}
	return float64(hit) / float64(len(ra))
}

// scoreByLLMJudge 让另一个模型当裁判打分（0~1）。
// 真实实现：构造 judge 提示词（题目+标准答案+待评答案+评分标准），
// 调用一个更强的模型，要求它只输出结构化分数。这里用桩演示接口形态。
func scoreByLLMJudge(input, expected, actual string) Result {
	// judgePrompt := fmt.Sprintf(`你是严格的评分员。
	// 问题：%s
	// 参考答案：%s
	// 待评答案：%s
	// 请按「准确性」打分，只输出 0~1 之间的小数。`, input, expected, actual)
	// score := callJudgeLLM(judgePrompt)  // 真实场景在此调用 API

	score := crudeSimilarity(expected, actual)
	return Result{
		Pass:   score >= 0.6,
		Score:  score,
		Detail: fmt.Sprintf("LLM-judge 打分 %.2f（此为模拟）", score),
	}
}

// runEval 跑完整个 golden set，返回每题结果和总通过率
func runEval(cases []TestCase, agent func(string) string) ([]Result, float64) {
	results := make([]Result, 0, len(cases))
	passed := 0
	for _, c := range cases {
		actual := agent(c.Input) // 跑 Agent 拿实际输出
		r := scoreByRule(c.Expected, c.Keywords, actual)
		r.ID = c.ID
		if r.Pass {
			passed++
		}
		results = append(results, r)
	}
	passRate := float64(passed) / float64(len(cases))
	return results, passRate
}

// compareRegression 对比两次评测，找出退化的题（上次过、这次挂）
func compareRegression(prev, curr []Result) []string {
	prevPass := map[string]bool{}
	for _, r := range prev {
		prevPass[r.ID] = r.Pass
	}
	var regressed []string
	for _, r := range curr {
		if prevPass[r.ID] && !r.Pass { // 上次过，这次挂
			regressed = append(regressed, r.ID)
		}
	}
	return regressed
}

func printReport(title string, results []Result, passRate float64) {
	fmt.Printf("== %s ==\n", title)
	for _, r := range results {
		status := "FAIL"
		if r.Pass {
			status = "PASS"
		}
		fmt.Printf("  [%s] %-6s score=%.2f  %s\n", r.ID, status, r.Score, r.Detail)
	}
	fmt.Printf("  通过率：%.0f%%\n\n", passRate*100)
}

func main() {
	// golden set：标准题库
	cases := []TestCase{
		{ID: "q1", Input: "1+1 等于几", Expected: "2"},
		{ID: "q2", Input: "MySQL 死锁常见原因", Keywords: []string{"事务", "加锁", "顺序"}},
		{ID: "q3", Input: "把状态码 200 的含义说清楚", Keywords: []string{"成功"}},
	}

	// v1 版 Agent：表现良好
	agentV1 := func(input string) string {
		switch {
		case strings.Contains(input, "1+1"):
			return "2"
		case strings.Contains(input, "死锁"):
			return "多个事务加锁顺序不一致导致死锁，统一加锁顺序可解决"
		case strings.Contains(input, "200"):
			return "200 表示请求成功"
		}
		return "不知道"
	}

	// v2 版 Agent：改了提示词后 q2 退化了（漏了"顺序"关键词）
	agentV2 := func(input string) string {
		switch {
		case strings.Contains(input, "1+1"):
			return "2"
		case strings.Contains(input, "死锁"):
			return "多个事务加锁导致死锁" // 退化：漏了"顺序"
		case strings.Contains(input, "200"):
			return "200 表示请求成功"
		}
		return "不知道"
	}

	resV1, rateV1 := runEval(cases, agentV1)
	printReport("v1 评测", resV1, rateV1)

	resV2, rateV2 := runEval(cases, agentV2)
	printReport("v2 评测", resV2, rateV2)

	// 回归对比：找出从"过"变"挂"的题
	regressed := compareRegression(resV1, resV2)
	if len(regressed) > 0 {
		fmt.Printf("⚠ 检测到回归退化（上版通过、本版失败）：%v\n", regressed)
		fmt.Println("  → 这次改动让部分用例退化了，别急着上线！")
	} else {
		fmt.Println("无回归退化，可以上线。")
	}
}
