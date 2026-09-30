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

type LocalFile struct {
	LocalPath  string
	RemoteName string
}

type FileSystem interface {
	List(path string) ([]string, error)
	ListDetailed(path string) ([]FileEntry, error)
	Read(path string) ([]byte, error)
	Write(path string, data []byte) error
	MkdirAll(path string) error

	// Move 移动文件。
	// srcID：源文件 ID（115 用 fileId，可跳过源详情查询），其他源忽略
	Move(src, srcID, dstDir, newName string) error

	Copy(src, dstDir string) error
	Rename(path, newName string) error
	Exists(path string) bool
	Delete(path string) error
	DeleteDir(path string) error
	Download(remotePath, localPath string) error
	Upload(localPath, remotePath string) error

	QueueUploads(files []LocalFile, dstDir string, accountId uint, sourceType string) (int, error)
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