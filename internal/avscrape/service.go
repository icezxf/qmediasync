package avscrape

import (
	"fmt"

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
	best := pickBest(allResults, cfg)
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

func pickBest(results []*ScrapeResult, cfg *Config) *ScrapeResult {
	if cfg.PreferChineseSource {
		for _, r := range results {
			if r.HasChinese {
				return r
			}
		}
	}
	for _, r := range results {
		if r.Source == "javstash" {
			return r
		}
	}
	var best *ScrapeResult
	bestScore := -1
	for _, r := range results {
		score := 0
		if r.Plot != "" {
			score++
		}
		if r.Director != "" {
			score++
		}
		if r.Studio != "" {
			score++
		}
		if len(r.PreviewImages) > 0 {
			score += 2
		}
		if r.Trailer != "" {
			score++
		}
		if score > bestScore {
			bestScore = score
			best = r
		}
	}
	return best
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