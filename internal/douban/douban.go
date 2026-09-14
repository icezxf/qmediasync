package douban

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"Q115-STRM/internal/helpers"
)

const (
	// 小程序 API 响应较快，间隔可以设为 2 秒
	minRequestInterval = 2 * time.Second
	cacheTTL = 30 * time.Minute
	// 图片里提供的 Key，建议用环境变量覆盖
	defaultApiKey = "0ac44ae016490db2204ce0a042db2916"
	// 备用 Key
	backupApiKey = "054022eaeae0b00e0fc068c0c0a2102a"
)

// ==================== 全局限速与缓存 ====================

var (
	rateMu          sync.Mutex
	lastRequestTime time.Time
)

func globalRateLimit() {
	rateMu.Lock()
	defer rateMu.Unlock()
	elapsed := time.Since(lastRequestTime)
	if elapsed < minRequestInterval {
		time.Sleep(minRequestInterval - elapsed)
	}
	lastRequestTime = time.Now()
}

type cacheEntry struct {
	rating float64
	expire time.Time
}

var (
	cacheMu     sync.Mutex
	ratingCache = map[string]cacheEntry{}
)

func getCache(key string) (float64, bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	entry, ok := ratingCache[key]
	if !ok || time.Now().After(entry.expire) {
		delete(ratingCache, key)
		return 0, false
	}
	return entry.rating, true
}

func setCache(key string, rating float64) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	ratingCache[key] = cacheEntry{rating: rating, expire: time.Now().Add(cacheTTL)}
}

// ==================== API 结构体 ====================

type doubanSearchResponse struct {
	Subjects []struct {
		Title  string `json:"title"`
		Year   string `json:"year"`
		Rating struct {
			Value float64 `json:"value"`
		} `json:"rating"`
	} `json:"subjects"`
	Msg  string `json:"msg"`
	Code int    `json:"code"`
}

// ==================== Client ====================

type Client struct {
	httpClient *http.Client
	apiKey     string
}

func NewClient(apiKey string) *Client {
	if apiKey == "" {
		apiKey = os.Getenv("DOUBAN_API_KEY")
	}
	if apiKey == "" {
		apiKey = defaultApiKey // 兜底使用图片里的 Key
	}
	return &Client{
		httpClient: &http.Client{Timeout: 15 * time.Second},
		apiKey:     apiKey,
	}
}

// GetRatingByTitle 通过标题+年份搜索获取评分
func (c *Client) GetRatingByTitle(title string, year int) (float64, error) {
	if strings.TrimSpace(title) == "" {
		return 0, nil
	}

	cacheKey := fmt.Sprintf("%s_%d", title, year)
	if rating, ok := getCache(cacheKey); ok {
		return rating, nil
	}

	globalRateLimit()

	// 构造搜索 URL
	searchURL := fmt.Sprintf("https://frodo.douban.com/api/v2/movie/search?q=%s&apiKey=%s",
		url.QueryEscape(title), c.apiKey)

	req, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		return 0, err
	}

	// 关键：必须带上微信小程序的 Headers，否则直接 403
	req.Header.Set("User-Agent", "MicroMessenger/")
	req.Header.Set("Referer", "https://servicewechat.com/wx2f9b06c1de1ccfca/91/page-frame.html")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 如果返回 403 或 112，尝试备用 Key
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == 400 {
			helpers.AppLogger.Warnf("[豆瓣API] 主 Key 可能失效，尝试备用 Key")
			// 可以在这里实现备用 Key 的递归请求，为简单先返回错误
		}
		return 0, fmt.Errorf("豆瓣 API 返回非 200: %d", resp.StatusCode)
	}

	var result doubanSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}

	if result.Code != 0 {
		helpers.AppLogger.Warnf("[豆瓣API] 请求失败: %s (code: %d)", result.Msg, result.Code)
		return 0, fmt.Errorf("douban api error: %s", result.Msg)
	}

	// 遍历搜索结果，匹配年份，提取评分
	var rating float64
	for _, sub := range result.Subjects {
		// 如果年份匹配，或者第一项就是最匹配的，取评分
		if year > 0 && sub.Year != "" && !strings.Contains(sub.Year, fmt.Sprintf("%d", year)) {
			continue
		}
		if sub.Rating.Value > 0 {
			rating = sub.Rating.Value
			break
		}
	}

	// 如果年份没匹配上，退而求其次取第一条有评分的
	if rating == 0 && len(result.Subjects) > 0 {
		rating = result.Subjects[0].Rating.Value
	}

	setCache(cacheKey, rating)
	return rating, nil
}