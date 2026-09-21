package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
)

// cosineSimilarity 计算两个向量的余弦相似度，范围 -1~1，越接近 1 越相似
func cosineSimilarity(a, b []float64) float64 {
	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

type EmbeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"` // 支持一次转多段
}

type EmbeddingResponse struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
}

// embed 把多段文本批量转成向量
func embed(texts []string) ([][]float64, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	model := os.Getenv("EMBEDDING_MODEL")
	if model == "" {
		model = "text-embedding-3-large" // 默认模型，换成你网关支持的
	}

	buf, _ := json.Marshal(EmbeddingRequest{Model: model, Input: texts})
	req, _ := http.NewRequest("POST", baseURL+"/embeddings", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var er EmbeddingResponse
	if err := json.Unmarshal(body, &er); err != nil {
		return nil, fmt.Errorf("解析失败: %v, 原始响应: %s", err, string(body))
	}
	if len(er.Data) == 0 {
		return nil, fmt.Errorf("embedding 接口无返回，原始响应: %s", string(body))
	}
	// 按 index 排好序返回
	vecs := make([][]float64, len(er.Data))
	for _, d := range er.Data {
		vecs[d.Index] = d.Embedding
	}
	return vecs, nil
}

type Document struct {
	Text   string
	Vector []float64
}

type VectorStore struct {
	docs []Document
}

// Index 把文档批量转向量并存入（离线建库）
func (s *VectorStore) Index(texts []string) error {
	vecs, err := embed(texts)
	if err != nil {
		return err
	}
	for i, t := range texts {
		s.docs = append(s.docs, Document{Text: t, Vector: vecs[i]})
	}
	return nil
}

// Search 检索与 query 最相似的 topK 段（在线检索）
func (s *VectorStore) Search(query string, topK int) []string {
	qvec, err := embed([]string{query})
	if err != nil || len(qvec) == 0 {
		return nil
	}
	type scored struct {
		text string
		sim  float64
	}
	var ranked []scored
	for _, d := range s.docs {
		ranked = append(ranked, scored{d.Text, cosineSimilarity(qvec[0], d.Vector)})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].sim > ranked[j].sim })

	var result []string
	for i := 0; i < len(ranked) && i < topK; i++ {
		result = append(result, ranked[i].text)
	}
	return result
}

func main() {
	store := &VectorStore{}
	err := store.Index([]string{
		"支付异常排查：先检查第三方支付渠道回调是否正常，再看订单状态机是否卡住",
		"数据库死锁通常由多个事务加锁顺序不一致导致，统一加锁顺序可解决",
		"服务内存持续增长最终 OOM，多半是 goroutine 泄漏或未释放的缓存",
	})
	if err != nil {
		fmt.Printf("建库失败（请检查 OPENAI_API_KEY / OPENAI_BASE_URL / EMBEDDING_MODEL）：%v\n", err)
		return
	}

	query := "用户反馈付款失败，怎么排查？" // 注意：库里没有"付款失败"这个词
	hits := store.Search(query, 1)
	if len(hits) == 0 {
		fmt.Println("检索失败，未返回结果")
		return
	}
	fmt.Printf("问题：%s\n最相关知识：%s\n", query, hits[0])
}
