package douban

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"Q115-STRM/internal/helpers"

	"github.com/PuerkitoBio/goquery"
)

// ==================== 配置 ====================

const (
	// 未登录建议 5 秒，登录后可降至 3 秒
	minRequestInterval = 5 * time.Second
	// 缓存有效期
	cacheTTL = 30 * time.Minute
)

// ==================== 全局限速器 ====================

var (
	rateMu          sync.Mutex
	lastRequestTime time.Time
)

// globalRateLimit 全局限速，所有协程共用
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
	cacheMu sync.Mutex
	ratingCache = map[string]cacheEntry{}
)

func getCache(key string) (float64, bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()

	entry, ok := ratingCache[key]
	if !ok {
		return 0, false
	}
	if time.Now().After(entry.expire) {
		delete(ratingCache, key)
		return 0, false
	}
	return entry.rating, true
}

func setCache(key string, rating float64) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	ratingCache[key] = cacheEntry{
		rating: rating,
		expire: time.Now().Add(cacheTTL),
	}
}

// ==================== Client ====================

// Client 豆瓣客户端
type Client struct {
	httpClient *http.Client
	cookie     string // 可选：豆瓣 Cookie，降低风控概率
}

// NewClient 创建豆瓣客户端
func NewClient(cookie string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 15 * time.Second},
		cookie:     cookie,
	}
}

// GetRating 根据标题和年份获取豆瓣评分
func (c *Client) GetRating(title string, year int) (float64, error) {
	if strings.TrimSpace(title) == "" {
		return 0, nil
	}

	// 1. 查缓存
	cacheKey := fmt.Sprintf("%s_%d", title, year)
	if rating, ok := getCache(cacheKey); ok {
		helpers.AppLogger.Infof("[豆瓣] 命中缓存: %s -> %.1f", title, rating)
		return rating, nil
	}

	// 2. 搜索豆瓣，获取 sid
	sid, err := c.searchSid(title, year)
	if err != nil {
		return 0, err
	}
	if sid == "" {
		// 未找到也缓存，避免反复搜索
		setCache(cacheKey, 0)
		return 0, nil
	}

	// 3. 请求详情页，获取评分
	rating, err := c.fetchRating(sid)
	if err != nil {
		return 0, err
	}

	// 4. 写缓存
	setCache(cacheKey, rating)
	return rating, nil
}

// searchSid 搜索豆瓣，返回第一个匹配的 sid
func (c *Client) searchSid(title string, year int) (string, error) {
	// 全局限速
	globalRateLimit()

	searchURL := fmt.Sprintf("https://www.douban.com/search?cat=1002&q=%s",
		url.QueryEscape(title))

	req, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		return "", err
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// 风控检测
	if err := checkRiskControl(resp, "搜索"); err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("豆瓣搜索返回非200: %d", resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return "", err
	}

	var sid string
	doc.Find("div.result-list .result").EachWithBreak(func(i int, s *goquery.Selection) bool {
		onclick, exists := s.Find("div.title a").Attr("onclick")
		if !exists {
			return true
		}
		re := regexp.MustCompile(`sid:\s*(\d+)`)
		match := re.FindStringSubmatch(onclick)
		if len(match) < 2 {
			return true
		}
		currentSid := match[1]

		// 年份过滤
		if year > 0 {
			yearText := s.Find("div.rating-info span:last-child").Text()
			if !strings.Contains(yearText, strconv.Itoa(year)) {
				return true
			}
		}

		sid = currentSid
		return false // 找到匹配，停止遍历
	})

	return sid, nil
}

// fetchRating 根据 sid 获取豆瓣评分
func (c *Client) fetchRating(sid string) (float64, error) {
	// 全局限速
	globalRateLimit()

	detailURL := fmt.Sprintf("https://movie.douban.com/subject/%s/", sid)

	req, err := http.NewRequest("GET", detailURL, nil)
	if err != nil {
		return 0, err
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	// 风控检测
	if err := checkRiskControl(resp, "详情"); err != nil {
		return 0, err
	}

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("豆瓣详情返回非200: %d", resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return 0, err
	}

	ratingStr := strings.TrimSpace(doc.Find("div.rating_self strong.rating_num").Text())
	if ratingStr == "" {
		helpers.AppLogger.Warnf("[豆瓣] 未解析到评分, sid=%s", sid)
		return 0, nil
	}

	rating, err := strconv.ParseFloat(ratingStr, 64)
	if err != nil {
		return 0, nil
	}

	return rating, nil
}

// setHeaders 设置请求头
func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("User-Agent",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
	req.Header.Set("Referer", "https://movie.douban.com/")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	if c.cookie != "" {
		req.Header.Set("Cookie", c.cookie)
	}
}

// checkRiskControl 检测是否触发豆瓣风控
func checkRiskControl(resp *http.Response, stage string) error {
	// 重定向到 sec.douban.com 或 accounts.douban.com 一般是被风控/登录失效
	if resp.Request != nil && resp.Request.URL != nil {
		host := resp.Request.URL.Host
		if strings.Contains(host, "sec.douban.com") {
			helpers.AppLogger.Errorf("[豆瓣] %s 触发风控，被重定向到 %s", stage, host)
			return errors.New("douban risk control triggered")
		}
		if strings.Contains(host, "accounts.douban.com") {
			helpers.AppLogger.Errorf("[豆瓣] %s 被重定向到登录页，可能 Cookie 已失效", stage)
			return errors.New("douban login required")
		}
	}

	// 检查状态码
	if resp.StatusCode == http.StatusForbidden {
		helpers.AppLogger.Errorf("[豆瓣] %s 返回 403，可能触发风控", stage)
		return errors.New("douban returned 403")
	}

	return nil
}