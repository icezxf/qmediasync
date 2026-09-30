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
	"Q115-STRM/internal/v115open"
)

// waitLimit 简单节流，避免 115 风控
func waitLimit() {
	time.Sleep(500 * time.Millisecond)
}

type FS115 struct {
	client *v115open.OpenClient
	ctx    context.Context
}

func NewFS115(p *models.AVPath) (*FS115, error) {
	account, err := models.GetAccountById(p.AccountID)
	if err != nil {
		return nil, fmt.Errorf("获取账号失败: %w", err)
	}
	client := account.Get115Client()
	if client == nil {
		return nil, fmt.Errorf("获取115客户端失败")
	}
	return &FS115{client: client, ctx: context.Background()}, nil
}

func (f *FS115) List(path string) ([]string, error) {
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

func (f *FS115) ListDetailed(path string) ([]FileEntry, error) {
	waitLimit()
	detail, err := f.client.GetFsDetailByPath(f.ctx, path)
	if err != nil || detail == nil || detail.FileId == "" {
		return nil, fmt.Errorf("获取目录详情失败: %s, %v", path, err)
	}
	waitLimit()
	resp, err := f.client.GetFsList(f.ctx, detail.FileId, true, false, true, 0, 1150)
	if err != nil {
		return nil, err
	}
	entries := make([]FileEntry, 0, len(resp.Data))
	for _, item := range resp.Data {
		entries = append(entries, FileEntry{
			Name:     item.FileName,
			Path:     path + "/" + item.FileName,
			IsDir:    item.FileCategory == v115open.TypeDir,
			Size:     item.FileSize,
			ID:       item.FileId,
			PickCode: item.PickCode,
			MTime:    time.Unix(item.Ptime, 0),
		})
	}
	return entries, nil
}

func (f *FS115) Read(path string) ([]byte, error) {
	waitLimit()
	detail, err := f.client.GetFsDetailByPath(f.ctx, path)
	if err != nil || detail == nil || detail.FileId == "" {
		return nil, fmt.Errorf("获取文件详情失败: %s, %v", path, err)
	}
	url := f.client.GetDownloadUrl(f.ctx, detail.PickCode, v115open.DEFAULTUA, false)
	if url == "" {
		return nil, fmt.Errorf("获取下载链接失败: %s", path)
	}
	return helpers.ReadFromUrl(url, v115open.DEFAULTUA)
}

func (f *FS115) Write(path string, data []byte) error {
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

	parentPath := filepath.ToSlash(filepath.Dir(path))
	waitLimit()
	parentDetail, err := f.client.GetFsDetailByPath(f.ctx, parentPath)
	if err != nil || parentDetail == nil || parentDetail.FileId == "" {
		return fmt.Errorf("获取父目录失败: %s", parentPath)
	}
	waitLimit()
	_, err = f.client.Upload(f.ctx, tmpPath, parentDetail.FileId, "", "")
	return err
}

func (f *FS115) MkdirAll(path string) error {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	currentPath := ""
	var currentId string = "0"

	for _, part := range parts {
		if part == "" {
			continue
		}
		if currentPath == "" {
			currentPath = "/" + part
		} else {
			currentPath = currentPath + "/" + part
		}
		waitLimit()
		detail, err := f.client.GetFsDetailByPath(f.ctx, currentPath)
		if err == nil && detail != nil && detail.FileId != "" {
			currentId = detail.FileId
			continue
		}
		waitLimit()
		newId, err := f.client.MkDir(f.ctx, currentId, part)
		if err != nil {
			return fmt.Errorf("创建目录失败 %s: %w", currentPath, err)
		}
		currentId = newId
	}
	return nil
}

func (f *FS115) Move(src, dstDir, newName string) error {
	waitLimit()
	srcDetail, err := f.client.GetFsDetailByPath(f.ctx, src)
	if err != nil || srcDetail == nil || srcDetail.FileId == "" {
		return fmt.Errorf("获取源文件失败: %s", src)
	}
	waitLimit()
	dstDetail, err := f.client.GetFsDetailByPath(f.ctx, dstDir)
	if err != nil || dstDetail == nil || dstDetail.FileId == "" {
		return fmt.Errorf("获取目标目录失败: %s", dstDir)
	}
	waitLimit()
	if _, err := f.client.Move(f.ctx, []string{srcDetail.FileId}, dstDetail.FileId); err != nil {
		return err
	}
	if newName != "" && newName != srcDetail.FileName {
		waitLimit()
		_, err = f.client.ReName(f.ctx, srcDetail.FileId, newName)
	}
	return err
}

func (f *FS115) Copy(src, dstDir string) error {
	waitLimit()
	srcDetail, err := f.client.GetFsDetailByPath(f.ctx, src)
	if err != nil || srcDetail == nil || srcDetail.FileId == "" {
		return fmt.Errorf("获取源文件失败: %s", src)
	}
	waitLimit()
	dstDetail, err := f.client.GetFsDetailByPath(f.ctx, dstDir)
	if err != nil || dstDetail == nil || dstDetail.FileId == "" {
		return fmt.Errorf("获取目标目录失败: %s", dstDir)
	}
	waitLimit()
	_, err = f.client.Copy(f.ctx, []string{srcDetail.FileId}, dstDetail.FileId, false)
	return err
}

func (f *FS115) Rename(path, newName string) error {
	waitLimit()
	detail, err := f.client.GetFsDetailByPath(f.ctx, path)
	if err != nil || detail == nil || detail.FileId == "" {
		return fmt.Errorf("获取文件失败: %s", path)
	}
	waitLimit()
	_, err = f.client.ReName(f.ctx, detail.FileId, newName)
	return err
}

func (f *FS115) Exists(path string) bool {
	waitLimit()
	detail, err := f.client.GetFsDetailByPath(f.ctx, path)
	return err == nil && detail != nil && detail.FileId != ""
}

func (f *FS115) Delete(path string) error {
	waitLimit()
	detail, err := f.client.GetFsDetailByPath(f.ctx, path)
	if err != nil || detail == nil || detail.FileId == "" {
		return nil
	}
	parentPath := filepath.ToSlash(filepath.Dir(path))
	waitLimit()
	parentDetail, err := f.client.GetFsDetailByPath(f.ctx, parentPath)
	if err != nil || parentDetail == nil || parentDetail.FileId == "" {
		return fmt.Errorf("获取父目录失败: %s", parentPath)
	}
	waitLimit()
	_, err = f.client.Del(f.ctx, []string{detail.FileId}, parentDetail.FileId)
	return err
}

func (f *FS115) DeleteDir(path string) error {
	return f.Delete(path)
}

func (f *FS115) Download(remotePath, localPath string) error {
	waitLimit()
	detail, err := f.client.GetFsDetailByPath(f.ctx, remotePath)
	if err != nil || detail == nil || detail.FileId == "" {
		return fmt.Errorf("获取文件详情失败: %s", remotePath)
	}
	url := f.client.GetDownloadUrl(f.ctx, detail.PickCode, v115open.DEFAULTUA, false)
	if url == "" {
		return fmt.Errorf("获取下载链接失败: %s", remotePath)
	}
	return helpers.DownloadFile(url, localPath, v115open.DEFAULTUA)
}

func (f *FS115) Upload(localPath, remotePath string) error {
	parentPath := filepath.ToSlash(filepath.Dir(remotePath))
	waitLimit()
	parentDetail, err := f.client.GetFsDetailByPath(f.ctx, parentPath)
	if err != nil || parentDetail == nil || parentDetail.FileId == "" {
		return fmt.Errorf("获取父目录失败: %s", parentPath)
	}
	waitLimit()
	_, err = f.client.Upload(f.ctx, localPath, parentDetail.FileId, "", "")
	return err
}