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
	// 全局请求间隔，建议 2 秒
	minRequestInterval = 2 * time.Second
	// 缓存有效期
	cacheTTL = 30 * time.Minute
	// 官方 App API Key（用于电影查询）
	defaultApiKey = "0ab215a8b1977939201640fa14c66bab"
	// 第三方聚合 API（用于电视剧查询）
	tvApiHost = "https://douban-idatabase.kfstorm.com"
)

// ==================== 全局限速 ====================

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

// ==================== 缓存 ====================

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

// 官方电影 API 响应
type doubanMovieApiResponse struct {
	Rating struct {
		Average string `json:"average"`
	} `json:"rating"`
	Title string `json:"title"`
	Msg   string `json:"msg"`
	Code  int    `json:"code"`
}

// 第三方聚合 API 响应
type doubanTvApiItem struct {
	DoubanID    string  `json:"douban_id"`
	ImdbID      string  `json:"imdb_id"`
	DoubanTitle string  `json:"douban_title"`
	Year        int     `json:"year"`
	Rating      float64 `json:"rating"`
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

// ==================== 电影查询（官方 API） ====================

// GetRatingByImdb 通过 IMDb ID 获取电影豆瓣评分
func (c *Client) GetRatingByImdb(imdbId string) (float64, error) {
	if strings.TrimSpace(imdbId) == "" {
		return 0, nil
	}

	cacheKey := "movie_imdb_" + imdbId
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
		return 0, fmt.Errorf("豆瓣电影 API 返回非 200: %d", resp.StatusCode)
	}

	var result doubanMovieApiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}

	if result.Code != 0 {
		helpers.AppLogger.Warnf("[豆瓣API] 电影请求失败: %s (code: %d)", result.Msg, result.Code)
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

// ==================== 电视剧查询（第三方聚合 API） ====================

// GetTVRatingByImdb 通过 IMDb ID 获取电视剧豆瓣评分
func (c *Client) GetTVRatingByImdb(imdbId string) (float64, error) {
	if strings.TrimSpace(imdbId) == "" {
		return 0, nil
	}

	cacheKey := "tv_imdb_" + imdbId
	if rating, ok := getCache(cacheKey); ok {
		return rating, nil
	}

	globalRateLimit()

	// 第三方 API，无需 apikey
	apiURL := fmt.Sprintf("%s/api/item?imdb_id=%s", tvApiHost, imdbId)

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("第三方豆瓣 API 返回非 200: %d", resp.StatusCode)
	}

	// 第三方 API 返回的是一个数组
	var results []doubanTvApiItem
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return 0, err
	}

	if len(results) == 0 {
		return 0, nil // 未找到
	}

	rating := results[0].Rating
	if rating <= 0 {
		return 0, nil
	}

	setCache(cacheKey, rating)
	return rating, nil
}