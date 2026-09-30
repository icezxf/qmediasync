package avscrape

import (
	"encoding/json"
	"errors"

	"Q115-STRM/internal/models"

	"gorm.io/gorm"
)

type Config struct {
	EnableMetaTube      bool   `json:"enable_metatube"`
	MetaTubeServer      string `json:"metatube_server"`
	EnableJavStash      bool   `json:"enable_javstash"`
	JavStashEndpoint    string `json:"javstash_endpoint"`
	JavStashAPIKey      string `json:"javstash_api_key"`
	EnableTranslate     bool   `json:"enable_translate"`
	TranslateEngine     string `json:"translate_engine"`
	TranslateTarget     string `json:"translate_target"`
	TranslateDeepLKey   string `json:"translate_deepl_key"`
	TranslateBingKey    string `json:"translate_bing_key"`
	TranslateBingRegion string `json:"translate_bing_region"`
	EnableJavDBRating   bool   `json:"enable_javdb_rating"` // 是否用 JavDB 评分
	JavDBEndpoint       string `json:"javdb_endpoint"`      // JavDB 非官方 API 地址
	PreferChineseSource bool   `json:"prefer_chinese_source"`
}

var defaultConfig = Config{
	EnableMetaTube:      true,
	MetaTubeServer:      "https://metatube-server.hf.space",
	EnableJavStash:      false,
	JavStashEndpoint:    "https://javstash.org/graphql",
	EnableTranslate:     false,
	TranslateEngine:     "deepl",
	TranslateTarget:     "zh",
	EnableJavDBRating:   false,
	PreferChineseSource: true,
}

func LoadConfig(db *gorm.DB) (*Config, error) {
	cfg := defaultConfig
	var row models.AVSettings
	err := db.Where("key = ?", "config").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return &cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(row.Value), &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func SaveConfig(db *gorm.DB, cfg *Config) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	var row models.AVSettings
	err = db.Where("key = ?", "config").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return db.Create(&models.AVSettings{Key: "config", Value: string(data)}).Error
	}
	if err != nil {
		return err
	}
	row.Value = string(data)
	return db.Save(&row).Error
}
