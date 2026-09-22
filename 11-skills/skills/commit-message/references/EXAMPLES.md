# Commit Message 正反例

## 好例子

```
fix(qdrant): upsert 改用 PUT 而非 POST

POST /collections/{}/points 会命中「按 ids 删除」的处理器，
报 missing field `ids`。upsert 必须用 PUT。
```

```
feat(auth): 支持飞书扫码登录

原有账密登录在内网频繁掉线，扫码登录复用企业身份，
减少一套独立密码体系的维护成本。
```

## 反例（别这么写）

- `update code` —— 没说改了什么、为什么。
- `修复了若干 bug` —— 过去式 + 一次混多件事。
- `fix: 改了 upsert 的方法，然后又顺手格式化了整个文件` —— 混合无关改动。
