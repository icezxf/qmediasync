package avscrape

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"Q115-STRM/internal/helpers"
	"Q115-STRM/internal/models"

	"gorm.io/gorm"
)

var videoExts = map[string]bool{
	".mp4": true, ".mkv": true, ".avi": true, ".wmv": true,
	".mov": true, ".flv": true, ".ts": true, ".m2ts": true,
	".iso": true, ".rmvb": true, ".strm": true,
}

var invalidPathChars = regexp.MustCompile(`[\\/:*?"<>|]`)

type Scanner struct {
	DB  *gorm.DB
	Svc *Service
}

func NewScanner(db *gorm.DB) *Scanner {
	return &Scanner{DB: db, Svc: NewService(db)}
}

func (s *Scanner) Scan(pathID uint) error {
	var path models.AVPath
	if err := s.DB.First(&path, pathID).Error; err != nil {
		return err
	}
	if !path.Enable {
		return fmt.Errorf("目录未启用: %d", pathID)
	}

	fs, err := NewFileSystem(&path)
	if err != nil {
		return fmt.Errorf("创建文件系统失败: %w", err)
	}

	files, err := fs.List(path.SourcePath)
	if err != nil {
		return fmt.Errorf("列出目录失败: %w", err)
	}

	helpers.AppLogger.Infof("[AV扫描] 目录 %s 共找到 %d 个文件", path.SourcePath, len(files))

	for _, name := range files {
		ext := strings.ToLower(filepath.Ext(name))
		if !videoExts[ext] {
			continue
		}
		fullPath := path.SourcePath + "/" + name
		code := ExtractCode(name)
		if code == "" {
			s.recordTask("", fullPath, "failed", "无法识别番号", "")
			continue
		}
		helpers.AppLogger.Infof("[AV扫描] 处理文件 %s → 番号 %s", fullPath, code)

		result, err := s.Svc.Scrape(code)
		if err != nil {
			s.recordTask(code, fullPath, "failed", err.Error(), "")
			continue
		}

		if err := s.writeMediaFiles(fs, &path, result, fullPath); err != nil {
			s.recordTask(code, fullPath, "failed", err.Error(), result.Source)
			continue
		}

		if path.Mode == "scrape_and_rename" || path.Mode == "rename_only" {
			media := MediaFromResult(result)
			if err := s.organize(fs, &path, media, fullPath, name); err != nil {
				s.recordTask(code, fullPath, "failed", err.Error(), result.Source)
				continue
			}
		}

		s.recordTask(code, fullPath, "done", "", result.Source)
	}

	s.DB.Model(&path).Update("last_scan_at", now())
	return nil
}

// renderFolderTemplate 渲染文件夹模板
func renderFolderTemplate(tpl string, m *models.AVMedia) string {
	if tpl == "" {
		tpl = "{code}"
	}

	actorName := ""
	if m.Actors != "" {
		var list []Actor
		if err := json.Unmarshal([]byte(m.Actors), &list); err == nil && len(list) > 0 {
			actorName = list[0].Name
		}
	}

	year := ""
	if len(m.ReleaseDate) >= 4 {
		year = m.ReleaseDate[:4]
	}

	r := tpl
	r = strings.ReplaceAll(r, "{actors}", sanitizePathSegment(actorName))
	r = strings.ReplaceAll(r, "{num}", sanitizePathSegment(m.Code))
	r = strings.ReplaceAll(r, "{code}", sanitizePathSegment(m.Code))
	r = strings.ReplaceAll(r, "{title}", sanitizePathSegment(m.Title))
	r = strings.ReplaceAll(r, "{year}", sanitizePathSegment(year))
	r = strings.ReplaceAll(r, "{studio}", sanitizePathSegment(m.Studio))
	r = strings.ReplaceAll(r, "{label}", sanitizePathSegment(m.Label))
	r = strings.ReplaceAll(r, "{series}", sanitizePathSegment(m.Series))
	return r
}

func sanitizePathSegment(s string) string {
	s = invalidPathChars.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// writeMediaFiles 写 NFO、下载图片、生成 .strm 预告片
func (s *Scanner) writeMediaFiles(fs FileSystem, path *models.AVPath, r *ScrapeResult, videoPath string) error {
	dir := filepath.Dir(videoPath)
	base := strings.TrimSuffix(filepath.Base(videoPath), filepath.Ext(videoPath))

	// ===== 1. poster：严格只找竖版，找不到就用 DMM 兜底 =====
	posterOK := false
	if url, ok := downloadImageByOrientation(fs, r.ImageCandidates, dir+"/poster.jpg", "portrait", "poster"); ok {
		r.Poster = url
		posterOK = true
	}
	if !posterOK {
		// 用番号拼 DMM URL 再试
		if dmmURL := dmmPosterURL(r.Code); dmmURL != "" {
			if tryDMMPoster(fs, dmmURL, dir+"/poster.jpg") {
				r.Poster = dmmURL
				posterOK = true
			}
		}
	}
	if !posterOK {
		helpers.AppLogger.Warnf("[AV元数据] %s 未找到竖版 poster，跳过", r.Code)
		r.Poster = ""
	}

	// ===== 2. fanart：严格只找横版 =====
	if url, ok := downloadImageByOrientation(fs, r.ImageCandidates, dir+"/fanart.jpg", "landscape", "fanart"); ok {
		r.Fanart = url
	} else {
		helpers.AppLogger.Warnf("[AV元数据] %s 未找到横版 fanart，跳过", r.Code)
		r.Fanart = ""
	}

	// ===== 3. 写 NFO =====
	if err := fs.Write(dir+"/"+base+".nfo", []byte(GenerateNFO(r))); err != nil {
		return fmt.Errorf("写 NFO 失败: %w", err)
	}

	// ===== 4. 剧照 =====
	if len(r.PreviewImages) > 0 {
		_ = fs.MkdirAll(dir + "/extrafanart")
		for i, url := range r.PreviewImages {
			if data, err := downloadImage(url); err == nil {
				_ = fs.Write(fmt.Sprintf("%s/extrafanart/scene-%02d.jpg", dir, i+1), data)
			}
		}
	}

	// ===== 5. 预告片 =====
	if r.Trailer != "" {
		_ = fs.MkdirAll(dir + "/trailers")
		_ = fs.Write(dir+"/trailers/trailer.strm", []byte(r.Trailer))
	}
	return nil
}

// dmmPosterURL 根据番号拼 DMM 竖版海报 URL
// 规则：字母前缀小写 + 数字补齐 5 位，例如 MIDV-192 → midv00192
// 返回空字符串表示无法拼接（FC2、1Pondo 等特殊番号）
func dmmPosterURL(code string) string {
	// 只保留字母数字
	var sb strings.Builder
	for _, r := range code {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		}
	}
	cleaned := strings.ToUpper(sb.String())

	// 分离字母前缀和数字后缀
	i := 0
	for i < len(cleaned) && cleaned[i] >= 'A' && cleaned[i] <= 'Z' {
		i++
	}
	letters := cleaned[:i]
	digits := cleaned[i:]

	// 无字母（如 1Pondo）或数字超过 5 位（如 FC2-PPV-1234567）都跳过
	if letters == "" || digits == "" || len(digits) > 5 {
		return ""
	}

	var num int
	if _, err := fmt.Sscanf(digits, "%d", &num); err != nil {
		return ""
	}
	filename := strings.ToLower(letters) + fmt.Sprintf("%05d", num)
	return "https://pics.dmm.co.jp/digital/video/" + filename + "/" + filename + "pl.jpg"
}

// tryDMMPoster 尝试从 DMM 下载竖版海报
func tryDMMPoster(fs FileSystem, url, dstPath string) bool {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Referer", "https://www.dmm.co.jp/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		helpers.AppLogger.Warnf("[AV元数据] DMM poster 请求失败: %v", err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		helpers.AppLogger.Warnf("[AV元数据] DMM poster HTTP %d", resp.StatusCode)
		return false
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil || len(data) < 1000 {
		helpers.AppLogger.Warnf("[AV元数据] DMM poster 数据无效（%d 字节）", len(data))
		return false
	}

	// 验证是竖版图
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		helpers.AppLogger.Warnf("[AV元数据] DMM poster 不是图片: %v", err)
		return false
	}
	if cfg.Width >= cfg.Height {
		helpers.AppLogger.Warnf("[AV元数据] DMM poster 不是竖版（%dx%d）", cfg.Width, cfg.Height)
		return false
	}

	if err := fs.Write(dstPath, data); err != nil {
		helpers.AppLogger.Warnf("[AV元数据] DMM poster 写盘失败: %v", err)
		return false
	}
	helpers.AppLogger.Infof("[AV元数据] poster 从 DMM 下载成功（%dx%d）: %s", cfg.Width, cfg.Height, url)
	return true
}

// downloadImageWithSize 下载图片并读取尺寸
func downloadImageWithSize(url string) ([]byte, int, int, error) {
	data, err := downloadImage(url)
	if err != nil {
		return nil, 0, 0, err
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return data, 0, 0, nil
	}
	return data, cfg.Width, cfg.Height, nil
}

// downloadImageByOrientation 严格按方向下载图片，不匹配就跳过
func downloadImageByOrientation(fs FileSystem, candidates []string, dstPath, wantOrientation, label string) (string, bool) {
	for i, url := range candidates {
		data, w, h, err := downloadImageWithSize(url)
		if err != nil {
			helpers.AppLogger.Warnf("[AV元数据] %s 候选[%d]下载失败: %s => %v", label, i, url, err)
			continue
		}
		if w == 0 || h == 0 {
			helpers.AppLogger.Warnf("[AV元数据] %s 候选[%d]尺寸未知，跳过", label, i)
			continue
		}
		isPortrait := h > w
		match := (wantOrientation == "portrait" && isPortrait) ||
			(wantOrientation == "landscape" && !isPortrait)
		if !match {
			helpers.AppLogger.Infof("[AV元数据] %s 候选[%d]方向不匹配（%dx%d），跳过", label, i, w, h)
			continue
		}
		if err := fs.Write(dstPath, data); err != nil {
			helpers.AppLogger.Warnf("[AV元数据] %s 写盘失败: %v", label, err)
			continue
		}
		helpers.AppLogger.Infof("[AV元数据] %s 下载成功（候选[%d]，%dx%d）: %s", label, i, w, h, url)
		return url, true
	}
	helpers.AppLogger.Warnf("[AV元数据] %s 未找到 %s 方向的图片", label, wantOrientation)
	return "", false
}

// organize 按模板整理文件到目标路径
func (s *Scanner) organize(fs FileSystem, path *models.AVPath, media *models.AVMedia, videoPath, videoName string) error {
	subPath := renderFolderTemplate(path.NameTemplate, media)
	subPath = strings.Trim(subPath, "/")
	for strings.Contains(subPath, "//") {
		subPath = strings.ReplaceAll(subPath, "//", "/")
	}
	if subPath == "" {
		subPath = media.Code
	}
	targetDir := strings.TrimRight(path.TargetPath, "/") + "/" + subPath
	helpers.AppLogger.Infof("[AV整理] 目标目录: %s (模板=%s)", targetDir, path.NameTemplate)

	if err := fs.MkdirAll(targetDir); err != nil {
		return fmt.Errorf("创建目标目录失败: %w", err)
	}

	ext := filepath.Ext(videoName)
	newName := media.Code + ext
	srcDir := filepath.Dir(videoPath)
	base := strings.TrimSuffix(filepath.Base(videoPath), ext)

	switch path.MoveMethod {
	case "copy":
		if err := fs.Copy(videoPath, targetDir); err != nil {
			return fmt.Errorf("复制视频失败: %w", err)
		}
		if filepath.Base(videoPath) != newName {
			newPath := targetDir + "/" + filepath.Base(videoPath)
			if err := fs.Rename(newPath, newName); err != nil {
				return fmt.Errorf("重命名视频失败: %w", err)
			}
		}
	default:
		if err := fs.Move(videoPath, targetDir, newName); err != nil {
			return fmt.Errorf("移动视频失败: %w", err)
		}
	}
	helpers.AppLogger.Infof("[AV整理] 移动视频: %s → %s/", filepath.Base(videoPath), targetDir)

	nfoSrc := srcDir + "/" + base + ".nfo"
	if fs.Exists(nfoSrc) {
		if err := fs.Move(nfoSrc, targetDir, ""); err != nil {
			helpers.AppLogger.Warnf("[AV整理] 移动 NFO 失败: %v", err)
		} else {
			helpers.AppLogger.Infof("[AV整理] 移动 NFO → %s/", targetDir)
		}
	}

	for _, img := range []string{"poster.jpg", "fanart.jpg", "thumb.jpg"} {
		src := srcDir + "/" + img
		if fs.Exists(src) {
			if err := fs.Move(src, targetDir, ""); err != nil {
				helpers.AppLogger.Warnf("[AV整理] 移动 %s 失败: %v", img, err)
			} else {
				helpers.AppLogger.Infof("[AV整理] 移动 %s → %s/", img, targetDir)
			}
		}
	}

	s.moveDir(fs, srcDir+"/extrafanart", targetDir+"/extrafanart", "extrafanart")
	s.moveDir(fs, srcDir+"/trailers", targetDir+"/trailers", "trailers")

	return nil
}

func (s *Scanner) moveDir(fs FileSystem, src, dst, label string) {
	if !fs.Exists(src) {
		return
	}
	if err := fs.MkdirAll(dst); err != nil {
		helpers.AppLogger.Warnf("[AV整理] 创建 %s 目录失败: %v", label, err)
		return
	}
	names, err := fs.List(src)
	if err != nil {
		helpers.AppLogger.Warnf("[AV整理] 列出 %s 失败: %v", label, err)
		return
	}
	for _, n := range names {
		if err := fs.Move(src+"/"+n, dst, ""); err != nil {
			helpers.AppLogger.Warnf("[AV整理] 移动 %s/%s 失败: %v", label, n, err)
		}
	}
	_ = fs.DeleteDir(src)
	helpers.AppLogger.Infof("[AV整理] 移动 %s/ (%d 项) → %s/", label, len(names), dst)
}

func (s *Scanner) recordTask(code, filePath, status, msg, provider string) {
	s.DB.Create(&models.AVTask{
		Code:     code,
		FilePath: filePath,
		Status:   status,
		Message:  msg,
		Provider: provider,
	})
}
