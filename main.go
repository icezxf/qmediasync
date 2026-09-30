package main

import (
	"Q115-STRM/emby302/config"
	"Q115-STRM/emby302/util/logs/colors"
	"Q115-STRM/emby302/web"
	"Q115-STRM/internal/avscrape"
	"Q115-STRM/internal/backup"
	"Q115-STRM/internal/controllers"
	"Q115-STRM/internal/db"
	"Q115-STRM/internal/db/database"
	"Q115-STRM/internal/github"
	"Q115-STRM/internal/helpers"
	"Q115-STRM/internal/migrate"
	"Q115-STRM/internal/models"
	"Q115-STRM/internal/synccron"
	"Q115-STRM/internal/v115open"
	"Q115-STRM/internal/websocket"
	"context"
	"database/sql"
	"embed"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
)

var Version string = "v0.0.1"
var PublishDate string = "2025-08-08"
var FANART_API_KEY = ""
var DEFAULT_TMDB_ACCESS_TOKEN = ""
var DEFAULT_TMDB_API_KEY = ""
var DEFAULT_SC_API_KEY = ""
var ENCRYPTION_KEY = ""
var Update bool = false

var AppName string = "QMediaSync"
var QMSApp *App

type App struct {
	isRelease   bool
	dbManager   *database.EmbeddedManager
	httpServer  *http.Server
	httpsServer *http.Server
	version     string
	publishDate string
}

func (app *App) Start() {
	startEmby302()
	if helpers.IsRelease {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	r.Use(controllers.Cors())
	setRouter(r)
	app.StartHttpServer(r)
	app.StartHttpsServer(r)
	if runtime.GOOS == "windows" {
		go func() {
			quit := make(chan os.Signal, 1)
			signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
			<-quit
			log.Println("收到Ctrl+C信号")
			helpers.ExitChan <- struct{}{}
		}()
		<-helpers.ExitChan
		log.Println("收到停止信号")
		app.Stop()
		close(helpers.ExitChan)
		log.Println("应用程序正常退出")
		return
	} else {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		<-quit
		log.Println("收到停止信号")
		app.Stop()
		log.Println("应用程序正常退出")
	}
}

func (app *App) Stop() {
	synccron.PauseAllNewSyncQueues()
	models.GlobalDownloadQueue.Stop()
	models.GlobalUploadQueue.Stop()
	synccron.GlobalCron.Stop()
	if app.dbManager != nil {
		app.dbManager.Stop()
	}
	helpers.CloseLogger()
	if app.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := app.httpServer.Shutdown(ctx); err != nil {
			log.Println("HTTP Server Shutdown:", err)
		}
	}
	if app.httpsServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := app.httpsServer.Shutdown(ctx); err != nil {
			log.Println("HTTPS Server Shutdown:", err)
		}
	}
}

func (app *App) StartHttpsServer(r *gin.Engine) {
	certFile := filepath.Join(helpers.RootDir, "config", "server.crt")
	keyFile := filepath.Join(helpers.RootDir, "config", "server.key")
	if !helpers.PathExists(certFile) || !helpers.PathExists(keyFile) {
		return
	}
	go func() {
		sslHost := ""
		if !helpers.IsRelease {
			sslHost = "localhost:12332"
		} else {
			sslHost = helpers.GlobalConfig.HttpsHost
		}
		app.httpsServer = &http.Server{Addr: sslHost, Handler: r}
		weberr := app.httpsServer.ListenAndServeTLS(certFile, keyFile)
		if weberr != nil {
			fmt.Println("ListenAndServe error:", weberr)
		}
	}()
}

func (app *App) StartHttpServer(r *gin.Engine) {
	host := helpers.GlobalConfig.HttpHost
	app.httpServer = &http.Server{Addr: host, Handler: r}
	go func() {
		weberr := app.httpServer.ListenAndServe()
		if weberr != nil {
			fmt.Println("ListenAndServe error:", weberr)
		}
	}()
}

func (app *App) StartDatabase(migrateMode bool) error {
	if helpers.GlobalConfig.Db.Engine == helpers.DbEngineSqlite {
		sqliteFile := filepath.Join(helpers.ConfigDir, helpers.GlobalConfig.Db.SqliteFile)
		helpers.AppLogger.Infof("sqlite数据库文件路径：%s", sqliteFile)
		db.Db = db.InitSqlite3(sqliteFile)
		models.Migrate()
		return nil
	}

	dbConfig := &database.Config{
		Mode:         helpers.GlobalConfig.Db.PostgresType,
		Host:         helpers.GlobalConfig.Db.PostgresConfig.Host,
		Port:         helpers.GlobalConfig.Db.PostgresConfig.Port,
		User:         helpers.GlobalConfig.Db.PostgresConfig.User,
		Password:     helpers.GlobalConfig.Db.PostgresConfig.Password,
		DBName:       helpers.GlobalConfig.Db.PostgresConfig.Database,
		SSLMode:      "disable",
		LogDir:       filepath.Join(helpers.ConfigDir, "postgres", "log"),
		DataDir:      filepath.Join(helpers.ConfigDir, "postgres", "data"),
		BinaryPath:   db.GetPostgresBinaryPath(helpers.DataDir),
		MaxOpenConns: helpers.GlobalConfig.Db.PostgresConfig.MaxOpenConns,
		MaxIdleConns: helpers.GlobalConfig.Db.PostgresConfig.MaxIdleConns,
	}
	if helpers.GlobalConfig.Db.PostgresConfig.SSL {
		dbConfig.SSLMode = "require"
	}
	if dbConfig.Mode == helpers.PostgresTypeEmbedded {
		app.dbManager = database.NewEmbeddedManager(dbConfig)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := app.dbManager.Start(ctx); err != nil {
			return err
		}
		db.InitPostgres(app.dbManager.GetDB())
		if migrateMode {
			helpers.AppLogger.Info("检测到使用内嵌PostgreSQL，启动迁移服务...")
			migrateServer := migrate.NewMigrateServer(app.dbManager, dbConfig)
			if err := migrateServer.Start(); err != nil {
				helpers.AppLogger.Errorf("启动迁移服务失败: %v", err)
				return err
			}
		}
	} else {
		if err := db.ConnectPostgres(dbConfig); err != nil {
			return err
		}
	}
	models.Migrate()
	return nil
}

func newApp() {
	if QMSApp != nil {
		log.Println("App已经初始化，不能再次初始化")
		return
	}
	QMSApp = &App{
		isRelease:   helpers.IsRelease,
		version:     Version,
		publishDate: PublishDate,
	}
}

func initTimeZone() {
	cstZone := time.FixedZone("CST", 8*3600)
	time.Local = cstZone
}

func checkRelease() {
	if helpers.IsRunningInDocker() {
		helpers.IsRelease = true
	}
	arg1 := strings.ToLower(os.Args[0])
	name := strings.ToLower(filepath.Base(arg1))
	helpers.IsRelease = strings.Index(name, "qmediasync") == 0 && !strings.Contains(arg1, "go-build")
}

func getRootDir() string {
	var exPath string = "/app"
	checkRelease()
	if os.Getenv("TRIM_APPDEST") != "" {
		helpers.RootDir = os.Getenv("TRIM_APPDEST")
		return helpers.RootDir
	}
	if helpers.IsRelease {
		ex, err := os.Executable()
		if err != nil {
			panic(err)
		}
		exPath = filepath.Dir(ex)
	} else {
		if runtime.GOOS == "windows" {
			exPath, _ = os.Getwd()
		} else {
			exPath = "/home/qicfan/dev/qmediasync"
		}
	}
	helpers.RootDir = exPath
	return exPath
}

func getDataAndConfigDir() {
	var appData string
	var dataDir string
	var configDir string
	needMk := false
	if runtime.GOOS == "windows" {
		appData := os.Getenv("LOCALAPPDATA")
		if appData == "" {
			appData = os.Getenv("APPDATA")
		}
		dataDir = filepath.Join(helpers.RootDir, "postgres")
		oldConfigDir := filepath.Join(appData, AppName, "config")
		configDir = filepath.Join(helpers.RootDir, "config")
		os.MkdirAll(dataDir, 0755)
		os.MkdirAll(configDir, 0755)
		helpers.DataDir = dataDir
		helpers.ConfigDir = configDir
		if helpers.PathExists(oldConfigDir) {
			helpers.MoveDir(oldConfigDir, configDir)
			os.RemoveAll(oldConfigDir)
		}
	} else {
		if os.Getenv("TRIM_PKGETC") == "" {
			appData = helpers.RootDir
			configDir = filepath.Join(appData, "config")
			dataDir = filepath.Join(appData, "postgres")
			needMk = true
			helpers.DataDir = dataDir
			helpers.ConfigDir = configDir
		} else {
			oldConfigDir := os.Getenv("TRIM_PKGETC")
			configDir = os.Getenv("TRIM_DATA_SHARE_PATHS")
			if configDir == "" {
				configDir = oldConfigDir
				needMk = false
			} else {
				configDir = filepath.Join(configDir, "config")
				needMk = true
				if helpers.PathExists(oldConfigDir) && oldConfigDir != configDir {
					if !helpers.IsDirEmpty(oldConfigDir) {
						os.MkdirAll(configDir, 0755)
						helpers.MoveDir(oldConfigDir, configDir)
						needMk = false
					}
				}
			}
			dataDir = filepath.Join(configDir, "postgres")
			helpers.DataDir = dataDir
			helpers.ConfigDir = configDir
		}
	}
	if needMk {
		os.MkdirAll(configDir, 0755)
	}
}

//go:embed emby302.yml
//go:embed assets/db_config.html
//go:embed assets/migrate.html
var embedFiles embed.FS

func init() {
	migrate.SetMigrateFiles(embedFiles)
}

func startEmby302() {
	dataRoot := helpers.ConfigDir
	data, err := embedFiles.ReadFile("emby302.yml")
	if err != nil {
		log.Fatal(err)
	}
	if err := config.ReadFromFile(data); err != nil {
		log.Fatal(err)
	}
	if models.GlobalEmbyConfig == nil || models.GlobalEmbyConfig.EmbyUrl == "" {
		helpers.AppLogger.Warnf("Emby302未配置Emby地址，跳过启动emby302服务")
		return
	}
	config.C.Emby.Host = models.GlobalEmbyConfig.EmbyUrl
	config.C.Emby.EpisodesUnplayPrior = false
	certFile := filepath.Join(dataRoot, "server.crt")
	keyFile := filepath.Join(dataRoot, "server.key")
	if helpers.PathExists(certFile) && helpers.PathExists(keyFile) {
		config.C.Ssl.Enable = true
		config.C.Ssl.SinglePort = false
		config.C.Ssl.Crt = "server.crt"
		config.C.Ssl.Key = "server.key"
	}
	config.BasePath = dataRoot
	config.C.Emby.LocalMediaRoot = "/"
	config.C.VideoPreview.Enable = true
	config.C.VideoPreview.Containers = []string{"strm"}
	go func() {
		if err := web.Listen(); err != nil {
			log.Fatal(colors.ToRed(err.Error()))
		}
	}()
}

func initLogger() {
	logPath := filepath.Join(helpers.ConfigDir, "logs")
	os.MkdirAll(logPath, 0755)
	libLogPath := filepath.Join(logPath, "libs")
	os.MkdirAll(libLogPath, 0755)
	helpers.AppLogger = helpers.NewLogger(helpers.GlobalConfig.Log.File, true, true)
	helpers.V115Log = helpers.NewLogger(helpers.GlobalConfig.Log.V115, false, true)
	helpers.OpenListLog = helpers.NewLogger(helpers.GlobalConfig.Log.OpenList, false, true)
	helpers.TMDBLog = helpers.NewLogger(helpers.GlobalConfig.Log.TMDB, false, true)
	helpers.BaiduPanLog = helpers.NewLogger(helpers.GlobalConfig.Log.BaiduPan, false, true)
}

func initOthers() {
	helpers.InitEventBus()
	models.LoadSettings()
	github.InitManager(models.SettingsGlobal.HttpProxy)
	helpers.AppLogger.Infof("已加载配置，准备初始化115请求队列，线程数: %d", models.SettingsGlobal.FileDetailThreads)
	qps := models.SettingsGlobal.FileDetailThreads
	if qps <= 0 {
		qps = 2
	}
	v115open.SetGlobalExecutorConfig(qps, qps*60, qps*3600)
	models.LoadScrapeSettings()
	models.InitDQ()
	models.InitUQ()

	// ===== 清空上次运行残留的 AV 刮削临时目录（新增） =====
	os.RemoveAll(filepath.Join(helpers.ConfigDir, "tmp", "avscrape"))
	// ====================================================

	models.InitNotificationManager()
	controllers.StartListenTelegramBot()
	models.GetEmbyConfig()
	helpers.SubscribeSync(helpers.V115TokenInValidEvent, models.HandleV115TokenInvalid)
	helpers.SubscribeSync(helpers.SaveOpenListTokenEvent, models.HandleOpenListTokenSaveSync)
	models.FailAllRunningSyncTasks()
	synccron.RefreshOAuthAccessToken()

	v115open.SetGlobalExecutorStatSaver(func(requestTime int64, url, method string, duration int64, isThrottled bool) {
		stat := &models.RequestStat{
			RequestTime: requestTime,
			URL:         url,
			Method:      method,
			Duration:    duration,
			IsThrottled: isThrottled,
			AccountID:   0,
		}
		if err := models.CreateRequestStat(stat); err != nil {
			helpers.V115Log.Errorf("写入请求统计失败: %v", err)
		}
	})

	// 启动同步任务队列管理器
	synccron.InitNewSyncQueueManager()

	// ===== 注册 AV 扫描回调 =====
	synccron.AVScanHandler = func(pathID uint) error {
		scanner := avscrape.NewScanner(db.Db)
		return scanner.Scan(pathID)
	}
	// ============================

	wsHub := websocket.NewEventHub()
	websocket.GlobalEventHub = wsHub
	go wsHub.Run()
	synccron.InitCron()
	synccron.InitSyncCron()
	synccron.InitScrapeCron()
	synccron.InitTokenCron()
	models.InitBackupService()
	models.ResetScrapePathStatus()
	models.UpdateScrapeMediaStatus(models.ScrapeMediaStatusScraping, models.ScrapeMediaStatusScanned, 0)
	models.UpdateScrapeMediaStatus(models.ScrapeMediaStatusRenaming, models.ScrapeMediaStatusScraped, 0)
	models.UpdateUploadingToPending()
	models.UpdateDownloadingToPending()
	helpers.Subscribe(helpers.BackupCronEevent, func(event helpers.Event) {
		backup.Backup("定时", "定时备份")
	})
	helpers.Subscribe(helpers.StrmSyncCompleteEvent, func(event helpers.Event) {
		scrapePathIds := event.Data.([]uint)
		for _, scrapePathId := range scrapePathIds {
			scrapePath := models.GetScrapePathByID(scrapePathId)
			if scrapePath == nil {
				helpers.AppLogger.Errorf("获取刮削目录失败: %v", scrapePathId)
				continue
			}
			taskObj := &synccron.NewSyncTask{
				ID:           scrapePathId,
				SourcePath:   "",
				SourcePathId: "",
				TargetPath:   "",
				AccountId:    scrapePath.AccountId,
				SourceType:   scrapePath.SourceType,
				IsFile:       false,
				TaskType:     synccron.SyncTaskTypeScrape,
			}
			if err := synccron.AddNewSyncTask(taskObj); err != nil {
				helpers.AppLogger.Errorf("添加刮削任务失败: %v", err)
			} else {
				helpers.AppLogger.Infof("创建刮削任务成功并已添加到执行队列，刮削目录ID: %d，", scrapePathId)
			}
		}
	})
}

func setRouter(r *gin.Engine) {
	webStatisPath := filepath.Join(helpers.RootDir, "web_statics")
	r.LoadHTMLFiles(filepath.Join(webStatisPath, "index.html"))
	r.StaticFile("/favicon.ico", filepath.Join(webStatisPath, "favicon.ico"))
	r.StaticFS("/assets", http.Dir(filepath.Join(webStatisPath, "assets")))
	r.GET("/", func(c *gin.Context) {
		c.HTML(200, "index.html", gin.H{})
	})
	r.POST("/emby/webhook", controllers.Webhook)
	r.POST("/api/login", controllers.LoginAction)
	r.GET("/115/url/*filename", controllers.Get115UrlByPickCode)
	r.GET("/115/newurl", controllers.Get115UrlByPickCode)
	r.GET("/baidupan/url/*filename", controllers.GetBaiduPanUrlByPickCode)
	r.GET("/openlist/url", controllers.GetOpenListFileUrl)
	r.GET("/proxy-115", controllers.Proxy115)
	r.GET("/api/scrape/tmp-image", controllers.ScrapeTmpImage)
	r.GET("/api/scrape/records/export", controllers.ExportScrapeRecords)
	r.GET("/api/logs/ws", controllers.LogWebSocket)
	r.GET("/api/events/ws", controllers.EventWebSocket)
	r.GET("/api/logs/old", controllers.GetOldLogs)
	r.GET("/api/logs/download", controllers.DownloadLogFile)
	r.POST("/api/update-fn-access-path", controllers.UpdateFNPath)
	r.GET("/api/path/is-fn-os", controllers.IsFnOS)

	// ===== AV 刮削模块注册 =====
	if err := avscrape.Register(r, db.Db); err != nil {
		log.Fatal("AV 刮削模块注册失败:", err)
	}
	// ==========================

	api := r.Group("/api")
	api.Use(controllers.JWTAuthMiddleware())
	{
		api.GET("/version", func(c *gin.Context) {
			c.JSON(http.StatusOK, map[string]interface{}{
				"version":   Version,
				"date":      PublishDate,
				"isWindows": runtime.GOOS == "windows",
				"isRelease": helpers.IsRelease,
			})
		})
		api.POST("/database/delete-all-table", controllers.DeleteAllTabble)
		api.GET("/announce", controllers.GetAnnounce)
		api.POST("/database/repair", controllers.RepairDB)
		api.POST("/auth/115-qrcode-open", controllers.GetLoginQrCodeOpen)
		api.POST("/auth/115-qrcode-status", controllers.GetQrCodeStatus)
		api.GET("/115/status", controllers.Get115Status)
		api.GET("/115/oauth-url", controllers.GetOAuthUrl)
		api.POST("115/oauth-confirm", controllers.ConfirmOAuthCode)
		api.GET("/115/queue/stats", controllers.GetQueueStats)
		api.POST("/115/queue/rate-limit", controllers.SetQueueRateLimit)
		api.GET("/115/stats/daily", controllers.GetRequestStatsByDay)
		api.GET("/115/stats/hourly", controllers.GetRequestStatsByHour)
		api.POST("/115/stats/clean", controllers.CleanOldRequestStats)
		api.GET("/baidupan/oauth-url", controllers.GetBaiDuPanOAuthUrl)
		api.POST("/baidupan/oauth-confirm", controllers.ConfirmBaiDuPanOAuthCode)
		api.GET("/baidupan/status", controllers.GetBaiDuPanStatus)
		api.GET("/update/last", controllers.GetLastRelease)
		api.POST("/update/to-version", controllers.UpdateToVersion)
		api.GET("/update/progress", controllers.UpdateProgress)
		api.POST("/update/cancel", controllers.CancelUpdate)
		api.GET("/user/info", controllers.GetUserInfo)
		api.GET("/path/list", controllers.GetPathList)
		api.POST("/path/create", controllers.CreateDir)
		api.DELETE("/path", controllers.DeleteDir)
		api.GET("/path/files", controllers.GetNetFileList)
		api.POST("/user/change", controllers.ChangePassword)
		api.POST("/setting/http-proxy", controllers.UpdateHttpProxy)
		api.GET("/setting/http-proxy", controllers.GetHttpProxy)
		api.POST("/setting/test-http-proxy", controllers.TestHttpProxy)
		api.GET("/setting/notification/channels", controllers.GetNotificationChannels)
		api.POST("/setting/notification/channels/telegram", controllers.CreateTelegramChannel)
		api.GET("/setting/notification/channels/telegram/:id", controllers.GetTelegramChannel)
		api.PUT("/setting/notification/channels/telegram", controllers.UpdateTelegramChannel)
		api.POST("/setting/notification/channels/meow", controllers.CreateMeoWChannel)
		api.GET("/setting/notification/channels/meow/:id", controllers.GetMeoWChannel)
		api.PUT("/setting/notification/channels/meow", controllers.UpdateMeoWChannel)
		api.POST("/setting/notification/channels/bark", controllers.CreateBarkChannel)
		api.GET("/setting/notification/channels/bark/:id", controllers.GetBarkChannel)
		api.PUT("/setting/notification/channels/bark", controllers.UpdateBarkChannel)
		api.POST("/setting/notification/channels/serverchan", controllers.CreateServerChanChannel)
		api.GET("/setting/notification/channels/serverchan/:id", controllers.GetServerChanChannel)
		api.PUT("/setting/notification/channels/serverchan", controllers.UpdateServerChanChannel)
		api.POST("/setting/notification/channels/webhook", controllers.CreateCustomWebhookChannel)
		api.GET("/setting/notification/channels/webhook/:id", controllers.GetCustomWebhookChannel)
		api.PUT("/setting/notification/channels/webhook", controllers.UpdateCustomWebhookChannel)
		api.POST("/setting/notification/channels/status", controllers.UpdateChannelStatus)
		api.DELETE("/setting/notification/channels/:id", controllers.DeleteChannel)
		api.GET("/setting/notification/rules", controllers.GetNotificationRules)
		api.PUT("/setting/notification/rules", controllers.UpdateNotificationRule)
		api.POST("/setting/notification/channels/test", controllers.TestChannelConnection)
		api.GET("/setting/strm-config", controllers.GetStrmConfig)
		api.POST("/setting/strm-config", controllers.UpdateStrmConfig)
		api.GET("/setting/cron", controllers.GetCronNextTime)
		api.POST("/cron/validate", controllers.ValidateCron)
		api.POST("/setting/emby/parse", controllers.ParseEmby)
		api.GET("/setting/emby-config", controllers.GetEmbyConfig)
		api.POST("/setting/emby-config", controllers.UpdateEmbyConfig)
		api.POST("/setting/threads", controllers.UpdateThreads)
		api.GET("/setting/threads", controllers.GetThreads)
		api.POST("/emby/sync/start", controllers.StartEmbySync)
		api.GET("/emby/sync/status", controllers.GetEmbySyncStatus)
		api.GET("/emby/libraries", controllers.GetEmbyLibraries)
		api.POST("/sync/start", controllers.StartSync)
		api.GET("/sync/records", controllers.GetSyncRecords)
		api.GET("/sync/task", controllers.GetSyncTask)
		api.GET("/sync/path-list", controllers.GetSyncPathList)
		api.POST("/sync/path-add", controllers.AddSyncPath)
		api.POST("/sync/path-update", controllers.UpdateSyncPath)
		api.POST("/sync/path-delete", controllers.DeleteSyncPath)
		api.POST("/sync/path/stop", controllers.StopSyncByPath)
		api.POST("/sync/path/start", controllers.StartSyncByPath)
		api.POST("/sync/path/full-start", controllers.FullStart115Sync)
		api.POST("/sync/delete-records", controllers.DelSyncRecords)
		api.POST("/sync/path/toggle-cron", controllers.ToggleSyncByPath)
		api.GET("/sync/path/:id", controllers.GetSyncPathById)
		api.GET("/sync/path/:id/scrape-paths", controllers.GetRelScrapePath)
		api.POST("/sync/path/scrape-paths", controllers.SaveRelScrapePath)
		api.POST("/sync/manual", controllers.ManualSync)
		api.GET("/account/list", controllers.GetAccountList)
		api.POST("/account/add", controllers.CreateTmpAccount)
		api.POST("/account/delete", controllers.DeleteAccount)
		api.POST("/account/openlist", controllers.CreateOpenListAccount)
		api.POST("/api-keys", controllers.CreateAPIKey)
		api.GET("/api-keys", controllers.ListAPIKeys)
		api.PUT("/api-keys/:id/status", controllers.UpdateAPIKeyStatus)
		api.DELETE("/api-keys/:id", controllers.DeleteAPIKey)
		api.GET("/scrape/movie-genre", controllers.GetMovieGenre)
		api.GET("/scrape/tvshow-genre", controllers.GetTvshowGenre)
		api.GET("/scrape/language", controllers.GetLanguage)
		api.GET("/scrape/countries", controllers.GetCountries)
		api.GET("/scrape/tmdb", controllers.GetTmdbSettings)
		api.POST("/scrape/tmdb", controllers.SaveTmdbSettings)
		api.POST("/scrape/tmdb-test", controllers.TestTmdbSettings)
		api.GET("/scrape/ai-settings", controllers.GetAiSettings)
		api.POST("/scrape/ai-settings", controllers.SaveAiSettings)
		api.POST("/scrape/ai-test", controllers.TestAiSettings)
		api.GET("/scrape/movie-categories", controllers.GetMovieCategories)
		api.GET("/scrape/tvshow-categories", controllers.GetTvshowCategories)
		api.POST("/scrape/movie-categories", controllers.SaveMovieCategory)
		api.POST("/scrape/tvshow-categories", controllers.SaveTvshowCategory)
		api.DELETE("/scrape/movie-categories/:id", controllers.DeleteMovieCategory)
		api.DELETE("/scrape/tvshow-categories/:id", controllers.DeleteTvshowCategory)
		api.GET("/scrape/pathes", controllers.GetScrapePathes)
		api.POST("/scrape/pathes", controllers.SaveScrapePath)
		api.DELETE("/scrape/pathes/:id", controllers.DeleteScrapePath)
		api.GET("/scrape/pathes/:id", controllers.GetScrapePath)
		api.POST("/scrape/pathes/start", controllers.ScanScrapePath)
		api.POST("/scrape/pathes/stop", controllers.StopScrape)
		api.POST("/scrape/pathes/toggle-cron", controllers.ToggleScrapePathCron)
		api.GET("/scrape/records", controllers.GetScrapeRecords)
		api.POST("/scrape/re-scrape", controllers.ReScrape)
		api.POST("/scrape/clear-failed", controllers.ClearFailedScrapeRecords)
		api.POST("/scrape/truncate-all", controllers.TruncateAllScrapeRecords)
		api.DELETE("/scrape/records", controllers.DeleteScrapeMediaFile)
		api.POST("/scrape/finish", controllers.FinishScrapeMediaFile)
		api.POST("/scrape/rename-failed", controllers.RenameFailedScrapeMediaFile)
		api.POST("/scrape/sync-pathes", controllers.SaveScrapeStrmPath)
		api.GET("/scrape/sync-pathes", controllers.GetScrapeStrmPaths)
		api.GET("/scrape/tmdb-search", controllers.TmdbSearch)
		api.GET("/upload/queue", controllers.UploadList)
		api.POST("/upload/queue/clear-pending", controllers.ClearPendingUploadTasks)
		api.POST("/upload/queue/start", controllers.StartUploadQueue)
		api.POST("/upload/queue/stop", controllers.StopUploadQueue)
		api.GET("/upload/queue/status", controllers.UploadQueueStatus)
		api.POST("/upload/queue/clear-success-failed", controllers.ClearUploadSuccessAndFailedTasks)
		api.POST("/upload/queue/retry-failed", controllers.RetryFailedUploadTasks)
		api.GET("/download/queue", controllers.DownloadList)
		api.POST("/download/queue/clear-pending", controllers.ClearPendingDownloadTasks)
		api.POST("/download/queue/start", controllers.StartDownloadQueue)
		api.POST("/download/queue/stop", controllers.StopDownloadQueue)
		api.GET("/download/queue/status", controllers.DownloadQueueStatus)
		api.POST("/download/queue/clear-success-failed", controllers.ClearDownloadSuccessAndFailedTasks)
		api.GET("/backup/list", controllers.GetBackupList)
		api.GET("/backup/records/:id", controllers.GetBackupRecord)
		api.POST("/backup/create", controllers.CreateBackup)
		api.DELETE("/backup/records/:id", controllers.DeleteBackup)
		api.POST("/backup/restore", controllers.RestoreFromBackup)
		api.POST("/backup/upload-restore", controllers.UploadAndRestore)
		api.GET("/backup/download/:id", controllers.DownloadBackup)
		api.GET("/backup/config", controllers.GetBackupConfig)
		api.PUT("/backup/config", controllers.UpdateBackupConfig)
		api.GET("/backup/status", controllers.GetBackupStatus)
	}
}

func initEnv() bool {
	log.Printf("当前版本号:%s, 发布日期:%s\n", Version, PublishDate)
	helpers.Version = Version
	helpers.ReleaseDate = PublishDate
	helpers.LoadEnvFromFile(filepath.Join(helpers.RootDir, "config", ".env"))
	if DEFAULT_SC_API_KEY != "" {
		helpers.DEFAULT_SC_API_KEY = DEFAULT_SC_API_KEY
	} else {
		helpers.DEFAULT_SC_API_KEY = os.Getenv("DEFAULT_SC_API_KEY")
	}
	if DEFAULT_TMDB_API_KEY != "" {
		helpers.DEFAULT_TMDB_API_KEY = DEFAULT_TMDB_API_KEY
	} else {
		helpers.DEFAULT_TMDB_API_KEY = os.Getenv("DEFAULT_TMDB_API_KEY")
	}
	if DEFAULT_TMDB_ACCESS_TOKEN != "" {
		helpers.DEFAULT_TMDB_ACCESS_TOKEN = DEFAULT_TMDB_ACCESS_TOKEN
	} else {
		helpers.DEFAULT_TMDB_ACCESS_TOKEN = os.Getenv("DEFAULT_TMDB_ACCESS_TOKEN")
	}
	if FANART_API_KEY != "" {
		helpers.FANART_API_KEY = FANART_API_KEY
	} else {
		helpers.FANART_API_KEY = os.Getenv("FANART_API_KEY")
	}
	if ENCRYPTION_KEY != "" {
		helpers.ENCRYPTION_KEY = ENCRYPTION_KEY
	} else {
		helpers.ENCRYPTION_KEY = os.Getenv("ENCRYPTION_KEY")
	}
	initTimeZone()
	getDataAndConfigDir()
	log.Printf("当前工作目录:%s\n", helpers.RootDir)
	log.Printf("当前数据目录：%s\n", helpers.DataDir)
	log.Printf("当前配置文件目录: %s\n", helpers.ConfigDir)
	ipv4, _ := helpers.GetLocalIP()
	log.Printf("本机IPv4地址是 <%s>\n", ipv4)
	configPath := filepath.Join(helpers.ConfigDir, "config.yml")
	helpers.IsFirstRun = !helpers.PathExists(configPath)
	if helpers.IsFirstRun {
		oldPostgresDataDir := filepath.Join(helpers.ConfigDir, "postgres")
		if helpers.PathExists(oldPostgresDataDir) {
			if err := helpers.MakeOldConfig(); err != nil {
				log.Printf("生成新的配置文件失败: %v", err)
				return false
			}
			helpers.IsFirstRun = false
		} else {
			StartConfigWebServer()
			return false
		}
	}
	err := helpers.InitConfig()
	if err != nil {
		log.Printf("初始化配置文件失败: %v", err)
		return false
	}
	initLogger()
	newApp()
	helpers.AppLogger.Infof("当前版本号:%s, 发布日期:%s\n", Version, PublishDate)
	if migrate.ShouldRestore() {
		helpers.AppLogger.Info("检测到迁移备份文件存在且使用外部PostgreSQL，开始自动恢复...")
		if err := QMSApp.StartDatabase(false); err != nil {
			log.Println("数据库启动失败:", err)
			return false
		}
		backupPath := migrate.GetMigrateBackupPath()
		if err := performMigrateRestore(backupPath); err != nil {
			helpers.AppLogger.Errorf("恢复数据失败: %v", err)
			return false
		}
		os.Remove(backupPath)
		helpers.AppLogger.Info("数据恢复完成，已删除迁移备份文件")
	} else {
		needMigrate := migrate.ShouldMigrate()
		if err := QMSApp.StartDatabase(needMigrate); err != nil {
			helpers.AppLogger.Errorf("数据库启动失败: %v", err)
			return false
		}
		if needMigrate {
			return false
		}
	}
	db.InitCache()
	initOthers()
	return true
}

func parseParams() {
	var update string
	flag.StringVar(&helpers.Guid, "guid", "", "GUID 参数")
	flag.BoolVar(&helpers.IsFnOS, "fnos", false, "是否是飞牛环境")
	flag.StringVar(&update, "update", "", "更新参数")
	flag.Parse()
	if helpers.IsFnOS {
		log.Printf("当前环境为飞牛环境\n")
	}
	if helpers.Guid == "" || helpers.Guid == "0" {
		guidEnv := os.Getenv("GUID")
		if guidEnv != "" {
			helpers.Guid = guidEnv
		} else {
			helpers.Guid = "0"
		}
	}
	if update != "" && runtime.GOOS == "windows" {
		Update = true
	}
}

// @title QMediaSync API
// @version 1.0
// @description 媒体同步和刮削系统API
// @host localhost:8115
// @BasePath /
// @securityDefinitions.apikey JwtAuth
// @in header
// @name Authorization
// @securityDefinitions.apikey ApiKeyAuth
// @in query
// @name api_key
func main() {
	parseParams()
	getRootDir()
	if Update {
		runUpdateProcess()
		return
	}
	if !initEnv() {
		panic("初始化环境失败")
	}
	if runtime.GOOS == "windows" {
		if helpers.IsRelease {
			go QMSApp.Start()
			helpers.StartApp(func() {
				QMSApp.Stop()
			})
		} else {
			QMSApp.Start()
		}
	} else {
		QMSApp.Start()
	}
}

func runUpdateProcess() {
	if len(os.Args) < 3 {
		fmt.Println("更新参数不足")
		return
	}
	updateDir := os.Args[2]
	fmt.Println("开始更新流程...")
	parentPID := os.Getppid()
	fmt.Printf("等待父进程退出 (PID: %d)...\n", parentPID)
	if err := waitForProcessExit(parentPID); err != nil {
		fmt.Printf("等待父进程退出失败: %v\n", err)
	}
	fmt.Println("父进程已退出，开始更新...")
	backupDir := filepath.Join(helpers.RootDir, "old")
	if helpers.PathExists(backupDir) {
		fmt.Println("删除旧的备份目录...")
		os.RemoveAll(backupDir)
	}
	os.MkdirAll(backupDir, 0777)
	appName := "QMediaSync.exe"
	appPath := filepath.Join(helpers.RootDir, appName)
	newAppPath := filepath.Join(updateDir, appName)
	if helpers.PathExists(newAppPath) {
		fmt.Printf("更新 %s...\n", appName)
		oldAppPath := appPath + ".old.exe"
		if helpers.PathExists(oldAppPath) {
			if err := os.Remove(oldAppPath); err != nil {
				fmt.Printf("删除旧 %s 失败: %v\n", appName, err)
				os.Exit(1)
			}
		}
		if err := os.Rename(appPath, oldAppPath); err != nil {
			fmt.Printf("重命名旧 %s 失败: %v\n", appName, err)
			os.Exit(1)
		}
		if err := helpers.CopyFile(newAppPath, appPath); err != nil {
			fmt.Printf("更新主程序失败: %v\n", err)
		}
	} else {
		fmt.Printf("更新目录中未找到 %s\n", appName)
	}
	replaceDir(filepath.Join(updateDir, "web_statics"), filepath.Join(helpers.RootDir, "web_statics"), backupDir)
	replaceDir(filepath.Join(updateDir, "scripts"), filepath.Join(helpers.RootDir, "scripts"), backupDir)
	tempExePath := newAppPath + ".temp.exe"
	if helpers.PathExists(tempExePath) {
		os.Remove(tempExePath)
	}
	fmt.Println("更新完成!")
	fmt.Println("启动新版本...")
	if !helpers.StartNewProcess(appPath, "") {
		fmt.Printf("启动新版本失败\n")
	}
}

func waitForProcessExit(pid int) error {
	maxWait := 30 * time.Second
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		alive, err := helpers.IsProcessAlive(pid)
		if err != nil {
			return nil
		}
		if !alive {
			fmt.Printf("父进程已退出，等待资源释放...\n")
			time.Sleep(2 * time.Second)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil
}

func replaceDir(srcDir, dstDir, backupDir string) {
	if !helpers.PathExists(srcDir) {
		return
	}
	dirName := filepath.Base(dstDir)
	backupPath := filepath.Join(backupDir, dirName)
	if helpers.PathExists(dstDir) {
		fmt.Printf("备份 %s 目录...\n", dirName)
		os.RemoveAll(backupPath)
		if err := helpers.CopyDir(dstDir, backupPath); err != nil {
			fmt.Printf("备份 %s 目录失败: %v\n", dirName, err)
		}
		fmt.Printf("删除旧 %s 目录...\n", dirName)
		os.RemoveAll(dstDir)
	}
	fmt.Printf("更新 %s 目录...\n", dirName)
	if err := helpers.CopyDir(srcDir, dstDir); err != nil {
		fmt.Printf("更新 %s 目录失败: %v\n", dirName, err)
	}
}

func isInRestrictedDirectory() (bool, string) {
	if runtime.GOOS != "windows" {
		return false, ""
	}
	exePath, err := os.Executable()
	if err != nil {
		return false, ""
	}
	exeDir := filepath.Dir(exePath)
	driveLetter := strings.ToUpper(string(exeDir[0]))
	if driveLetter == "C" {
		return true, "应用程序位于 C 盘，建议将应用程序移动到其他盘符（如 D 盘、E 盘等）以避免权限问题"
	}
	restrictedPaths := []string{"Program Files", "Program Files (x86)", "ProgramData", "Windows"}
	for _, restrictedPath := range restrictedPaths {
		if strings.Contains(exeDir, restrictedPath) {
			return true, fmt.Sprintf("应用程序位于受限目录 '%s' 中，建议将应用程序移动到普通用户目录或其他非系统目录", restrictedPath)
		}
	}
	return false, ""
}

func performMigrateRestore(backupPath string) error {
	helpers.AppLogger.Infof("开始从迁移备份恢复: %s", backupPath)
	if _, err := os.Stat(backupPath); os.IsNotExist(err) {
		return fmt.Errorf("备份文件不存在: %s", backupPath)
	}
	if err := backup.Restore(backupPath); err != nil {
		return fmt.Errorf("恢复失败: %v", err)
	}
	helpers.AppLogger.Info("迁移恢复完成")
	return nil
}

func StartConfigWebServer() {
	if helpers.IsRelease {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.Default()
	data, err := embedFiles.ReadFile("assets/db_config.html")
	if err != nil {
		log.Fatal(err)
	}
	tmpl := template.Must(template.New("db_config.html").Parse(string(data)))
	r.SetHTMLTemplate(tmpl)
	r.GET("/", func(c *gin.Context) {
		isRestricted, warningMsg := isInRestrictedDirectory()
		c.HTML(200, "db_config.html", gin.H{
			"title":        "数据库配置",
			"isRestricted": isRestricted,
			"warningMsg":   warningMsg,
			"isWindows":    runtime.GOOS == "windows",
		})
	})
	r.POST("/api/config/test-db", func(c *gin.Context) {
		var req struct {
			Host     string `json:"host"`
			Port     int    `json:"port"`
			User     string `json:"user"`
			Password string `json:"password"`
			Database string `json:"database"`
			SSL      bool   `json:"ssl"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"success": false, "error": err.Error()})
			return
		}
		sslMode := "disable"
		if req.SSL {
			sslMode = "require"
		}
		connStr := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=postgres sslmode=%s",
			req.Host, req.Port, req.User, req.Password, sslMode)
		sqlDB, err := sql.Open("postgres", connStr)
		if err != nil {
			c.JSON(200, gin.H{"success": false, "error": "连接失败: " + err.Error()})
			return
		}
		defer sqlDB.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := sqlDB.PingContext(ctx); err != nil {
			c.JSON(200, gin.H{"success": false, "error": "连接失败: " + err.Error()})
			return
		}
		var dbExists bool
		err = sqlDB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", req.Database).Scan(&dbExists)
		if err != nil {
			c.JSON(200, gin.H{"success": false, "error": "检查数据库失败: " + err.Error()})
			return
		}
		if !dbExists {
			c.JSON(200, gin.H{"success": true, "message": "数据库连接成功", "dbExists": false})
			return
		}
		connStrDb := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
			req.Host, req.Port, req.User, req.Password, req.Database, sslMode)
		sqlDBDb, err := sql.Open("postgres", connStrDb)
		if err != nil {
			c.JSON(200, gin.H{"success": true, "message": "数据库连接成功", "dbExists": true, "hasOtherTables": false})
			return
		}
		defer sqlDBDb.Close()
		var tableCount int
		err = sqlDBDb.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name NOT LIKE 'gorr_%'").Scan(&tableCount)
		if err != nil {
			c.JSON(200, gin.H{"success": true, "message": "数据库连接成功", "dbExists": true, "hasOtherTables": false})
			return
		}
		c.JSON(200, gin.H{"success": true, "message": "数据库连接成功", "dbExists": true, "hasOtherTables": tableCount > 0})
	})
	r.POST("/api/config/save", func(c *gin.Context) {
		var req struct {
			Engine        string `json:"engine"`
			PostgresType  string `json:"postgresType"`
			Host          string `json:"host"`
			Port          int    `json:"port"`
			User          string `json:"user"`
			Password      string `json:"password"`
			Database      string `json:"database"`
			SSL           bool   `json:"ssl"`
			AdminUsername string `json:"adminUsername"`
			AdminPassword string `json:"adminPassword"`
			DropDatabase  bool   `json:"dropDatabase"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		yamlConfig := helpers.MakeDefaultConfig()
		yamlConfig.AdminUsername = req.AdminUsername
		yamlConfig.AdminPassword = req.AdminPassword
		if req.Engine == string(helpers.DbEnginePostgres) {
			yamlConfig.Db.PostgresType = helpers.PostgresType(req.PostgresType)
			if req.PostgresType == string(helpers.PostgresTypeExternal) {
				if req.DropDatabase {
					sslMode := "disable"
					if req.SSL {
						sslMode = "require"
					}
					connStr := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=postgres sslmode=%s",
						req.Host, req.Port, req.User, req.Password, sslMode)
					sqlDB, err := sql.Open("postgres", connStr)
					if err == nil {
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						sqlDB.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s", req.Database))
						sqlDB.Close()
					}
				}
				yamlConfig.Db.PostgresConfig = helpers.PostgresConfig{
					Host:         req.Host,
					Port:         req.Port,
					User:         req.User,
					Password:     req.Password,
					Database:     req.Database,
					SSL:          req.SSL,
					MaxOpenConns: 25,
					MaxIdleConns: 25,
				}
			} else {
				yamlConfig.Db.PostgresConfig = helpers.PostgresConfig{
					Host:         "localhost",
					Port:         5432,
					User:         "qms",
					Password:     "qms123456",
					Database:     "qms",
					MaxOpenConns: 25,
					MaxIdleConns: 25,
				}
			}
		} else {
			yamlConfig.Db.Engine = helpers.DbEngineSqlite
		}
		if err := helpers.SaveConfig(yamlConfig); err != nil {
			c.JSON(500, gin.H{"error": "保存配置失败: " + err.Error()})
			return
		}
		c.JSON(200, gin.H{"success": true, "message": "配置已保存，配置服务已退出，请重启软件或者容器"})
		go func() {
			time.Sleep(1 * time.Second)
			os.Exit(0)
		}()
	})
	fmt.Printf("配置服务已启动，请在浏览器中访问: http://ip:12333\n")
	go func() {
		time.Sleep(2 * time.Second)
		helpers.OpenBrowser("http://127.0.0.1:12333")
	}()
	if err := r.Run(":12333"); err != nil {
		log.Fatalf("启动配置服务失败: %v", err)
	}
}