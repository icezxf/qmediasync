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

	if cfg.EnableTranslate && !best.HasChinese {
		tr := NewTranslator(cfg.TranslateEngine, cfg.TranslateTarget)
		tr.TranslateResult(best)
		best.HasChinese = true
	}

	media := MediaFromResult(best)

	// Upsert
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

// mergeResults 合并多个源的结果
// 图片优先级：JavStash 海报 > DMM/FANZA > JAV321 > JavBus
// mergeResults 合并多个源的结果
func mergeResults(results []*ScrapeResult, cfg *Config) *ScrapeResult {
	sorted := sortByChinese(results, cfg.PreferChineseSource)

	best := *sorted[0]
	best.PosterCandidates = []string{}
	best.FanartCandidates = []string{}

	// 图片候选池
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

	sort.SliceStable(posterList, func(i, j int) bool { return posterList[i].priority < posterList[j].priority })
	sort.SliceStable(fanartList, func(i, j int) bool { return fanartList[i].priority < fanartList[j].priority })

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
	if len(best.PosterCandidates) > 0 {
		best.Poster = best.PosterCandidates[0]
	}
	if len(best.FanartCandidates) > 0 {
		best.Fanart = best.FanartCandidates[0]
	}

	// ===== 关键修复：演员只取最可信的一个源，不合并 =====
	bestActors := []Actor{}
	bestActorPriority := 999
	for _, r := range sorted {
		if len(r.Actors) == 0 {
			continue
		}
		p := actorSourcePriority(r.Source)
		if p < bestActorPriority {
			bestActorPriority = p
			bestActors = r.Actors
		}
	}
	best.Actors = bestActors

	// ===== 合并其他字段（按优先级取第一个非空）=====
	for _, r := range sorted[1:] {
		if best.Plot == "" && r.Plot != "" { best.Plot = r.Plot }
		if best.OriginalTitle == "" && r.OriginalTitle != "" { best.OriginalTitle = r.OriginalTitle }
		if best.Director == "" && r.Director != "" { best.Director = r.Director }
		if best.Studio == "" && r.Studio != "" { best.Studio = r.Studio }
		if best.Label == "" && r.Label != "" { best.Label = r.Label }
		if best.Series == "" && r.Series != "" { best.Series = r.Series }
		if best.Rating == 0 && r.Rating > 0 { best.Rating = r.Rating }
		if best.Runtime == 0 && r.Runtime > 0 { best.Runtime = r.Runtime }
		if best.ReleaseDate == "" && r.ReleaseDate != "" { best.ReleaseDate = r.ReleaseDate }
		if best.Trailer == "" && r.Trailer != "" { best.Trailer = r.Trailer }

		// 剧照合并去重
		for _, img := range r.PreviewImages {
			exists := false
			for _, bi := range best.PreviewImages {
				if bi == img { exists = true; break }
			}
			if !exists {
				best.PreviewImages = append(best.PreviewImages, img)
			}
		}

		// 标签合并去重
		for _, g := range r.Genres {
			exists := false
			for _, bg := range best.Genres {
				if bg == g { exists = true; break }
			}
			if !exists {
				best.Genres = append(best.Genres, g)
			}
		}

		// URL 合并去重
		for _, u := range r.Urls {
			exists := false
			for _, bu := range best.Urls {
				if bu == u { exists = true; break }
			}
			if !exists {
				best.Urls = append(best.Urls, u)
			}
		}
	}

	return &best
}

// actorSourcePriority 演员数据来源优先级：数字越小越可信
// JavStash 是社区维护，演员最准，排第一
func actorSourcePriority(source string) int {
	switch {
	case strings.HasPrefix(source, "javstash"):
		return 1
	case strings.Contains(source, "FANZA"):
		return 2
	case strings.Contains(source, "DMM"):
		return 2
	case strings.Contains(source, "JAV321"):
		return 3
	case strings.Contains(source, "JavBus"):
		return 10
	default:
		return 5
	}
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