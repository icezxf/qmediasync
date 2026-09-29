package avscrape

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Register 在 main.go 里调用
func Register(r *gin.Engine, db *gorm.DB) error {
	if err := AutoMigrate(db); err != nil {
		return err
	}
	ctrl := NewController(db)
	g := r.Group("/api/avscrape")
	{
		g.GET("/config", ctrl.GetConfig)
		g.POST("/config", ctrl.SaveConfig)
		g.POST("/scrape", ctrl.Scrape)
		g.GET("/library", ctrl.ListMedia)
		g.GET("/library/:id", ctrl.GetMedia)
	}
	return nil
}