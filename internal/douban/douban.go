package douban

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"Q115-STRM/internal/helpers"
)

const (
	minRequestInterval = 2 * time.Second
	cacheTTL           = 30 * time.Minute
	// 你验证过可用的官方 Key
	defaultApiKey = "0ab215a8b1977939201640fa14c66bab"
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

// ==================== API 响应结构 ====================

type doubanApiResponse struct {
	Rating struct {
		Average string `json:"average"` // 官方 API 返回的是字符串 "7.7"
	} `json:"rating"`
	Title string `json:"title"`
	Msg   string `json:"msg"`
	Code  int    `json:"code"`
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
		apiKey = defaultApiKey
	}
	return &Client{
		httpClient: &http.Client{Timeout: 15 * time.Second},
		apiKey:     apiKey,
	}
}

// GetRatingByImdb 通过 IMDb ID 获取豆瓣评分（推荐）
func (c *Client) GetRatingByImdb(imdbId string) (float64, error) {
	if strings.TrimSpace(imdbId) == "" {
		return 0, nil
	}

	cacheKey := "imdb_" + imdbId
	if rating, ok := getCache(cacheKey); ok {
		return rating, nil
	}

	globalRateLimit()

	apiURL := fmt.Sprintf("https://api.douban.com/v2/movie/imdb/%s", imdbId)
	formData := url.Values{}
	formData.Set("apikey", c.apiKey)

	req, err := http.NewRequest("POST", apiURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("豆瓣 API 返回非 200: %d", resp.StatusCode)
	}

	var result doubanApiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}

	if result.Code != 0 {
		helpers.AppLogger.Warnf("[豆瓣API] 请求失败: %s (code: %d)", result.Msg, result.Code)
		return 0, fmt.Errorf("douban api error: %s", result.Msg)
	}

	ratingStr := strings.TrimSpace(result.Rating.Average)
	if ratingStr == "" {
		return 0, nil
	}

	rating, err := strconv.ParseFloat(ratingStr, 64)
	if err != nil {
		return 0, nil
	}

	setCache(cacheKey, rating)
	return rating, nil
}