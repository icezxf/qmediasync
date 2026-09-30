package avscrape

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"Q115-STRM/internal/helpers"
	"Q115-STRM/internal/models"
	"Q115-STRM/internal/openlist"
	"Q115-STRM/internal/v115open"
)

func slowDown() {
	time.Sleep(500 * time.Millisecond)
}

type FSOpenList struct {
	client *openlist.Client
	ctx    context.Context
}

func NewFSOpenList(p *models.AVPath) (*FSOpenList, error) {
	account, err := models.GetAccountById(p.AccountID)
	if err != nil {
		return nil, fmt.Errorf("获取账号失败: %w", err)
	}
	client := account.GetOpenListClient()
	if client == nil {
		return nil, fmt.Errorf("获取OpenList客户端失败")
	}
	return &FSOpenList{client: client, ctx: context.Background()}, nil
}

func (f *FSOpenList) List(path string) ([]string, error) {
	entries, err := f.ListDetailed(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return names, nil
}

func (f *FSOpenList) ListDetailed(path string) ([]FileEntry, error) {
	slowDown()
	resp, err := f.client.FileList(f.ctx, path, 1, 1000)
	if err != nil {
		return nil, err
	}
	entries := make([]FileEntry, 0, len(resp.Content))
	for _, item := range resp.Content {
		t, _ := time.Parse(time.RFC3339, item.Modified)
		entries = append(entries, FileEntry{
			Name:  item.Name,
			Path:  path + "/" + item.Name,
			IsDir: item.IsDir,
			Size:  item.Size,
			MTime: t,
		})
	}
	return entries, nil
}

func (f *FSOpenList) Read(path string) ([]byte, error) {
	slowDown()
	url := f.client.GetRawUrl(path)
	if url == "" {
		return nil, fmt.Errorf("获取直链失败: %s", path)
	}
	return helpers.ReadFromUrl(url, v115open.DEFAULTUA)
}

func (f *FSOpenList) Write(path string, data []byte) error {
	if f.Exists(path) {
		if err := f.Delete(path); err != nil {
			return fmt.Errorf("删除已存在文件失败: %w", err)
		}
	}
	tmpFile, err := os.CreateTemp("", "avscrape-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return err
	}
	tmpFile.Close()
	slowDown()
	_, err = f.client.Upload(tmpPath, path)
	return err
}

func (f *FSOpenList) MkdirAll(path string) error {
	slowDown()
	return f.client.Mkdir(path)
}

// Move 移动文件（OpenList 忽略 srcID，用路径）
func (f *FSOpenList) Move(src, srcID, dstDir, newName string) error {
	slowDown()
	if newName != "" && newName != filepath.Base(src) {
		if err := f.client.Rename(filepath.Dir(src), filepath.Base(src), newName); err != nil {
			return err
		}
		src = filepath.Join(filepath.Dir(src), newName)
	}
	slowDown()
	return f.client.Move(filepath.Dir(src), dstDir, []string{filepath.Base(src)})
}

func (f *FSOpenList) Copy(src, dstDir string) error {
	slowDown()
	return f.client.Copy(filepath.Dir(src), dstDir, []string{filepath.Base(src)})
}

func (f *FSOpenList) Rename(path, newName string) error {
	slowDown()
	return f.client.Rename(filepath.Dir(path), filepath.Base(path), newName)
}

func (f *FSOpenList) Exists(path string) bool {
	slowDown()
	detail, err := f.client.FileDetail(path)
	return err == nil && detail != nil && detail.Name != ""
}

func (f *FSOpenList) Delete(path string) error {
	slowDown()
	return f.client.Del(filepath.Dir(path), []string{filepath.Base(path)})
}

func (f *FSOpenList) DeleteDir(path string) error {
	slowDown()
	return f.client.Del(filepath.Dir(path), []string{filepath.Base(path)})
}

func (f *FSOpenList) Download(remotePath, localPath string) error {
	slowDown()
	url := f.client.GetRawUrl(remotePath)
	if url == "" {
		return fmt.Errorf("获取直链失败: %s", remotePath)
	}
	return helpers.DownloadFile(url, localPath, v115open.DEFAULTUA)
}

func (f *FSOpenList) Upload(localPath, remotePath string) error {
	if f.Exists(remotePath) {
		if err := f.Delete(remotePath); err != nil {
			return fmt.Errorf("删除已存在文件失败: %w", err)
		}
	}
	slowDown()
	_, err := f.client.Upload(localPath, remotePath)
	return err
}

// QueueUploads 把本地文件加入上传队列（异步走 GlobalUploadQueue）
func (f *FSOpenList) QueueUploads(files []LocalFile, dstDir string, accountId uint, sourceType string) (int, error) {
	if len(files) == 0 {
		return 0, nil
	}

	// 确保目标目录存在
	slowDown()
	_ = f.client.Mkdir(dstDir)

	createdSubDirs := map[string]bool{}

	count := 0
	for _, file := range files {
		remotePath := dstDir + "/" + file.RemoteName

		// 处理子目录
		parentPath := dstDir
		if idx := lastIndexByte(file.RemoteName, '/'); idx > 0 {
			parentPath = dstDir + "/" + file.RemoteName[:idx]
			if !createdSubDirs[parentPath] {
				slowDown()
				_ = f.client.Mkdir(parentPath)
				createdSubDirs[parentPath] = true
			}
		}

		fileName := filepath.Base(file.RemoteName)
		if err := models.AddUploadTaskFromAV(accountId, models.SourceType(sourceType), fileName, file.LocalPath, remotePath, parentPath); err != nil {
			helpers.AppLogger.Warnf("[AV上传队列] %s 加入队列失败: %v", file.RemoteName, err)
			continue
		}
		helpers.AppLogger.Infof("[AV上传队列] %s 已加入队列 (remote=%s, parentPath=%s)", file.RemoteName, remotePath, parentPath)
		count++
	}
	return count, nil
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

var _ = strings.TrimSpace