package avscrape

import "time"

type AVPath struct {
ID           uint      `gorm:"primaryKey" json:"id"`
Name         string    `gorm:"size:128" json:"name"`
SourceType   string    `gorm:"size:32" json:"source_type"`
AccountID    uint      `json:"account_id"`
SourcePath   string    `gorm:"type:text" json:"source_path"`
TargetPath   string    `gorm:"type:text" json:"target_path"`
Mode         string    `gorm:"size:32" json:"mode"`
MoveMethod   string    `gorm:"size:32" json:"move_method"`
NameTemplate string    `gorm:"size:255" json:"name_template"`
Enable       bool      `json:"enable"`
LastScanAt   time.Time `json:"last_scan_at"`
CreatedAt    time.Time `json:"created_at"`
UpdatedAt    time.Time `json:"updated_at"`
}
