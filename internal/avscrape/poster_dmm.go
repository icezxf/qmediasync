package avscrape

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// dmmPosterURL 根据番号构造 DMM 封面 URL
// 规则：字母部分小写 + 数字部分补零到 5 位
// 例：MIDV-192 → midv00192
//     SSNI-658 → ssni00658
func dmmPosterURL(code string) string {
	var sb strings.Builder
	for _, r := range code {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		}
	}
	cleaned := strings.ToUpper(sb.String())
	i := 0
	for i < len(cleaned) && cleaned[i] >= 'A' && cleaned[i] <= 'Z' {
		i++
	}
	letters := cleaned[:i]
	digits := cleaned[i:]
	if letters == "" || digits == "" || len(digits) > 5 {
		return ""
	}
	var num int
	if _, err := fmt.Sscanf(digits, "%d", &num); err != nil {
		return ""
	}
	filename := strings.ToLower(letters) + fmt.Sprintf("%05d", num)
	return "https://pics.dmm.co.jp/digital/video/" + filename + "/" + filename + "pl.jpg"
}

// downloadDMMImage 下载 DMM 封面（带 Referer 防盗链）
func downloadDMMImage(url string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Referer", "https://www.dmm.co.jp/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil || len(data) < 1000 {
		return nil, fmt.Errorf("数据无效（%d 字节）", len(data))
	}
	return data, nil
}
