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
	// 启动外网302服务
	startEmby302()
	if helpers.IsRelease {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	r.Use(controllers.Cors())

	// ===== AV 刮削模块注册 =====
	if err := avscrape.Register(r, db.Db); err != nil {
		log.Fatal("AV 刮削模块注册失败:", err)
	}
	// ==========================

	app.StartHttpServer(r)
	app.StartHttpsServer(r)
	if runtime.GOOS == "windows" {
		// 监听Ctrl+C信号
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
		// 停止应用
		app.Stop()
		log.Println("应用程序正常退出")
	}
}

func (app *App) Stop() {
	// 关闭同步任务执行队列
	synccron.PauseAllNewSyncQueues()
	// 关闭上传下载队列
	models.GlobalDownloadQueue.Stop()
	models.GlobalUploadQueue.Stop()
	// 关闭定时任务（包含备份定时任务）
	synccron.GlobalCron.Stop()
	// 关闭数据库
	if app.dbManager != nil {
		app.dbManager.Stop()
	}
	helpers.CloseLogger() // 关闭日志
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
		// 在12332端口上启动https服务
		sslHost := ""
		// 启动web server
		if !helpers.IsRelease {
			sslHost = "localhost:12332"
		} else {
			sslHost = helpers.GlobalConfig.HttpsHost
		}
		app.httpsServer = &http.Server{
			Addr:    sslHost,
			Handler: r,
		}
		// 没有证书则回退到普通 HTTP
		weberr := app.httpsServer.ListenAndServeTLS(certFile, keyFile)
		if weberr != nil {
			fmt.Println("ListenAndServe error:", weberr)
		}
	}()
}

func (app *App) StartHttpServer(r *gin.Engine) {
	host := helpers.GlobalConfig.HttpHost
	if host == "" {
		host = ":12333"
	}
	// 同时在12333端口上启动http服务
	app.httpServer = &http.Server{
		Addr:    host,
		Handler: r,
	}
	go func() {
		weberr := app.httpServer.ListenAndServe()
		if weberr != nil {
			fmt.Println("ListenAndServe error:", weberr)
		}
	}()
}

func (app *App) StartDatabase(migrateMode bool) error {
	// 根据配置启动数据库连接
	if helpers.GlobalConfig.Db.Engine == helpers.DbEngineSqlite {
		// 如果是sqlite，直接初始化sqlite连接
		sqliteFile := filepath.Join(helpers.ConfigDir, helpers.GlobalConfig.Db.SqliteFile)
		helpers.AppLogger.Infof("sqlite数据库文件路径：%s", sqliteFile)
		db.Db = db.InitSqlite3(sqliteFile)
		models.Migrate()
		// ===== AV 表迁移 =====
		if err := avscrape.AutoMigrate(db.Db); err != nil {
			return err
		}
		// =====================
		return nil
	}

	// 初始化数据库配置
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
		// 如果使用内置数据库，则需要启动和初始化数据库
		app.dbManager = database.NewEmbeddedManager(dbConfig)
		// 启动数据库
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		if err := app.dbManager.Start(ctx); err != nil {
			return err
		}
		db.InitPostgres(app.dbManager.GetDB())

		// 如果是迁移模式，启动迁移服务
		if migrateMode {
			helpers.AppLogger.Info("检测到使用内嵌PostgreSQL，启动迁移服务...")
			migrateServer := migrate.NewMigrateServer(app.dbManager, dbConfig)
			if err := migrateServer.Start(); err != nil {
				helpers.AppLogger.Errorf("启动迁移服务失败: %v", err)
				return err
			}
		}
	} else {
		// 初始化PostgreSQL数据库连接
		if err := db.ConnectPostgres(dbConfig); err != nil {
			return err
		}
	}
	models.Migrate()
	// ===== AV 表迁移 =====
	if err := avscrape.AutoMigrate(db.Db); err != nil {
		return err
	}
	// =====================
	return nil
}

func newApp() {
	if QMSApp != nil {
		log.Println("App已经初始化，不能再次初始化")
		return
	}
	// 初始化APP
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
	var exPath string = "/app" // 默认使用docker的路径
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
	helpers.RootDir = exPath // 获取当前工作目录
	return exPath
}

// 获取用户数据目录
func getDataAndConfigDir() {
	var appData string
	var dataDir string
	var configDir string
	needMk := false
	if runtime.GOOS == "windows" {
		// 使用AppData目录，用户有完全控制权限
		appData := os.Getenv("LOCALAPPDATA")
		if appData == "" {
			appData = os.Getenv("APPDATA")
		}
		dataDir = filepath.Join(helpers.RootDir, "postgres")      // 数据库目录
		oldConfigDir := filepath.Join(appData, AppName, "config") // 配置目录
		configDir = filepath.Join(helpers.RootDir, "config")      // 配置目录
		err := os.MkdirAll(dataDir, 0755)
		if err != nil {
			fmt.Printf("创建数据目录失败: %v\n", err)
			panic("创建数据目录失败")
		}
		err = os.MkdirAll(configDir, 0755)
		if err != nil {
			fmt.Printf("创建配置目录失败: %v\n", err)
			panic("创建配置目录失败")
		}
		helpers.DataDir = dataDir
		helpers.ConfigDir = configDir
		if helpers.PathExists(oldConfigDir) {
			// 迁移旧配置
			err := helpers.MoveDir(oldConfigDir, configDir)
			if err != nil {
				fmt.Printf("迁移旧配置目录失败: %v\n", err)
				panic("迁移旧配置目录失败")
			}
			// 删除旧目录
			err = os.RemoveAll(oldConfigDir)
			if err != nil {
				fmt.Printf("删除旧配置目录失败: %v\n", err)
				panic("删除旧配置目录失败")
			}
		}
	} else {
		if os.Getenv("TRIM_PKGETC") == "" {
			appData = helpers.RootDir
			configDir = filepath.Join(appData, "config") // 配置目录
			dataDir = filepath.Join(appData, "postgres") // 数据库目录
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
				// 检查是否需要迁移文件
				// oldConfigDir必须存在且不为空
				if helpers.PathExists(oldConfigDir) && oldConfigDir != configDir {
					// 检查oldConfigDir是否为空目录
					if !helpers.IsDirEmpty(oldConfigDir) {
						err := os.MkdirAll(configDir, 0755)
						if err != nil {
							log.Printf("创建配置目录失败: %v\n", err)
							panic("创建配置目录失败")
						}
						// 迁移旧配置
						err = helpers.MoveDir(oldConfigDir, configDir)
						if err != nil {
							log.Printf("迁移旧配置目录失败: %v\n", err)
							panic("迁移旧配置目录失败")
						}
						needMk = false
					}
				}
			}
			dataDir = filepath.Join(configDir, "postgres") // 数据库目录
			helpers.DataDir = dataDir
			helpers.ConfigDir = configDir
		}
	}
	if needMk {
		err := os.MkdirAll(configDir, 0755)
		if err != nil {
			log.Printf("创建配置目录失败: %v\n", err)
			panic("创建配置目录失败")
		}
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
	config.C.Emby.EpisodesUnplayPrior = false // 关闭剧集排序
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
	os.MkdirAll(logPath, 0755) // 如果没有logs目录则创建
	libLogPath := filepath.Join(logPath, "libs")
	os.MkdirAll(libLogPath, 0755) // 如果没有logs/libs目录则创建
	helpers.AppLogger = helpers.NewLogger(helpers.GlobalConfig.Log.File, true, true)
	helpers.V115Log = helpers.NewLogger(helpers.GlobalConfig.Log.V115, false, true)
	helpers.OpenListLog = helpers.NewLogger(helpers.GlobalConfig.Log.OpenList, false, true)
	helpers.TMDBLog = helpers.NewLogger(helpers.GlobalConfig.Log.TMDB, false, true)
	helpers.BaiduPanLog = helpers.NewLogger(helpers.GlobalConfig.Log.BaiduPan, false, true)
}

func initOthers() {
	helpers.InitEventBus() // 初始化事件总线
	models.LoadSettings()  // 从数据库加载设置
	// 初始化GitHub访问管理器
	github.InitManager(models.SettingsGlobal.HttpProxy)
	helpers.AppLogger.Infof("已加载配置，准备初始化115请求队列，线程数: %d", models.SettingsGlobal.FileDetailThreads)
	qps := models.SettingsGlobal.FileDetailThreads
	if qps <= 0 {
		qps = 2
	}
	v115open.SetGlobalExecutorConfig(qps, qps*60, qps*3600)
	models.LoadScrapeSettings()          // 从数据库加载刮削设置
	models.InitDQ()                      // 初始化下载队列
	models.InitUQ()                      // 初始化上传队列
	models.InitNotificationManager()     // 初始化通知管理器
	controllers.StartListenTelegramBot() // 初始化TelegramBot监听
	models.GetEmbyConfig()               // 加载Emby配置
	helpers.SubscribeSync(helpers.V115TokenInValidEvent, models.HandleV115TokenInvalid)
	helpers.SubscribeSync(helpers.SaveOpenListTokenEvent, models.HandleOpenListTokenSaveSync)
	models.FailAllRunningSyncTasks()   // 将所有运行中的同步任务设置为失败状态
	synccron.RefreshOAuthAccessToken() // 启动时刷新一次115的访问凭证，防止有过期的token导致同步失败

	// 设置115请求队列的统计保存回调函数
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
	// 初始化WebSocket事件中心
	wsHub := websocket.NewEventHub()
	websocket.GlobalEventHub = wsHub
	go wsHub.Run()
	synccron.InitCron()       // 初始化定时任务（包含备份定时任务）
	synccron.InitSyncCron()   // 初始化同步目录的定时任务
	synccron.InitScrapeCron() // 初始化刮削目录的自定义定时任务
	synccron.InitTokenCron()  // 初始化定时刷新115的访问凭证
	// 初始化备份服务
	models.InitBackupService()
	// 将所有刮削中和整理中的记录改为未执行
	models.ResetScrapePathStatus()
	// 将所有刮削中改为待刮削
	models.UpdateScrapeMediaStatus(models.ScrapeMediaStatusScraping, models.ScrapeMediaStatusScanned, 0)
	// 将所有整理中的记录改为待整理
	models.UpdateScrapeMediaStatus(models.ScrapeMediaStatusRenaming, models.ScrapeMediaStatusScraped, 0)
	// 上传中的任务改为待上传
	models.UpdateUploadingToPending()
	// 下载中的任务改为待下载
	models.UpdateDownloadingToPending()
	helpers.Subscribe(helpers.BackupCronEevent, func(event helpers.Event) {
		backup.Backup("定时", "定时备份")
	})
	helpers.Subscribe(helpers.StrmSyncCompleteEvent, func(event helpers.Event) {
		// 触发关联的刮削任务
		scrapePathIds := event.Data.([]uint)
		// 将任务添加到队列中
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
			}
		}
	})
}

func main() {
	// 解析命令行参数
	flag.BoolVar(&Update, "update", false, "是否更新")
	flag.Parse()
	if Update {
		// 执行更新逻辑
		return
	}
	getRootDir()
	getDataAndConfigDir()
	initTimeZone()
	newApp()

	// ===== 初始化配置 =====
	configFile := filepath.Join(helpers.ConfigDir, "config.yml")
	if !helpers.PathExists(configFile) {
		helpers.AppLogger = nil
		defaultCfg := helpers.MakeDefaultConfig()
		// 用环境变量覆盖默认值
		if v := os.Getenv("DB_HOST"); v != "" {
			defaultCfg.Db.PostgresConfig.Host = v
		}
		if v := os.Getenv("DB_PORT"); v != "" {
			defaultCfg.Db.PostgresConfig.Port = helpers.StringToInt(v)
		}
		if v := os.Getenv("DB_USER"); v != "" {
			defaultCfg.Db.PostgresConfig.User = v
		}
		if v := os.Getenv("DB_PASSWORD"); v != "" {
			defaultCfg.Db.PostgresConfig.Password = v
		}
		if v := os.Getenv("DB_NAME"); v != "" {
			defaultCfg.Db.PostgresConfig.Database = v
		}
		if v := os.Getenv("DB_SSLMODE"); v == "require" {
			defaultCfg.Db.PostgresConfig.SSL = true
		}
		// 默认 SQLite，最稳定
		defaultCfg.Db.Engine = helpers.DbEngineSqlite
		defaultCfg.Db.SqliteFile = "qmediasync.db"
		if err := helpers.SaveConfig(defaultCfg); err != nil {
			log.Fatal("生成默认配置失败:", err)
		}
		fmt.Printf("已生成默认配置：%s\n", configFile)
	}
	if err := helpers.InitConfig(); err != nil {
		log.Fatal("加载配置失败:", err)
	}
	// ======================

	initLogger()
	// 启动数据库
	if err := QMSApp.StartDatabase(false); err != nil {
		log.Fatal("启动数据库失败:", err)
	}
	initOthers()
	QMSApp.Start()
	_ = sql.ErrNoRows
	_ = flag.ErrHelp
	_ = template.HTMLEscapeString
}