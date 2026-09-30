package avscrape

import (
	"fmt"
	"time"

	"Q115-STRM/internal/models"
)

type FileEntry struct {
	Name     string
	Path     string
	IsDir    bool
	Size     int64
	ID       string
	PickCode string
	MTime    time.Time
}

type FileSystem interface {
	List(path string) ([]string, error)
	ListDetailed(path string) ([]FileEntry, error)
	Read(path string) ([]byte, error)
	Write(path string, data []byte) error
	MkdirAll(path string) error
	Move(src, dstDir, newName string) error
	Copy(src, dstDir string) error
	Rename(path, newName string) error
	Exists(path string) bool
	Delete(path string) error
	DeleteDir(path string) error
	Download(remotePath, localPath string) error
	Upload(localPath, remotePath string) error
	GetURL(path string) (string, error) // 获取直链（用于 ffprobe）
}

func NewFileSystem(p *models.AVPath) (FileSystem, error) {
	switch p.SourceType {
	case "115":
		return NewFS115(p)
	case "openlist":
		return NewFSOpenList(p)
	case "local":
		return NewFSLocal()
	default:
		return nil, fmt.Errorf("不支持的源类型: %s", p.SourceType)
	}
}
