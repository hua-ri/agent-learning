package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// SubAgent 一个聚焦的子 Agent：只负责一个子任务。
// 真实实现里 Run 内部是一整个 Agent 循环（见第 02-03 篇）：
// 会调用 LLM、用工具、查知识库。这里用一个可跑的桩演示编排骨架。
type SubAgent struct {
	Name string // 角色名，如 "子Agent-1"
}

// Run 执行子任务。模拟一次有耗时的独立工作。
func (a *SubAgent) Run(subTask string) string {
	time.Sleep(300 * time.Millisecond)
	return fmt.Sprintf("[%s] 关于「%s」的调研结论：……（此处为该子 Agent 独立产出）", a.Name, subTask)
}

// Orchestrator 主 Agent：负责拆任务、并行派发、汇总
type Orchestrator struct {
	MaxConcurrency int // 并发上限，0 表示不限
}

// Plan 把大任务拆成子任务。真实实现里这一步是让 LLM 输出结构化子任务列表，
// 这里为了聚焦并行骨架，用固定拆分演示。
func (o *Orchestrator) Plan(task string) []string {
	return []string{
		"云厂商 A 的 Serverless 价格与冷启动",
		"云厂商 B 的 Serverless 价格与冷启动",
		"云厂商 C 的 Serverless 价格与冷启动",
	}
}

// Run 并行派发子任务给子 Agent，收集所有结果后汇总
func (o *Orchestrator) Run(task string) string {
	subTasks := o.Plan(task)

	results := make([]string, len(subTasks))
	var wg sync.WaitGroup

	// 信号量限制并发数（0 表示不限）
	var sem chan struct{}
	if o.MaxConcurrency > 0 {
		sem = make(chan struct{}, o.MaxConcurrency)
	}

	for i, st := range subTasks {
		wg.Add(1)
		// 每个子任务一个独立 goroutine，并行执行
		go func(idx int, subTask string) {
			defer wg.Done()
			if sem != nil {
				sem <- struct{}{}        // 获取令牌，满了就阻塞等待
				defer func() { <-sem }() // 用完归还
			}
			agent := &SubAgent{Name: fmt.Sprintf("子Agent-%d", idx+1)}
			results[idx] = agent.Run(subTask) // 写各自的槽位，无需加锁
		}(i, st)
	}

	wg.Wait() // 等所有子 Agent 都完成

	return o.Aggregate(task, results)
}

// Aggregate 把子 Agent 的产出综合成最终答案。
// 真实实现里这里再调一次 LLM 把碎片综合成报告。
func (o *Orchestrator) Aggregate(task string, results []string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("任务「%s」的综合报告：\n", task))
	for _, r := range results {
		sb.WriteString("  - " + r + "\n")
	}
	sb.WriteString("（主 Agent 已将以上碎片综合成结论）")
	return sb.String()
}

func main() {
	orch := &Orchestrator{MaxConcurrency: 3}
	task := "调研三家云厂商的 Serverless 方案并给出选型建议"

	start := time.Now()
	report := orch.Run(task)
	elapsed := time.Since(start)

	fmt.Println(report)
	fmt.Printf("\n耗时：%v（3 个子任务并行，而非 3×300ms 串行）\n", elapsed)
}
