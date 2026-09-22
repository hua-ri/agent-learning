package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

func apiBase() (string, string) {
	base := os.Getenv("OPENAI_BASE_URL")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	return base, os.Getenv("OPENAI_API_KEY")
}

func postJSON(url string, auth string, reqBody any) ([]byte, error) {
	buf, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func embed(texts []string) ([][]float64, error) {
	base, key := apiBase()
	model := os.Getenv("EMBEDDING_MODEL")
	if model == "" {
		model = "text-embedding-3-large"
	}
	body, err := postJSON(base+"/embeddings", key, map[string]any{
		"model":           model,
		"input":           texts,
		"encoding_format": "float", // 有些网关默认返回 base64，显式要 float 数组
	})
	if err != nil {
		return nil, err
	}
	var er struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &er); err != nil {
		return nil, fmt.Errorf("embedding 响应解析失败（原始前 300 字）：%.300s", body)
	}
	// 按输入顺序取（OpenAI 保证 data 与 input 顺序一致），不依赖 index 字段：
	// 有些网关不回 index、全默认 0，会导致向量互相覆盖、留下 nil，最终被 Qdrant 拒绝
	if len(er.Data) != len(texts) {
		return nil, fmt.Errorf("embedding 数量不对：输入 %d 条，只返回 %d 条（多半是网关截断/限流）；原始前 300 字：%.300s", len(texts), len(er.Data), body)
	}
	dim := len(er.Data[0].Embedding)
	if dim == 0 {
		return nil, fmt.Errorf("首条 embedding 为空，网关可能没真正返回向量；原始前 300 字：%.300s", body)
	}
	vecs := make([][]float64, len(er.Data))
	for i, d := range er.Data {
		if len(d.Embedding) != dim {
			return nil, fmt.Errorf("第 %d 条 embedding 维度 %d ≠ 首条 %d（别混用不同模型/维度）", i, len(d.Embedding), dim)
		}
		vecs[i] = d.Embedding
	}
	return vecs, nil
}

func qdrant() string {
	if u := os.Getenv("QDRANT_URL"); u != "" {
		return u
	}
	return "http://localhost:6333"
}

const collection = "rag"

func createCollection(dim int) error {
	buf, _ := json.Marshal(map[string]any{
		"vectors": map[string]any{"size": dim, "distance": "Cosine"},
	})
	req, _ := http.NewRequest("PUT", qdrant()+"/collections/"+collection, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func upsert(texts []string, vecs [][]float64) error {
	points := make([]map[string]any, len(texts))
	for i := range texts {
		points[i] = map[string]any{
			"id":      i,
			"vector":  vecs[i],
			"payload": map[string]any{"text": texts[i]},
		}
	}
	// 注意：Qdrant 的 upsert 是 PUT /collections/{}/points；
	// 用 POST 会命中「按 ids 删除/更新」的处理器，报 missing field `ids`
	buf, _ := json.Marshal(map[string]any{"points": points})
	req, _ := http.NewRequest("PUT", qdrant()+"/collections/"+collection+"/points?wait=true", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "\"status\":\"ok\"") {
		return fmt.Errorf("写入失败: %s", body)
	}
	return nil
}

func search(qVec []float64, topK int) ([]string, error) {
	body, err := postJSON(qdrant()+"/collections/"+collection+"/points/search", "",
		map[string]any{"vector": qVec, "limit": topK, "with_payload": true})
	if err != nil {
		return nil, err
	}
	var sr struct {
		Result []struct {
			Score   float64 `json:"score"`
			Payload struct {
				Text string `json:"text"`
			} `json:"payload"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("检索失败: %s", body)
	}
	var out []string
	for _, r := range sr.Result {
		out = append(out, r.Payload.Text)
	}
	return out, nil
}

func generate(query string, contexts []string) (string, error) {
	base, key := apiBase()
	model := os.Getenv("CHAT_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}
	var sb strings.Builder
	for i, c := range contexts {
		fmt.Fprintf(&sb, "[%d] %s\n", i, c)
	}
	sys := "你只依据下面的资料回答，句末用 [编号] 标来源；资料没提到的，直接说“资料中未提及”。\n\n资料：\n" + sb.String()
	body, err := postJSON(base+"/chat/completions", key, map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": sys},
			{"role": "user", "content": query},
		},
	})
	if err != nil {
		return "", err
	}
	var cr struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &cr); err != nil || len(cr.Choices) == 0 {
		return "", fmt.Errorf("生成失败: %s", body)
	}
	return cr.Choices[0].Message.Content, nil
}

func main() {
	docs := []string{
		"支付异常排查：先检查第三方支付渠道回调是否正常，再看订单状态机是否卡在 PENDING",
		"数据库死锁通常由多个事务加锁顺序不一致导致，统一加锁顺序即可解决",
		"服务内存持续增长最终 OOM，多半是 goroutine 泄漏或未释放的缓存",
		"订单号 40021 的退款需人工审核，因为金额超过自动退款阈值",
	}

	vecs, err := embed(docs)
	if err != nil {
		fmt.Println("向量化失败（检查模型环境变量 / Ollama 是否在跑）：", err)
		return
	}

	if err := createCollection(len(vecs[0])); err != nil {
		fmt.Println("建 collection 失败（检查 Qdrant 是否启动）：", err)
		return
	}
	if err := upsert(docs, vecs); err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("已写入 %d 条向量到 Qdrant（维度 %d）\n", len(docs), len(vecs[0]))

	query := "用户反馈付款失败，怎么排查？"
	qVec, err := embed([]string{query})
	if err != nil {
		fmt.Println(err)
		return
	}
	hits, err := search(qVec[0], 2)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("检索到：", hits)

	answer, err := generate(query, hits)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("\n问题：%s\n答案：%s\n", query, answer)
}
