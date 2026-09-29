package avscrape

import (
	"fmt"

	"gorm.io/gorm"
)

type Service struct {
	DB *gorm.DB
}

func NewService(db *gorm.DB) *Service {
	return &Service{DB: db}
}

// Scrape 执行一次完整刮削
func (s *Service) Scrape(code string) (*ScrapeResult, error) {
	cfg, err := LoadConfig(s.DB)
	if err != nil {
		return nil, err
	}

	var allResults []*ScrapeResult

	// 1. MetaTube
	if cfg.EnableMetaTube && cfg.MetaTubeServer != "" {
		mt := NewMetaTubeClient(cfg.MetaTubeServer)
		hits, err := mt.Search(code)
		if err == nil {
			for _, h := range hits {
				// 用 provider/id 拉详情
				providerID := extractProviderID(h.Source, h.Code)
				if providerID == "" {
					continue
				}
				detail, err := mt.Detail(code, providerID)
				if err == nil {
					allResults = append(allResults, detail)
				} else {
					// 详情失败就用搜索结果
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

	// 3. 选源
	best := pickBest(allResults, cfg)

	// 4. 翻译
	if cfg.EnableTranslate && !best.HasChinese {
		tr := NewTranslator(cfg.TranslateEngine, cfg.TranslateTarget)
		tr.TranslateResult(best)
		best.HasChinese = true
	}

	// 5. 入库
	media := MediaFromResult(best)
	if err := s.DB.Create(media).Error; err != nil {
		return nil, err
	}
	return best, nil
}

// pickBest 按优先级选源：有中文 > JavStash > MetaTube
func pickBest(results []*ScrapeResult, cfg *Config) *ScrapeResult {
	// 1. 优先有中文的
	if cfg.PreferChineseSource {
		for _, r := range results {
			if r.HasChinese {
				return r
			}
		}
	}
	// 2. JavStash
	for _, r := range results {
		if r.Source == "javstash" {
			return r
		}
	}
	// 3. 默认第一条
	return results[0]
}

// extractProviderID 从 "metatube:JavBus" 和 code 拼出 "JavBus/SNOS-377"
func extractProviderID(source, code string) string {
	if len(source) <= len("metatube:") {
		return ""
	}
	provider := source[len("metatube:"):]
	return provider + "/" + code
}

// ListMedia 媒体库列表
func (s *Service) ListMedia(page, pageSize int) ([]AVMedia, int64, error) {
	var list []AVMedia
	var total int64
	s.DB.Model(&AVMedia{}).Count(&total)
	err := s.DB.Order("created_at desc").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&list).Error
	return list, total, err
}

// GetMedia 单条详情
func (s *Service) GetMedia(id uint) (*AVMedia, error) {
	var m AVMedia
	err := s.DB.First(&m, id).Error
	return &m, err
}