package avscrape

import (
	"fmt"
	"time"
)

// FileEntry 文件条目
type FileEntry struct {
	Name     string
	Path     string
	IsDir    bool
	Size     int64
	ID       string // 115 fileId 或 OpenList 路径
	PickCode string // 115 pickcode
	MTime    time.Time
}

// FileSystem 文件系统抽象接口
type FileSystem interface {
	List(path string) ([]string, error)                 // 列出文件名
	ListDetailed(path string) ([]FileEntry, error)      // 列出文件详情
	Read(path string) ([]byte, error)                   // 读取文件
	Write(path string, data []byte) error               // 写入文件（覆盖）
	MkdirAll(path string) error                         // 递归创建目录
	Move(src, dstDir, newName string) error             // 移动文件（可改名）
	Copy(src, dstDir string) error                      // 复制文件
	Rename(path, newName string) error                  // 原地改名
	Exists(path string) bool                            // 判断存在
	Delete(path string) error                           // 删除文件
	DeleteDir(path string) error                        // 删除目录（递归）
	Download(remotePath, localPath string) error        // 下载到本地
	Upload(localPath, remotePath string) error          // 上传到远程
}

// NewFileSystem 根据 AVPath 的 SourceType 创建对应实现
func NewFileSystem(p *AVPath) (FileSystem, error) {
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