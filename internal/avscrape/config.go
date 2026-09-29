package avscrape

import (
	"encoding/json"
	"errors"

	"gorm.io/gorm"
)

// Config 运行时配置
type Config struct {
	EnableMetaTube      bool   `json:"enable_metatube"`
	MetaTubeServer      string `json:"metatube_server"`
	EnableJavStash      bool   `json:"enable_javstash"`
	JavStashEndpoint    string `json:"javstash_endpoint"`
	JavStashAPIKey      string `json:"javstash_api_key"`
	EnableTranslate     bool   `json:"enable_translate"`
	TranslateEngine     string `json:"translate_engine"` // google_free / libretranslate
	TranslateTarget     string `json:"translate_target"` // zh
	PreferChineseSource bool   `json:"prefer_chinese_source"`
}

var defaultConfig = Config{
	EnableMetaTube:      true,
	MetaTubeServer:      "https://metatube-server.hf.space",
	EnableJavStash:      false,
	JavStashEndpoint:    "https://javstash.org/graphql",
	EnableTranslate:     false,
	TranslateEngine:     "google_free",
	TranslateTarget:     "zh",
	PreferChineseSource: true,
}

// LoadConfig 从数据库读取配置，没有则返回默认
func LoadConfig(db *gorm.DB) (*Config, error) {
	cfg := defaultConfig
	var row AVSettings
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

// SaveConfig 保存配置
func SaveConfig(db *gorm.DB, cfg *Config) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	var row AVSettings
	err = db.Where("key = ?", "config").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return db.Create(&AVSettings{Key: "config", Value: string(data)}).Error
	}
	if err != nil {
		return err
	}
	row.Value = string(data)
	return db.Save(&row).Error
}