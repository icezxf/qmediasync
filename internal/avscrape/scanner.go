package avscrape

import (
"fmt"
"path/filepath"
"strings"
"gorm.io/gorm"
)

var videoExts = map[string]bool{
".mp4": true, ".mkv": true, ".avi": true, ".wmv": true,
".mov": true, ".flv": true, ".ts": true, ".m2ts": true,
".iso": true, ".rmvb": true, ".strm": true,
}

type FileSystem interface {
List(path string) ([]string, error)
Read(path string) ([]byte, error)
Write(path string, data []byte) error
MkdirAll(path string) error
Rename(src, dst string) error
Exists(path string) bool
}
type Scanner struct {
DB  *gorm.DB
Svc *Service
FS  FileSystem
}
func (s *Scanner) Scan(pathID uint) error {
var path AVPath
if err := s.DB.First(&path, pathID).Error; err != nil {
return err
}
if !path.Enable {
return fmt.Errorf("目录未启用: %d", pathID)
}
files, err := s.FS.List(path.SourcePath)
if err != nil {
return fmt.Errorf("列出目录失败: %w", err)
}
for _, f := range files {
ext := strings.ToLower(filepath.Ext(f))
if !videoExts[ext] {
continue
}
fullPath := path.SourcePath + "/" + f
code := ExtractCode(f)
if code == "" {
s.recordTask("", fullPath, "failed", "无法识别番号", "")
continue
}
var existing AVMedia
if err := s.DB.Where("code = ?", code).First(&existing).Error; err == nil {
if path.Mode == "scrape_and_rename" || path.Mode == "rename_only" {
_ = s.organize(&path, &existing, fullPath)
}
continue
}
result, err := s.Svc.Scrape(code)
if err != nil {
s.recordTask(code, fullPath, "failed", err.Error(), "")
continue
}
if err := s.writeMediaFiles(&path, result, fullPath); err != nil {
s.recordTask(code, fullPath, "failed", err.Error(), result.Source)
continue
}
if path.Mode == "scrape_and_rename" || path.Mode == "rename_only" {
if err := s.organize(&path, MediaFromResult(result), fullPath); err != nil {
s.recordTask(code, fullPath, "failed", err.Error(), result.Source)
continue
}
}
s.recordTask(code, fullPath, "done", "", result.Source)
}
s.DB.Model(&path).Update("last_scan_at", now())
return nil
}
func (s *Scanner) writeMediaFiles(path *AVPath, r *ScrapeResult, videoPath string) error {
dir := filepath.Dir(videoPath)
base := strings.TrimSuffix(filepath.Base(videoPath), filepath.Ext(videoPath))
if err := s.FS.Write(dir+"/"+base+".nfo", []byte(GenerateNFO(r))); err != nil {
return fmt.Errorf("写 NFO 失败: %w", err)
}
if r.Poster != "" {
if data, err := downloadImage(r.Poster); err == nil {
_ = s.FS.Write(dir+"/poster.jpg", data)
}
}
if r.Fanart != "" {
if data, err := downloadImage(r.Fanart); err == nil {
_ = s.FS.Write(dir+"/fanart.jpg", data)
}
}
if len(r.PreviewImages) > 0 {
_ = s.FS.MkdirAll(dir + "/extrafanart")
for i, url := range r.PreviewImages {
if data, err := downloadImage(url); err == nil {
_ = s.FS.Write(fmt.Sprintf("%s/extrafanart/scene-%02d.jpg", dir, i+1), data)
}
}
}
if r.Trailer != "" {
_ = s.FS.MkdirAll(dir + "/trailers")
_ = s.FS.Write(dir+"/trailers/trailer.strm", []byte(r.Trailer))
}
return nil
}
func (s *Scanner) organize(path *AVPath, m *AVMedia, videoPath string) error {
targetDir := path.TargetPath + "/" + m.Code
if err := s.FS.MkdirAll(targetDir); err != nil {
return err
}
ext := filepath.Ext(videoPath)
newPath := targetDir + "/" + m.Code + ext
switch path.MoveMethod {
case "copy":
data, err := s.FS.Read(videoPath)
if err != nil {
return err
}
return s.FS.Write(newPath, data)
default:
return s.FS.Rename(videoPath, newPath)
}
}
func (s *Scanner) recordTask(code, filePath, status, msg, provider string) {
s.DB.Create(&AVTask{Code: code, FilePath: filePath, Status: status, Message: msg, Provider: provider})
}
