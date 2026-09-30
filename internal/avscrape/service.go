package avscrape

import (
	"fmt"
	"sort"

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

	// 1. MetaTube 拉所有启用源
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

	// 2. JavStash
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

	// 3. 按优先级排序所有结果
	sorted := sortByPriority(allResults)

	// 4. 逐字段按优先级取第一个非空值（合并成一个结果）
	merged := mergeByPriority(sorted)

	// 5. 最终结果如果是日文 → 翻译
	if !merged.HasChinese && cfg.EnableTranslate {
		tr := NewTranslator(cfg.TranslateEngine, cfg.TranslateTarget)
		tr.TranslateResult(merged)
		merged.HasChinese = true
	}

	// 6. 入库
	media := MediaFromResult(merged)
	if err := s.DB.Create(media).Error; err != nil {
		return nil, err
	}
	return merged, nil
}

// sortByPriority 按优先级排序：
//  1. 有中文 + JavStash
//  2. 有中文 + MetaTube（字段多的在前）
//  3. 无中文 + JavStash
//  4. 无中文 + MetaTube（字段多的在前）
func sortByPriority(results []*ScrapeResult) []*ScrapeResult {
	sorted := make([]*ScrapeResult, len(results))
	copy(sorted, results)

	sort.SliceStable(sorted, func(i, j int) bool {
		return priorityLevel(sorted[i]) < priorityLevel(sorted[j])
	})
	return sorted
}

// priorityLevel 数字越小优先级越高
func priorityLevel(r *ScrapeResult) int {
	isJavStash := r.Source == "javstash"
	switch {
	case r.HasChinese && isJavStash:
		return 1
	case r.HasChinese && !isJavStash:
		return 2
	case !r.HasChinese && isJavStash:
		return 3
	default:
		return 4
	}
}

// mergeByPriority 按已排序的顺序，逐个字段取第一个非空值
func mergeByPriority(sorted []*ScrapeResult) *ScrapeResult {
	if len(sorted) == 0 {
		return nil
	}
	// 以第一个为基准
	merged := *sorted[0]
	// 深拷贝切片字段，避免改到原对象
	merged.Genres = append([]string{}, merged.Genres...)
	merged.Actors = append([]Actor{}, merged.Actors...)
	merged.PreviewImages = append([]string{}, merged.PreviewImages...)
	merged.Urls = append([]string{}, merged.Urls...)

	// 记录哪些字段已经填过
	hasPlot := merged.Plot != ""
	hasFanart := merged.Fanart != ""
	hasTrailer := merged.Trailer != ""
	hasDirector := merged.Director != ""
	hasStudio := merged.Studio != ""
	hasLabel := merged.Label != ""
	hasSeries := merged.Series != ""
	hasRating := merged.Rating > 0
	hasChinese := merged.HasChinese

	// 演员和标签用 map 去重，按优先级从高到低累加
	actorSeen := make(map[string]int) // name -> index in merged.Actors
	for i, a := range merged.Actors {
		actorSeen[a.Name] = i
	}
	genreSeen := make(map[string]bool)
	for _, g := range merged.Genres {
		genreSeen[g] = true
	}

	for _, r := range sorted[1:] {
		if !hasPlot && r.Plot != "" {
			merged.Plot = r.Plot
			hasPlot = true
		}
		if !hasFanart && r.Fanart != "" {
			merged.Fanart = r.Fanart
			hasFanart = true
		}
		if !hasTrailer && r.Trailer != "" {
			merged.Trailer = r.Trailer
			hasTrailer = true
		}
		if !hasDirector && r.Director != "" {
			merged.Director = r.Director
			hasDirector = true
		}
		if !hasStudio && r.Studio != "" {
			merged.Studio = r.Studio
			hasStudio = true
		}
		if !hasLabel && r.Label != "" {
			merged.Label = r.Label
			hasLabel = true
		}
		if !hasSeries && r.Series != "" {
			merged.Series = r.Series
			hasSeries = true
		}
		if !hasRating && r.Rating > 0 {
			merged.Rating = r.Rating
			hasRating = true
		}
		if !hasChinese && r.HasChinese {
			hasChinese = true
		}
		if len(merged.PreviewImages) == 0 && len(r.PreviewImages) > 0 {
			merged.PreviewImages = append([]string{}, r.PreviewImages...)
		}

		// 演员：高优先级的演员信息覆盖低优先级同名演员的空字段
		for _, a := range r.Actors {
			if idx, ok := actorSeen[a.Name]; ok {
				// 同名演员，用已有的（高优先级）保留，只补空缺
				existing := &merged.Actors[idx]
				if existing.Role == "" && a.Role != "" {
					existing.Role = a.Role
				}
				if existing.Thumb == "" && a.Thumb != "" {
					existing.Thumb = a.Thumb
				}
				if existing.Image == "" && a.Image != "" {
					existing.Image = a.Image
				}
				if existing.Birthday == "" && a.Birthday != "" {
					existing.Birthday = a.Birthday
				}
				if existing.Country == "" && a.Country != "" {
					existing.Country = a.Country
				}
				if existing.Height == 0 && a.Height != 0 {
					existing.Height = a.Height
				}
			} else {
				// 新演员，追加
				actorSeen[a.Name] = len(merged.Actors)
				merged.Actors = append(merged.Actors, a)
			}
		}

		// 标签合并去重
		for _, g := range r.Genres {
			if !genreSeen[g] {
				merged.Genres = append(merged.Genres, g)
				genreSeen[g] = true
			}
		}

		// URL 合并
		for _, u := range r.Urls {
			found := false
			for _, mu := range merged.Urls {
				if mu == u {
					found = true
					break
				}
			}
			if !found {
				merged.Urls = append(merged.Urls, u)
			}
		}
	}

	merged.HasChinese = hasChinese
	return &merged
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