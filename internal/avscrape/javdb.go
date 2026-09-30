package avscrape

import (
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "sync"
    "time"
)

type JavDBClient struct {
    Endpoint string
    HTTP     *http.Client
    lastReq  time.Time
    mu       sync.Mutex
}

func NewJavDBClient() *JavDBClient {
    return &JavDBClient{
        Endpoint: "https://jdforrepam.com",
        HTTP:     &http.Client{Timeout: 20 * time.Second},
    }
}

// GetRating 获取番号的 JavDB 评分（5分制）
func (c *JavDBClient) GetRating(code string) (float64, int, error) {
    // 限速：确保两次请求间隔至少 15 秒
    c.mu.Lock()
    elapsed := time.Since(c.lastReq)
    if elapsed < 15*time.Second {
        time.Sleep(15*time.Second - elapsed)
    }
    c.lastReq = time.Now()
    c.mu.Unlock()

    // 调用 JavDB API 搜索
    url := fmt.Sprintf("%s/api/v1/javdb/movies/search?q=%s", c.Endpoint, code)
    resp, err := c.HTTP.Get(url)
    if err != nil {
        return 0, 0, err
    }
    defer resp.Body.Close()
    body, _ := io.ReadAll(resp.Body)

    var result struct {
        Data []struct {
            Rate         string `json:"rate"`
            CommentCount string `json:"comment_count"`
        } `json:"data"`
    }
    if err := json.Unmarshal(body, &result); err != nil {
        return 0, 0, err
    }
    if len(result.Data) == 0 {
        return 0, 0, fmt.Errorf("no result")
    }

    var rating float64
    fmt.Sscanf(result.Data[0].Rate, "%f", &rating)
    var votes int
    fmt.Sscanf(result.Data[0].CommentCount, "%d", &votes)
    return rating, votes, nil
}
