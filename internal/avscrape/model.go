package avscrape

import (
	"time"

	"gorm.io/gorm"
)

// AVSettings 键值对配置表
type AVSettings struct {
	Key   string `gorm:"primaryKey;size:64" json:"key"`
	Value string `gorm:"type:text" json:"value"`
}

// AVTask 刮削任务
type AVTask struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Code      string    `gorm:"index;size:64" json:"code"`        // 番号
	FilePath  string    `gorm:"type:text" json:"file_path"`       // 原文件路径
	Status    string    `gorm:"size:32" json:"status"`            // pending/running/done/failed
	Provider  string    `gorm:"size:32" json:"provider"`          // 最终选用的源
	Message   string    `gorm:"type:text" json:"message"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AVMedia 刮削结果（用于媒体库展示）
type AVMedia struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	Code          string    `gorm:"index;size:64" json:"code"`
	Title         string    `gorm:"type:text" json:"title"`
	OriginalTitle string    `gorm:"type:text" json:"original_title"`
	Plot          string    `gorm:"type:text" json:"plot"`
	Runtime       int       `json:"runtime"`        // 分钟
	ReleaseDate   string    `gorm:"size:32" json:"release_date"`
	Director      string    `gorm:"size:255" json:"director"`
	Studio        string    `gorm:"size:255" json:"studio"`
	Label         string    `gorm:"size:255" json:"label"`
	Series        string    `gorm:"size:255" json:"series"`
	Genres        string    `gorm:"type:text" json:"genres"`        // JSON 数组
	Actors        string    `gorm:"type:text" json:"actors"`        // JSON 数组
	Poster        string    `gorm:"type:text" json:"poster"`
	Fanart        string    `gorm:"type:text" json:"fanart"`
	PreviewImages string    `gorm:"type:text" json:"preview_images"` // JSON 数组
	Trailer       string    `gorm:"type:text" json:"trailer"`
	Rating        float64   `json:"rating"`
	Urls          string    `gorm:"type:text" json:"urls"`          // JSON 数组
	NFOContent    string    `gorm:"type:text" json:"nfo_content"`   // 完整 NFO 文本
	Translated    bool      `json:"translated"`                     // 是否已翻译
	Source        string    `gorm:"size:32" json:"source"`          // 实际来源
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// AutoMigrate 自动迁移
func AutoMigrate(db *gorm.DB) error {
	return db.AutoMigrate(&AVSettings{}, &AVTask{}, &AVMedia{})
}