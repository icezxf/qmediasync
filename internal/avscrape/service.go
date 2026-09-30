package avscrape

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"Q115-STRM/internal/helpers"
	"Q115-STRM/internal/models"

	"gorm.io/gorm"
)

type Service struct {
	DB *gorm.DB
}

func NewService(db *gorm.DB) *Service {
	return &Service{DB: db}
}

func (s *Service) Scrape(code string) (*ScrapeResult, error) {
	cfg, err := LoadConfig(s.DB)
	if err != nil {
		return nil, err
	}
	var allResults []*ScrapeResult

	if cfg.EnableMetaTube && cfg.MetaTubeServer != "" {
		mt := NewMetaTubeClient(cfg.MetaTubeServer)
		hits, err := mt.Search(code)
		if err == nil {
			for _, h := range hits {
				providerID := extractProviderID(h.Source, h.Code)
				if providerID == "" {
					continue
				}
				detail, err := mt.Detail(code, providerID)
				if err == nil {
					allResults = append(allResults, detail)
				} else {
					allResults = append(allResults, h)
				}
			}
		}
	}

	if cfg.EnableJavStash {
		js := NewJavStashClient(cfg.JavStashEndpoint, cfg.JavStashAPIKey)
		hits, err := js.Search(code)
		if err == nil {
			allResults = append(allResults, hits...)
		}
	}

	if len(allResults) == 0 {
		return nil, fmt.Errorf("no result for %s", code)
	}

	best := mergeResults(allResults, cfg)

	// JavDB 评分
	if cfg.EnableJavDBRating {
		if rating, votes, err := GetJavDBRating(cfg.JavDBEndpoint, code); err == nil && rating > 0 {
			best.Rating = rating
			best.Votes = votes
			helpers.AppLogger.Infof("[AV刮削] JavDB 评分: %.2f (%d人)", rating, votes)
		} else if err != nil {
			helpers.AppLogger.Warnf("[AV刮削] JavDB 评分获取失败: %v", err)
		}
	}

	// 翻译：只翻标题、简介、标签，不翻演员名
	if cfg.EnableTranslate {
		tr := NewTranslator(cfg.TranslateEngine, cfg.TranslateTarget)
		tr.DeepLKey = cfg.TranslateDeepLKey
		tr.BingKey = cfg.TranslateBingKey
		tr.BingRegion = cfg.TranslateBingRegion
		helpers.AppLogger.Infof("[AV刮削] 开始翻译 %s (engine=%s)", code, cfg.TranslateEngine)
		tr.TranslateResult(best)
	}

	media := MediaFromResult(best)

	var existing models.AVMedia
	err = s.DB.Where("code = ?", media.Code).First(&existing).Error
	if err == nil {
		media.ID = existing.ID
		media.CreatedAt = existing.CreatedAt
		if err := s.DB.Save(media).Error; err != nil {
			return nil, err
		}
	} else {
		if err := s.DB.Create(media).Error; err != nil {
			return nil, err
		}
	}
	return best, nil
}

// ============================================================
// JavDB 评分
// ============================================================

var javdbHTTPClient = &http.Client{Timeout: 20 * time.Second}
var javdbLastReq time.Time
var javdbMu sync.Mutex

func GetJavDBRating(endpoint, code string) (float64, int, error) {
	if endpoint == "" {
		return 0, 0, fmt.Errorf("JavDB endpoint 未配置")
	}

	javdbMu.Lock()
	elapsed := time.Since(javdbLastReq)
	if elapsed < 15*time.Second {
		wait := 15*time.Second - elapsed
		helpers.AppLogger.Infof("[JavDB] 限速等待 %.1f 秒", wait.Seconds())
		time.Sleep(wait)
	}
	javdbLastReq = time.Now()
	javdbMu.Unlock()

	u := strings.TrimRight(endpoint, "/") + "/api/v1/movies/search?q=" + code
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")

	resp, err := javdbHTTPClient.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("JavDB HTTP %d", resp.StatusCode)
	}

	var result struct {
		Data []struct {
			Number       string `json:"number"`
			Rate         string `json:"rate"`
			CommentCount string `json:"comment_count"`
			Score        string `json:"score"`
			Votes        int    `json:"votes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return 0, 0, fmt.Errorf("JavDB 解析失败: %w", err)
	}
	if len(result.Data) == 0 {
		return 0, 0, fmt.Errorf("JavDB 无结果")
	}

	d := result.Data[0]
	var raw float64
	if d.Rate != "" {
		fmt.Sscanf(d.Rate, "%f", &raw)
	} else if d.Score != "" {
		fmt.Sscanf(d.Score, "%f", &raw)
	}
	var votes int
	if d.CommentCount != "" {
		fmt.Sscanf(d.CommentCount, "%d", &votes)
	} else {
		votes = d.Votes
	}

	if raw <= 0 {
		return 0, 0, fmt.Errorf("JavDB 无评分")
	}
	rating := raw
	if raw <= 5.0 {
		rating = raw * 2
	}
	return rating, votes, nil
}

// ============================================================
// 合并多源
// ============================================================

func mergeResults(results []*ScrapeResult, cfg *Config) *ScrapeResult {
	sorted := sortByChinese(results, cfg.PreferChineseSource)
	best := *sorted[0]
	best.ImageCandidates = []string{}

	type candidate struct {
		url      string
		priority int
	}
	var imageList []candidate
	for _, r := range sorted {
		p := sourceImagePriority(r.Source)
		if r.Poster != "" {
			imageList = append(imageList, candidate{r.Poster, p})
		}
		if r.Fanart != "" {
			imageList = append(imageList, candidate{r.Fanart, p})
		}
	}

	sort.SliceStable(imageList, func(i, j int) bool {
		return imageList[i].priority < imageList[j].priority
	})

	seen := map[string]bool{}
	for _, c := range imageList {
		if !seen[c.url] {
			best.ImageCandidates = append(best.ImageCandidates, c.url)
			seen[c.url] = true
		}
	}

	for _, r := range sorted[1:] {
		if best.Plot == "" && r.Plot != "" {
			best.Plot = r.Plot
		}
		if best.OriginalTitle == "" && r.OriginalTitle != "" {
			best.OriginalTitle = r.OriginalTitle
		}
		if best.Director == "" && r.Director != "" {
			best.Director = r.Director
		}
		if best.Studio == "" && r.Studio != "" {
			best.Studio = r.Studio
		}
		if best.Label == "" && r.Label != "" {
			best.Label = r.Label
		}
		if best.Series == "" && r.Series != "" {
			best.Series = r.Series
		}
		if best.Rating == 0 && r.Rating > 0 {
			best.Rating = r.Rating
		}
		if best.Votes == 0 && r.Votes > 0 {
			best.Votes = r.Votes
		}
		if best.Runtime == 0 && r.Runtime > 0 {
			best.Runtime = r.Runtime
		}
		if best.ReleaseDate == "" && r.ReleaseDate != "" {
			best.ReleaseDate = r.ReleaseDate
		}
		if best.Trailer == "" && r.Trailer != "" {
			best.Trailer = r.Trailer
		}

		for _, img := range r.PreviewImages {
			exists := false
			for _, bi := range best.PreviewImages {
				if bi == img {
					exists = true
					break
				}
			}
			if !exists {
				best.PreviewImages = append(best.PreviewImages, img)
			}
		}

		// 演员合并：按名字 + aliases 交叉匹配去重
		for _, a := range r.Actors {
			found := false
			for i := range best.Actors {
				if aliasMatch(best.Actors[i], a) {
					if best.Actors[i].Image == "" && a.Image != "" {
						best.Actors[i].Image = a.Image
					}
					if best.Actors[i].Birthday == "" && a.Birthday != "" {
						best.Actors[i].Birthday = a.Birthday
					}
					if best.Actors[i].Country == "" && a.Country != "" {
						best.Actors[i].Country = a.Country
					}
					if best.Actors[i].Height == 0 && a.Height > 0 {
						best.Actors[i].Height = a.Height
					}
					for _, al := range a.Aliases {
						exists := false
						for _, bal := range best.Actors[i].Aliases {
							if bal == al {
								exists = true
								break
							}
						}
						if !exists {
							best.Actors[i].Aliases = append(best.Actors[i].Aliases, al)
						}
					}
					found = true
					break
				}
			}
			if !found {
				best.Actors = append(best.Actors, a)
			}
		}

		// 标签合并去重（归一化后比较）
		for _, g := range r.Genres {
			exists := false
			for _, bg := range best.Genres {
				if normalizeTag(bg) == normalizeTag(g) {
					exists = true
					break
				}
			}
			if !exists {
				best.Genres = append(best.Genres, g)
			}
		}

		for _, u := range r.Urls {
			exists := false
			for _, bu := range best.Urls {
				if bu == u {
					exists = true
					break
				}
			}
			if !exists {
				best.Urls = append(best.Urls, u)
			}
		}
	}

	return &best
}

// aliasMatch 演员去重：名字 + aliases 交叉匹配 + 归一化
func aliasMatch(a, b Actor) bool {
	an := normalizeName(a.Name)
	bn := normalizeName(b.Name)

	if an == bn {
		return true
	}
	for _, alias := range a.Aliases {
		if normalizeName(alias) == bn {
			return true
		}
	}
	for _, alias := range b.Aliases {
		if normalizeName(alias) == an {
			return true
		}
	}
	if len(a.Aliases) > 0 && len(b.Aliases) > 0 {
		for _, al := range a.Aliases {
			for _, bl := range b.Aliases {
				if normalizeName(al) == normalizeName(bl) {
					return true
				}
			}
		}
	}
	return false
}

// normalizeName 归一化人名：去空格、全角转半角、小写
func normalizeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "　", "")
	s = strings.ReplaceAll(s, " ", "")
	return strings.ToLower(s)
}

// normalizeTag 归一化标签：统一分隔符、去空格、小写、特殊映射
func normalizeTag(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "・", "、")
	s = strings.ReplaceAll(s, "·", "、")
	s = strings.ReplaceAll(s, "　", "")
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ToLower(s)
	// "four k" → "4k"
	s = strings.ReplaceAll(s, "fourk", "4k")
	return s
}

func sourceImagePriority(source string) int {
	switch {
	case strings.HasPrefix(source, "javstash"):
		return 1
	case strings.Contains(source, "DMM"):
		return 2
	case strings.Contains(source, "FANZA"):
		return 2
	case strings.Contains(source, "JAV321"):
		return 3
	case strings.Contains(source, "JavBus"):
		return 10
	default:
		return 5
	}
}

func sortByChinese(results []*ScrapeResult, preferChinese bool) []*ScrapeResult {
	sorted := make([]*ScrapeResult, len(results))
	copy(sorted, results)
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			si := completeness(sorted[i])
			sj := completeness(sorted[j])
			if preferChinese {
				ci := boolToInt(sorted[i].HasChinese)
				cj := boolToInt(sorted[j].HasChinese)
				if cj > ci || (cj == ci && sj > si) {
					sorted[i], sorted[j] = sorted[j], sorted[i]
				}
			} else {
				if sj > si {
					sorted[i], sorted[j] = sorted[j], sorted[i]
				}
			}
		}
	}
	return sorted
}

func completeness(r *ScrapeResult) int {
	s := 0
	if r.Plot != "" {
		s += 10
	}
	if r.Poster != "" {
		s += 3
	}
	if r.Fanart != "" {
		s += 3
	}
	if len(r.PreviewImages) > 0 {
		s += 5
	}
	if r.Director != "" {
		s += 2
	}
	if r.Studio != "" {
		s += 2
	}
	if r.Label != "" {
		s += 1
	}
	if r.Series != "" {
		s += 1
	}
	if r.Rating > 0 {
		s += 2
	}
	if r.Trailer != "" {
		s += 2
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func extractProviderID(source, code string) string {
	if len(source) <= len("metatube:") {
		return ""
	}
	return source[len("metatube:"):] + "/" + code
}

func (s *Service) ListMedia(page, pageSize int) ([]models.AVMedia, int64, error) {
	var list []models.AVMedia
	var total int64
	s.DB.Model(&models.AVMedia{}).Count(&total)
	err := s.DB.Order("created_at desc").Offset((page - 1) * pageSize).Limit(pageSize).Find(&list).Error
	return list, total, err
}

func (s *Service) GetMedia(id uint) (*models.AVMedia, error) {
	var m models.AVMedia
	err := s.DB.First(&m, id).Error
	return &m, err
}
