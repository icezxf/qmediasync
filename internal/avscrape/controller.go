package avscrape

import (
"net/http"
"strconv"
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
func (c *Controller) GetConfig(ctx *gin.Context) {
cfg, err := LoadConfig(c.DB)
if err != nil {
ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
return
}
ctx.JSON(http.StatusOK, cfg)
}
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
func (c *Controller) Scrape(ctx *gin.Context) {
var req struct{ Code string `json:"code"` }
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
func (c *Controller) ListMedia(ctx *gin.Context) {
page, _ := strconv.Atoi(ctx.DefaultQuery("page", "1"))
pageSize, _ := strconv.Atoi(ctx.DefaultQuery("page_size", "20"))
if page < 1 { page = 1 }
if pageSize < 1 || pageSize > 100 { pageSize = 20 }
list, total, err := c.Svc.ListMedia(page, pageSize)
if err != nil {
ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
return
}
ctx.JSON(http.StatusOK, gin.H{"list": list, "total": total})
}
func (c *Controller) GetMedia(ctx *gin.Context) {
id, _ := strconv.Atoi(ctx.Param("id"))
m, err := c.Svc.GetMedia(uint(id))
if err != nil {
ctx.JSON(http.StatusNotFound, gin.H{"error": "not found"})
return
}
ctx.JSON(http.StatusOK, m)
}
func (c *Controller) ListPaths(ctx *gin.Context) {
var list []AVPath
c.DB.Order("created_at desc").Find(&list)
ctx.JSON(http.StatusOK, gin.H{"list": list})
}
func (c *Controller) CreatePath(ctx *gin.Context) {
var p AVPath
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
func (c *Controller) DeletePath(ctx *gin.Context) {
id, _ := strconv.Atoi(ctx.Param("id"))
c.DB.Delete(&AVPath{}, id)
ctx.JSON(http.StatusOK, gin.H{"ok": true})
}
func (c *Controller) ScanPath(ctx *gin.Context) {
id, _ := strconv.Atoi(ctx.Param("id"))
scanner := &Scanner{DB: c.DB, Svc: c.Svc, FS: c.getFileSystem()}
go scanner.Scan(uint(id))
ctx.JSON(http.StatusOK, gin.H{"ok": true, "msg": "扫描已启动"})
}
func (c *Controller) ListTasks(ctx *gin.Context) {
page, _ := strconv.Atoi(ctx.DefaultQuery("page", "1"))
pageSize, _ := strconv.Atoi(ctx.DefaultQuery("page_size", "20"))
var list []AVTask
var total int64
c.DB.Model(&AVTask{}).Count(&total)
c.DB.Order("created_at desc").Offset((page - 1) * pageSize).Limit(pageSize).Find(&list)
ctx.JSON(http.StatusOK, gin.H{"list": list, "total": total})
}
func (c *Controller) getFileSystem() FileSystem {
return nil
}
