---
name: commit-message
description: 写规范的 Git commit message。当用户要提交代码、生成 commit 信息、整理本次改动说明时使用。
---

# 写一条规范的 Commit Message

按 Conventional Commits 规范产出，格式如下：

```
<type>(<scope>): <简短描述>

<可选正文：解释为什么这么改>
```

- type 取：feat / fix / docs / refactor / test / chore。
- 描述用一句话说清「改了什么、为什么」，动词开头，不加句号。
- 修 bug 要点明根因，别只写「改了代码」。
