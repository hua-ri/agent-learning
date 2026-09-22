---
name: commit-message
description: 写规范的 Git commit message。当用户要提交代码、生成 commit 信息、整理本次改动说明时使用。
---

# 写一条规范的 Commit Message

按 Conventional Commits 规范产出，格式如下：

```
<type>(<scope>): <subject>

<body>
```

## 步骤

1. 判断改动类型 `type`：`fix`（修 bug）/ `feat`（新功能）/ `refactor`（重构）/ `docs`（文档）/ `test`（测试）/ `chore`（杂项）。
2. `scope` 填受影响的模块名（可选），`subject` 用祈使句、不超过 50 字、句末不加句号。
3. `body` 解释「为什么」而不是「改了什么」，每行不超过 72 字。

## 硬规则

- subject 必须是祈使句（"修复" 而非 "修复了"）。
- 一次只描述一件事，混合改动请拆多条。
- 需要更多正反例时，读 `references/EXAMPLES.md`（只有需要时才读）。
