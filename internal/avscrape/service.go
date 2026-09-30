package avscrape

import (
	"fmt"
	"sort"
	"strings"

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

	// MetaTube
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

	// JavStash
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

	// 翻译
	if cfg.EnableTranslate && !best.HasChinese {
		tr := NewTranslator(cfg.TranslateEngine, cfg.TranslateTarget)
		tr.TranslateResult(best)
		best.HasChinese = true
	}

	media := MediaFromResult(best)
	if err := s.DB.Create(media).Error; err != nil {
		return nil, err
	}
	return best, nil
}

// mergeResults 合并多个源的结果
// 图片优先级：JavStash 海报 > DMM/FANZA > JAV321 > JavBus
func mergeResults(results []*ScrapeResult, cfg *Config) *ScrapeResult {
	sorted := sortByChinese(results, cfg.PreferChineseSource)

	best := *sorted[0]
	best.PosterCandidates = []string{}
	best.FanartCandidates = []string{}

	type candidate struct {
		url      string
		priority int
	}
	var posterList []candidate
	var fanartList []candidate

	for _, r := range sorted {
		p := sourceImagePriority(r.Source)
		if r.Poster != "" {
			posterList = append(posterList, candidate{r.Poster, p})
		}
		if r.Fanart != "" {
			fanartList = append(fanartList, candidate{r.Fanart, p})
		}
	}

	// 按优先级排序（数字越小越优先）
	sort.SliceStable(posterList, func(i, j int) bool {
		return posterList[i].priority < posterList[j].priority
	})
	sort.SliceStable(fanartList, func(i, j int) bool {
		return fanartList[i].priority < fanartList[j].priority
	})

	// 去重后填充候选列表
	seenPoster := map[string]bool{}
	for _, c := range posterList {
		if !seenPoster[c.url] {
			best.PosterCandidates = append(best.PosterCandidates, c.url)
			seenPoster[c.url] = true
		}
	}
	seenFanart := map[string]bool{}
	for _, c := range fanartList {
		if !seenFanart[c.url] {
			best.FanartCandidates = append(best.FanartCandidates, c.url)
			seenFanart[c.url] = true
		}
	}

	// Poster/Fanart 字段本身保留优先级最高的（用于 NFO 展示）
	if len(best.PosterCandidates) > 0 {
		best.Poster = best.PosterCandidates[0]
	}
	if len(best.FanartCandidates) > 0 {
		best.Fanart = best.FanartCandidates[0]
	}

	// 合并其他字段（按顺序取第一个非空的）
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
		if best.Runtime == 0 && r.Runtime > 0 {
			best.Runtime = r.Runtime
		}
		if best.ReleaseDate == "" && r.ReleaseDate != "" {
			best.ReleaseDate = r.ReleaseDate
		}
		if best.Trailer == "" && r.Trailer != "" {
			best.Trailer = r.Trailer
		}

		// 剧照合并去重
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

		// 演员合并去重
		for _, a := range r.Actors {
			found := false
			for i := range best.Actors {
				if best.Actors[i].Name == a.Name {
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
					found = true
					break
				}
			}
			if !found {
				best.Actors = append(best.Actors, a)
			}
		}

		// 标签合并去重
		for _, g := range r.Genres {
			exists := false
			for _, bg := range best.Genres {
				if bg == g {
					exists = true
					break
				}
			}
			if !exists {
				best.Genres = append(best.Genres, g)
			}
		}

		// URL 合并去重
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

// sourceImagePriority 图片来源优先级，数字越小越优先
// JavStash 的图无防盗链，最稳定，放第一位
func sourceImagePriority(source string) int {
	switch {
	case strings.HasPrefix(source, "javstash"):
		return 1 // JavStash 最优先
	case strings.Contains(source, "DMM"):
		return 2 // DMM 质量高，无防盗链
	case strings.Contains(source, "FANZA"):
		return 2
	case strings.Contains(source, "JAV321"):
		return 3
	case strings.Contains(source, "JavBus"):
		return 10 // JavBus 有防盗链，放最后
	default:
		return 5
	}
}

// sortByChinese 有中文优先，其次按完整度
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