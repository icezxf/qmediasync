package douban

import (
    "fmt"
    "net/http"
    "net/url"
    "regexp"
    "strconv"
    "strings"
    "time"

    "github.com/PuerkitoBio/goquery"
)

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
    sid, err := c.searchSid(title, year)
    if err != nil {
        return 0, err
    }
    if sid == "" {
        return 0, nil
    }
    time.Sleep(2 * time.Second) // 限速，避免触发豆瓣风控
    return c.fetchRating(sid)
}

// searchSid 搜索豆瓣，返回第一个匹配的 sid
func (c *Client) searchSid(title string, year int) (string, error) {
    searchURL := fmt.Sprintf("https://www.douban.com/search?cat=1002&q=%s",
        url.QueryEscape(title))

    req, _ := http.NewRequest("GET", searchURL, nil)
    c.setHeaders(req)

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return "", err
    }
    defer resp.Body.Close()

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
        return false
    })

    return sid, nil
}

// fetchRating 根据 sid 获取豆瓣评分
func (c *Client) fetchRating(sid string) (float64, error) {
    detailURL := fmt.Sprintf("https://movie.douban.com/subject/%s/", sid)

    req, _ := http.NewRequest("GET", detailURL, nil)
    c.setHeaders(req)

    resp, err := c.httpClient.Do(req)
    if err != nil {
        return 0, err
    }
    defer resp.Body.Close()

    doc, err := goquery.NewDocumentFromReader(resp.Body)
    if err != nil {
        return 0, err
    }

    ratingStr := strings.TrimSpace(doc.Find("div.rating_self strong.rating_num").Text())
    if ratingStr == "" {
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
    if c.cookie != "" {
        req.Header.Set("Cookie", c.cookie)
    }
}
