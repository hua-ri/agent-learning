package main

// 可暂停 / 可恢复的 Agent 循环：在高风险工具前停下，把状态存盘、进程退出，
// 等人类批准后由「另一次进程调用」从磁盘恢复继续。
//
// 这才是 Human-in-the-loop 的真身——审批可能等几小时几天，你不能让进程一直挂着。
// 唯一稳妥的办法：序列化状态 → 退出 → 人类响应后从存储恢复。
//
// 本 demo 完全离线（用一个确定性的「脚本化 planner」冒充 LLM），
// 因为要演示的是 checkpoint/resume 机制，不是模型有多聪明。
//
// 用法（注意每条是「独立的进程调用」，中间进程真的退出了）：
//   go run .            # 开新会话，跑到审批点存盘退出
//   go run . status     # 看当前 checkpoint
//   go run . approve    # 批准，从盘上恢复继续
//   go run . reject     # 拒绝，从盘上恢复继续
//
// 关键设计点（都来自业界最佳实践）：
//   1) 存的是「待执行工具调用」而非只有对话历史——「对话恢复 ≠ 执行恢复」
//   2) 工具幂等：崩溃在「执行了但没存结果」之间，恢复会重跑，靠 already-done 标记兜底
//   3) 按「可逆性 + 影响面」分级设卡，且在 harness 层强制，不靠提示词
//   4) 进程真的退出，由另一次调用恢复——这才是持久化，区别于阻塞式等待

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

const checkpointFile = "checkpoint.json"
const approvalThreshold = 1000.0 // 退款 >= 1000 元需人工审批

// ---------- 状态：可 JSON 序列化，不含任何连接/句柄 ----------

type Checkpoint struct {
	SessionID string     `json:"session_id"`
	Task      string     `json:"task"`
	Cursor    int        `json:"cursor"`  // 走到脚本 plan 的第几步
	Audit     []string   `json:"audit"`   // 审计轨迹：谁在什么时候做了什么
	Refunded  bool       `json:"refunded"` // 幂等标记：退款是否已执行
	Pending   *Pending   `json:"pending,omitempty"`
}

type Pending struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

// ---------- 脚本化 planner：冒充 LLM 产出的工具调用序列 ----------

type step struct {
	tool string
	args map[string]any
	// risky: 是否属于需按阈值判断审批的高风险工具
	risky bool
}

var plan = []step{
	{tool: "lookup_order", args: map[string]any{"order": "A123"}, risky: false},          // 只读，绿区，自动
	{tool: "refund", args: map[string]any{"order": "A123", "amount": 1200.0}, risky: true}, // 写钱，按阈值判断
	{tool: "notify_user", args: map[string]any{"order": "A123"}, risky: false},            // 通知本人，绿区，自动
}

// ---------- 工具执行（幂等）----------

func execTool(cp *Checkpoint, s step) {
	switch s.tool {
	case "lookup_order":
		cp.Audit = append(cp.Audit, logline("执行 lookup_order(order=A123) → 订单存在，已付款 1200 元"))
	case "refund":
		if cp.Refunded { // 幂等：已退过就跳过，防恢复时重跑
			cp.Audit = append(cp.Audit, logline("refund 已执行过，幂等跳过"))
			return
		}
		amt := s.args["amount"].(float64)
		cp.Refunded = true
		cp.Audit = append(cp.Audit, logline(fmt.Sprintf("执行 refund(order=A123, amount=%g) → 已退款", amt)))
	case "notify_user":
		cp.Audit = append(cp.Audit, logline("执行 notify_user(order=A123) → 已通知用户"))
	}
}

func logline(msg string) string {
	return time.Now().Format("15:04:05") + " " + msg
}

// needApproval 在 harness 层判定是否需要人工卡点：可逆性 + 影响面。
func needApproval(s step) bool {
	if !s.risky {
		return false
	}
	if s.tool == "refund" {
		return s.args["amount"].(float64) >= approvalThreshold
	}
	return true
}

// ---------- 循环：从 cursor 往后跑，遇到审批点存盘退出 ----------

func runLoop(cp *Checkpoint) {
	for cp.Cursor < len(plan) {
		s := plan[cp.Cursor]
		if needApproval(s) && cp.Pending == nil {
			// 到达审批点：记下待执行调用，存盘，退出进程。
			cp.Pending = &Pending{Tool: s.tool, Args: s.args}
			save(cp)
			fmt.Printf("\n⏸  需要人工审批：%s(%v)\n", s.tool, s.args)
			fmt.Printf("   已存盘到 %s，进程退出。\n", checkpointFile)
			fmt.Printf("   批准：go run . approve    拒绝：go run . reject\n")
			return // 进程结束——不是 sleep，是真的退出
		}
		execTool(cp, s)
		cp.Cursor++
		save(cp) // 每步后存盘：崩溃了重载即可，最多重跑一步（靠幂等兜底）
	}
	// 全部走完
	cp.Audit = append(cp.Audit, logline("任务完成"))
	printAudit(cp)
	os.Remove(checkpointFile)
	fmt.Println("\n✅ 会话结束，checkpoint 已清理。")
}

// ---------- checkpoint 读写 ----------

func save(cp *Checkpoint) {
	b, _ := json.MarshalIndent(cp, "", "  ")
	os.WriteFile(checkpointFile, b, 0644)
}

func load() (*Checkpoint, bool) {
	b, err := os.ReadFile(checkpointFile)
	if err != nil {
		return nil, false
	}
	var cp Checkpoint
	json.Unmarshal(b, &cp)
	return &cp, true
}

func printAudit(cp *Checkpoint) {
	fmt.Printf("\n—— 审计轨迹（session=%s）——\n", cp.SessionID)
	for _, a := range cp.Audit {
		fmt.Println("  " + a)
	}
}

func main() {
	cmd := "start"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "start":
		if _, ok := load(); ok {
			fmt.Println("已有进行中的会话，先 go run . approve/reject 处理，或删掉 checkpoint.json。")
			return
		}
		cp := &Checkpoint{
			SessionID: fmt.Sprintf("s-%d", time.Now().Unix()),
			Task:      "给订单 A123 退款 1200 元并通知用户",
			Audit:     []string{logline("会话开始：" + "给订单 A123 退款 1200 元并通知用户")},
		}
		fmt.Printf("▶  新会话 %s：%s\n", cp.SessionID, cp.Task)
		runLoop(cp)

	case "status":
		cp, ok := load()
		if !ok {
			fmt.Println("没有进行中的会话。")
			return
		}
		b, _ := json.MarshalIndent(cp, "", "  ")
		fmt.Println(string(b))

	case "approve", "reject":
		cp, ok := load()
		if !ok || cp.Pending == nil {
			fmt.Println("没有待审批的操作。")
			return
		}
		p := cp.Pending
		if cmd == "reject" {
			cp.Audit = append(cp.Audit, logline(fmt.Sprintf("人工拒绝 %s(%v)，跳过该步", p.Tool, p.Args)))
			cp.Pending = nil
			cp.Cursor++ // 跳过被拒的步骤
			fmt.Printf("🚫 已拒绝 %s，恢复执行……\n", p.Tool)
			runLoop(cp)
			return
		}
		// 批准：从盘上恢复，执行被批准的这一步，再继续。
		fmt.Printf("👍 已批准 %s(%v)，从磁盘恢复执行……\n", p.Tool, p.Args)
		cp.Audit = append(cp.Audit, logline(fmt.Sprintf("人工批准 %s(%v)", p.Tool, p.Args)))
		execTool(cp, plan[cp.Cursor])
		cp.Cursor++
		cp.Pending = nil
		save(cp)
		runLoop(cp)

	default:
		fmt.Println("未知命令。用法：go run . [start|status|approve|reject]")
	}
}
