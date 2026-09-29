package avscrape

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"

	"Q115-STRM/internal/helpers"
	"Q115-STRM/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type Controller struct {
	DB  *gorm.DB
	Svc *Service
}

func NewController(db *gorm.DB) *Controller {
	return &Controller{DB: db, Svc: NewService(db)}
}

// GetConfig GET /api/avscrape/config
func (c *Controller) GetConfig(ctx *gin.Context) {
	cfg, err := LoadConfig(c.DB)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, cfg)
}

// SaveConfig POST /api/avscrape/config
func (c *Controller) SaveConfig(ctx *gin.Context) {
	var cfg Config
	if err := ctx.ShouldBindJSON(&cfg); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := SaveConfig(c.DB, &cfg); err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, gin.H{"ok": true})
}

// Scrape POST /api/avscrape/scrape  { "code": "SNOS-377" }
func (c *Controller) Scrape(ctx *gin.Context) {
	var req struct {
		Code string `json:"code"`
	}
	if err := ctx.ShouldBindJSON(&req); err != nil || req.Code == "" {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "code required"})
		return
	}
	result, err := c.Svc.Scrape(req.Code)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, result)
}

// ListMedia GET /api/avscrape/library?page=1&page_size=20
func (c *Controller) ListMedia(ctx *gin.Context) {
	page, _ := strconv.Atoi(ctx.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(ctx.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	list, total, err := c.Svc.ListMedia(page, pageSize)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, gin.H{"list": list, "total": total})
}

// GetMedia GET /api/avscrape/library/:id
func (c *Controller) GetMedia(ctx *gin.Context) {
	id, _ := strconv.Atoi(ctx.Param("id"))
	m, err := c.Svc.GetMedia(uint(id))
	if err != nil {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	ctx.JSON(http.StatusOK, m)
}

// ListPaths GET /api/avscrape/paths
func (c *Controller) ListPaths(ctx *gin.Context) {
	var list []models.AVPath
	c.DB.Order("created_at desc").Find(&list)
	ctx.JSON(http.StatusOK, gin.H{"list": list})
}

// CreatePath POST /api/avscrape/paths
func (c *Controller) CreatePath(ctx *gin.Context) {
	var p models.AVPath
	if err := ctx.ShouldBindJSON(&p); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := c.DB.Create(&p).Error; err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, p)
}

// UpdatePath PUT /api/avscrape/paths/:id
func (c *Controller) UpdatePath(ctx *gin.Context) {
	id, _ := strconv.Atoi(ctx.Param("id"))
	var p models.AVPath
	if err := ctx.ShouldBindJSON(&p); err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p.ID = uint(id)
	if err := c.DB.Save(&p).Error; err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	ctx.JSON(http.StatusOK, p)
}

// GetPath GET /api/avscrape/paths/:id
func (c *Controller) GetPath(ctx *gin.Context) {
	id, _ := strconv.Atoi(ctx.Param("id"))
	var p models.AVPath
	if err := c.DB.First(&p, id).Error; err != nil {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	ctx.JSON(http.StatusOK, p)
}

// DeletePath DELETE /api/avscrape/paths/:id
func (c *Controller) DeletePath(ctx *gin.Context) {
	id, _ := strconv.Atoi(ctx.Param("id"))
	c.DB.Delete(&models.AVPath{}, id)
	ctx.JSON(http.StatusOK, gin.H{"ok": true})
}

// ScanPath POST /api/avscrape/paths/:id/scan
func (c *Controller) ScanPath(ctx *gin.Context) {
	id, _ := strconv.Atoi(ctx.Param("id"))

	var p models.AVPath
	if err := c.DB.First(&p, id).Error; err != nil {
		ctx.JSON(http.StatusNotFound, gin.H{"error": "AV 刮削目录不存在"})
		return
	}
	if !p.Enable {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "该目录未启用，请先编辑并开启「启用」开关"})
		return
	}
	if p.SourcePath == "" {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "源路径为空，请先编辑目录并填写源路径"})
		return
	}

	fs, err := NewFileSystem(&p)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "创建文件系统失败: " + err.Error()})
		return
	}

	// 递归统计
	videoFiles, err := walkVideos(fs, p.SourcePath, 0)
	if err != nil {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "遍历源目录失败: " + err.Error()})
		return
	}

	recognizedCount := 0
	unknownSamples := []string{}
	for _, fullPath := range videoFiles {
		name := filepath.Base(fullPath)
		if ExtractCode(name) != "" {
			recognizedCount++
		} else if len(unknownSamples) < 5 {
			unknownSamples = append(unknownSamples, name)
		}
	}

	helpers.AppLogger.Infof("[AV扫描] 目录=%s 视频=%d 可识别=%d",
		p.SourcePath, len(videoFiles), recognizedCount)

	ctx.JSON(http.StatusOK, gin.H{
		"ok":              true,
		"msg":             fmt.Sprintf("目录 %s 共 %d 个视频，可识别番号 %d 个", p.SourcePath, len(videoFiles), recognizedCount),
		"source_path":     p.SourcePath,
		"video_count":     len(videoFiles),
		"recognizable":    recognizedCount,
		"unknown_samples": unknownSamples,
	})

	if len(videoFiles) == 0 {
		return
	}

	scanner := NewScanner(c.DB)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				helpers.AppLogger.Errorf("[AV扫描] panic: %v", r)
			}
		}()
		if err := scanner.Scan(uint(id)); err != nil {
			helpers.AppLogger.Errorf("[AV扫描] 失败: %v", err)
			c.DB.Create(&models.AVTask{
				Code:     "",
				FilePath: p.SourcePath,
				Status:   "failed",
				Message:  "扫描失败: " + err.Error(),
			})
		}
	}()
}

// ListTasks GET /api/avscrape/tasks
func (c *Controller) ListTasks(ctx *gin.Context) {
	page, _ := strconv.Atoi(ctx.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(ctx.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	var list []models.AVTask
	var total int64
	c.DB.Model(&models.AVTask{}).Count(&total)
	c.DB.Order("created_at desc").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&list)
	ctx.JSON(http.StatusOK, gin.H{"list": list, "total": total})
}

// ClearTasks DELETE /api/avscrape/tasks
func (c *Controller) ClearTasks(ctx *gin.Context) {
	c.DB.Where("1 = 1").Delete(&models.AVTask{})
	ctx.JSON(http.StatusOK, gin.H{"ok": true})
}
