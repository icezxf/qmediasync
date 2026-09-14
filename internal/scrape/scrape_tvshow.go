package scrape

import (
	"Q115-STRM/internal/baidupan"
	"Q115-STRM/internal/db"
	"Q115-STRM/internal/douban"
	"Q115-STRM/internal/helpers"
	"Q115-STRM/internal/models"
	"Q115-STRM/internal/openlist"
	"Q115-STRM/internal/tmdb"
	"Q115-STRM/internal/v115open"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

type tvShowScrapeImpl struct {
	ScrapeBase
	fileTasks    chan *tvshowTask
	episodeTasks chan uint
}

type tvshowTask struct {
	mediaFile *models.ScrapeMediaFile
	seasons   []uint
}

func NewTvShowScrapeImpl(scrapePath *models.ScrapePath, ctx context.Context, v115Client *v115open.OpenClient, openlistClient *openlist.Client, baiduPanClient *baidupan.Client) scrapeImpl {
	tmdbImpl := NewTmdbTvShowImpl(scrapePath, ctx)
	return &tvShowScrapeImpl{
		ScrapeBase: ScrapeBase{
			scrapePath:     scrapePath,
			ctx:            ctx,
			identifyImpl:   NewIdTvShowImpl(scrapePath, ctx, tmdbImpl),
			categoryImpl:   NewCategoryTvShowImpl(scrapePath),
			renameImpl:     NewRenameTvShowImpl(scrapePath, ctx, v115Client, openlistClient, baiduPanClient),
			tmdbClient:     tmdbImpl.Client,
			v115Client:     v115Client,
			baiduPanClient: baiduPanClient,
			openlistClient: openlistClient,
		},
	}
}

// 先处理电视剧：用PathId分组，每组取第一条，识别完后更新同步批次同PathId的所有记录的tmdbid，name, year，然后将这些ID放入待处理队列
func (t *tvShowScrapeImpl) Start() error {
	total := models.GetScannedScrapeMediaFilesTotal(t.scrapePath.ID, t.scrapePath.MediaType)
	if total == 0 {
		helpers.AppLogger.Infof("没有待刮削和待整理的记录，无需启动刮削任务")
		return nil
	}
	t.fileTasks = make(chan *tvshowTask, t.scrapePath.GetMaxThreads())
	t.episodeTasks = make(chan uint, 100)
	wg := &sync.WaitGroup{}
	episodeWg := &sync.WaitGroup{}
	for i := 0; i < t.scrapePath.GetMaxThreads(); i++ {
		go t.scrapeTvShow(i+1, wg, episodeWg)
		go t.scrapeEpisode(i+1, episodeWg)
	}
	stopChan := make(chan struct{})
	go func() {
		for {
			select {
			case <-t.ctx.Done():
			stoploop:
				for {
					select {
					case tvshowTask := <-t.fileTasks:
						helpers.AppLogger.Infof("清理剩余电视剧刮削任务 %d", tvshowTask.mediaFile.ID)
						wg.Done()
					case episodeMediaFileID := <-t.episodeTasks:
						helpers.AppLogger.Infof("清理剩余集刮削任务 %d", episodeMediaFileID)
						episodeWg.Done()
					default:
						break stoploop
					}
				}
				time.Sleep(time.Second)
				return
			case <-stopChan:
				helpers.AppLogger.Infof("刮削任务接收到停止信号，退出清理循环")
				return
			default:
				helpers.AppLogger.Infof("1秒后继续检查是否已经停止任务")
				time.Sleep(time.Second)
			}
		}
	}()
mainloop:
	for {
		select {
		case <-t.ctx.Done():
			helpers.AppLogger.Infof("刮削任务接收到取消信号，退出主循环")
			break mainloop
		default:
		}
		mediaFiles := models.GetScannedScrapeMediaFilesGroupByTvshowPathId(t.scrapePath.ID, t.scrapePath.GetMaxThreads()*2)
		if len(mediaFiles) == 0 {
			helpers.AppLogger.Infof("所有待刮削和待整理记录都已加入处理队列，关闭队列通道，等待执行完成")
			break mainloop
		}
		tvshowTasks := make(map[string]*tvshowTask, 0)
	fileloop:
		for _, mediaFile := range mediaFiles {
			if _, ok := tvshowTasks[mediaFile.TvshowPathId]; !ok {
				tvshowTasks[mediaFile.TvshowPathId] = &tvshowTask{
					mediaFile: mediaFile,
					seasons:   make([]uint, 0),
				}
			}
			if slices.Contains(tvshowTasks[mediaFile.TvshowPathId].seasons, mediaFile.ID) {
				continue fileloop
			}
			tvshowTasks[mediaFile.TvshowPathId].seasons = append(tvshowTasks[mediaFile.TvshowPathId].seasons, mediaFile.ID)
		}
		for _, tvshowTask := range tvshowTasks {
			t.fileTasks <- tvshowTask
			wg.Add(1)
			helpers.AppLogger.Infof("电视剧 %s 已加入处理队列", tvshowTask.mediaFile.VideoFilename)
		}
		helpers.AppLogger.Infof("已加入 %d 个季到处理队列，等待刮削完成", len(tvshowTasks))
		wg.Wait()
		episodeWg.Wait()
	}
	helpers.AppLogger.Infof("所有集刮削整理任务都已完成，发送停止信号")
	select {
	case stopChan <- struct{}{}:
		helpers.AppLogger.Infof("已发送信号让所有清理监控任务退出")
	default:
	}
	close(stopChan)
	close(t.fileTasks)
	close(t.episodeTasks)
	helpers.AppLogger.Infof("所有刮削整理任务都已完成，本次任务结束")
	return nil
}

func (t *tvShowScrapeImpl) scrapeTvShow(taskIndex int, wg *sync.WaitGroup, episodeWg *sync.WaitGroup) {
mainloop:
	for {
		select {
		case <-t.ctx.Done():
			helpers.AppLogger.Infof("电视剧工作线程 %d 检测到停止信号，退出", taskIndex)
			return
		case tt, ok := <-t.fileTasks:
			if !ok {
				helpers.AppLogger.Infof("电视剧工作线程 %d 已关闭", taskIndex)
				return
			}
			helpers.AppLogger.Infof("电视剧工作线程 %d 开始处理电视剧 %s", taskIndex, filepath.Base(tt.mediaFile.TvshowPath))
			err := t.ProcessTvShow(tt)
			if err != nil {
				wg.Done()
				helpers.AppLogger.Errorf("电视剧工作线程 %d 刮削电视剧 %s 失败: %v", taskIndex, filepath.Base(tt.mediaFile.TvshowPath), err)
				continue mainloop
			}
			helpers.AppLogger.Infof("电视剧工作线程 %d 完成处理电视剧 %s", taskIndex, filepath.Base(tt.mediaFile.TvshowPath))
		seasonloop:
			for _, seasonMediaFileID := range tt.seasons {
				if !t.scrapePath.IsRunning() {
					helpers.AppLogger.Infof("电视剧工作线程 %d 检测到刮削任务已停止，退出", taskIndex)
					break seasonloop
				}
				seasonMediaFile := models.GetScrapeMediaFileById(seasonMediaFileID)
				helpers.AppLogger.Infof("电视剧工作线程 %d 开始处理电视剧 %s 季 %d", taskIndex, filepath.Base(tt.mediaFile.TvshowPath), seasonMediaFile.SeasonNumber)
				serr := t.ProcessSeason(seasonMediaFile)
				if serr != nil {
					helpers.AppLogger.Errorf("电视剧工作线程 %d 刮削电视剧 %s 季 %d 失败: %v", taskIndex, filepath.Base(tt.mediaFile.TvshowPath), seasonMediaFile.SeasonNumber, serr)
					continue seasonloop
				}
				helpers.AppLogger.Infof("电视剧工作线程 %d 完成处理电视剧 %s 季 %d", taskIndex, filepath.Base(tt.mediaFile.TvshowPath), seasonMediaFile.SeasonNumber)
				episodeMediaFileIds := models.GetScrapeMediaFileIdBySeasonId(seasonMediaFile.MediaSeasonId)
				if len(episodeMediaFileIds) == 0 {
					helpers.AppLogger.Infof("电视剧工作线程 %d 季 %d 下没有集，跳过", taskIndex, seasonMediaFile.SeasonNumber)
					continue seasonloop
				}
				for _, episodeMediaFileID := range episodeMediaFileIds {
					if !t.scrapePath.IsRunning() {
						helpers.AppLogger.Infof("电视剧工作线程 %d 检测到刮削任务已停止，退出", taskIndex)
						break seasonloop
					}
					t.episodeTasks <- episodeMediaFileID
					episodeWg.Add(1)
					helpers.AppLogger.Infof("电视剧工作线程 %d 已将 %d 加入集处理队列", taskIndex, episodeMediaFileID)
				}
			}
			wg.Done()
		}
	}
}

// 根据提取的信息，先确定电视剧和季
// 然后查询电视剧和季是否存在，如果存在未刮削则刮削电视剧和季；如果存在已刮削，则刮削集
// 整理阶段，先查询电视剧和季是否整理，如果未整理先整理电视剧和季，然后整理集
func (t *tvShowScrapeImpl) ProcessTvShow(tt *tvshowTask) error {
	mediaFile := tt.mediaFile
	mediaFile.ScrapeRootPath = filepath.Join(helpers.ConfigDir, "tmp", "刮削临时文件", fmt.Sprintf("%d", mediaFile.ScrapePathId), "电视剧")
	if err := os.MkdirAll(mediaFile.ScrapeRootPath, 0777); err != nil {
		helpers.AppLogger.Errorf("创建临时目录失败: %v", err)
		return err
	}
	if mediaFile.Media == nil || (mediaFile.Media != nil && mediaFile.Media.Status == models.MediaStatusUnScraped) {
		t.FillTvshowPath(mediaFile)
		err := t.ScrapeTvshow(mediaFile)
		if err != nil {
			t.ScrapeFailedAllEdpisode(mediaFile, err.Error())
			return err
		}
		t.UpdateTvshowDataToAllEpisode(mediaFile)
	}
	if mediaFile.Media != nil && mediaFile.Media.Status == models.MediaStatusScraped {
		t.MakeTvshowPath(mediaFile, t.scrapePath.CategoryMap)
		if t.scrapePath.ScrapeType != models.ScrapeTypeOnlyRename {
			if uerr := t.UploadTvshowScrapeFile(mediaFile); uerr != nil {
				t.RenamedFailedAllEdpisode(mediaFile, uerr.Error())
				return uerr
			}
			mediaFile.Media.Status = models.MediaStatusRenamed
			mediaFile.Media.Save()
			helpers.AppLogger.Infof("电视剧 %s 元数据上传完成，标记为已整理", mediaFile.Media.Name)
		}
	}
	return nil
}

func (t *tvShowScrapeImpl) FillTvshowPath(mediaFile *models.ScrapeMediaFile) error {
	if t.scrapePath.SourceType == models.SourceType115 && mediaFile.TvshowPathId == "" && mediaFile.TvshowPath != "" {
		tvshowPathDetail, err := t.v115Client.GetFsDetailByPath(t.ctx, mediaFile.TvshowPath)
		if err != nil {
			helpers.AppLogger.Errorf("从115中查询电视剧目录详情 %s 失败: %v", mediaFile.TvshowPath, err)
			return err
		}
		mediaFile.TvshowPathId = tvshowPathDetail.FileId
	}
	return nil
}

func (t *tvShowScrapeImpl) ScrapeTvshow(mediaFile *models.ScrapeMediaFile) error {
	if err := t.identifyImpl.Identify(mediaFile); err != nil {
		return err
	}
	if scrapeErr := t.ScrapeTvshowMedia(mediaFile); scrapeErr != nil {
		return scrapeErr
	}
	if cerr := t.GenrateCategory(mediaFile); cerr != nil {
		return cerr
	}
	t.GenerateNewTvshowName(mediaFile)
	if mediaFile.ScrapeType != models.ScrapeTypeOnlyRename {
		localTempPath := mediaFile.GetTmpFullTvshowPath()
		if err := os.MkdirAll(localTempPath, 0777); err != nil {
			helpers.AppLogger.Errorf("创建临时目录 %s 失败，下次重试，错误: %v", localTempPath, err)
			return err
		} else {
			helpers.AppLogger.Infof("临时目录 %s 创建成功", localTempPath)
		}
		t.GenerateTvShowNfo(mediaFile, localTempPath, t.scrapePath.ExcludeNoImageActor)
		fileList := map[string]string{}
		fileList[t.GetTvshowRealName(mediaFile, "poster.jpg", "image")] = mediaFile.Media.PosterPath
		fileList[t.GetTvshowRealName(mediaFile, "clearlogo.jpg", "image")] = mediaFile.Media.LogoPath
		fileList[t.GetTvshowRealName(mediaFile, "fanart.jpg", "image")] = mediaFile.Media.BackdropPath
		t.DownloadImages(localTempPath, v115open.DEFAULTUA, fileList)
	}
	return nil
}

func (t *tvShowScrapeImpl) ScrapeTvshowMedia(mediaFile *models.ScrapeMediaFile) error {
	helpers.AppLogger.Infof("刮削电视剧, 名字=%s，年份=%d, tmdbid=%d", mediaFile.Name, mediaFile.Year, mediaFile.TmdbId)
	tmdbInfo := &models.TmdbInfo{}
	tvDetail, err := t.tmdbClient.GetTvDetail(mediaFile.TmdbId, models.GlobalScrapeSettings.GetTmdbLanguage())
	if err != nil {
		helpers.AppLogger.Errorf("查询tmdb电视剧详情失败, 下次重试, 失败原因: %v", err)
		return err
	}
	tmdbInfo.TvShowDetail = tvDetail
	cast, _ := t.tmdbClient.GetTvCredits(mediaFile.TmdbId, models.GlobalScrapeSettings.GetTmdbLanguage())
	tmdbInfo.Credits = cast
	images, _ := t.tmdbClient.GetTvImages(mediaFile.TmdbId, models.GlobalScrapeSettings.GetTmdbImageLanguage())
	if images != nil {
		if len(images.Posters) == 0 && tvDetail.PosterPath != "" {
			images.Posters = append(images.Posters, tmdb.Image{
				FilePath: tvDetail.PosterPath,
			})
		}
		if len(images.Backdrops) == 0 && tvDetail.BackdropPath != "" {
			images.Backdrops = append(images.Backdrops, tmdb.Image{
				FilePath: tvDetail.BackdropPath,
			})
		}
		tmdbInfo.Images = images
	} else {
		tmdbInfo.Images = &tmdb.Images{}
		if tvDetail.PosterPath != "" {
			tmdbInfo.Images.Posters = append(tmdbInfo.Images.Posters, tmdb.Image{
				FilePath: tvDetail.PosterPath,
			})
		}
		if tvDetail.BackdropPath != "" {
			tmdbInfo.Images.Backdrops = append(tmdbInfo.Images.Backdrops, tmdb.Image{
				FilePath: tvDetail.BackdropPath,
			})
		}
	}
	t.MakeMediaFromTMDB(mediaFile, tmdbInfo)

	// ===== 豆瓣电视剧评分补全 =====
	t.enrichTVWithDoubanRating(mediaFile)
	// ===== 结束 =====

	return nil
}

// enrichTVWithDoubanRating 用豆瓣评分覆盖电视剧的 TMDB 评分（通过 IMDb ID 查询）
func (t *tvShowScrapeImpl) enrichTVWithDoubanRating(mediaFile *models.ScrapeMediaFile) {
	if mediaFile.Media == nil {
		return
	}

	imdbId := mediaFile.Media.ImdbId
	if imdbId == "" {
		helpers.AppLogger.Infof("[豆瓣] 剧集没有 IMDb ID，跳过: %s", mediaFile.Media.Name)
		return
	}

	doubanClient := douban.NewClient("")
	rating, err := doubanClient.GetTVRatingByImdb(imdbId)
	if err != nil {
		helpers.AppLogger.Warnf("[豆瓣] 获取电视剧评分失败: %v, 剧集: %s (IMDb: %s)", err, mediaFile.Media.Name, imdbId)
		return
	}

	if rating > 0 {
		oldRating := mediaFile.Media.VoteAverage
		if oldRating == rating {
			helpers.AppLogger.Infof("[豆瓣] 电视剧评分未变化: %s (%.1f)", mediaFile.Media.Name, rating)
			return
		}
		mediaFile.Media.VoteAverage = rating
		mediaFile.Media.Save()
		helpers.AppLogger.Infof("[豆瓣] 电视剧评分已更新: %s (%.1f -> %.1f)", mediaFile.Media.Name, oldRating, rating)
	} else {
		helpers.AppLogger.Infof("[豆瓣] 未找到电视剧评分: %s (IMDb: %s)", mediaFile.Media.Name, imdbId)
	}
}

func (t *tvShowScrapeImpl) GetTvshowUploadFiles(mediaFile *models.ScrapeMediaFile) []uploadFile {
	destPath := mediaFile.GetDestFullTvshowPath()
	destPathId := mediaFile.NewPathId
	tvshowSourcePath := mediaFile.GetTmpFullTvshowPath()
	fileList := make([]uploadFile, 0)
	nfoName := t.GetTvshowRealName(mediaFile, "", "nfo")
	nfoPath := filepath.Join(tvshowSourcePath, nfoName)
	helpers.AppLogger.Infof("nfo文件路径 %s", nfoPath)
	if helpers.PathExists(nfoPath) {
		file := uploadFile{
			ID:         fmt.Sprintf("%d", mediaFile.ID),
			FileName:   nfoName,
			SourcePath: nfoPath,
			DestPath:   destPath,
			DestPathId: destPathId,
		}
		fileList = append(fileList, file)
	}
	imageList := []string{"poster.jpg", "clearlogo.jpg", "clearart.jpg", "square.jpg", "logo.jpg", "fanart.jpg", "backdrop.jpg", "background.jpg", "4kbackground.jpg", "thumb.jpg", "banner.jpg", "disc.jpg"}
	for _, im := range imageList {
		name := t.GetTvshowRealName(mediaFile, im, "image")
		sPath := filepath.Join(tvshowSourcePath, name)
		if helpers.PathExists(sPath) {
			file := uploadFile{
				ID:         fmt.Sprintf("%d", mediaFile.ID),
				FileName:   name,
				SourcePath: sPath,
				DestPath:   destPath,
				DestPathId: destPathId,
			}
			fileList = append(fileList, file)
		}
	}
	return fileList
}

// 上传电视剧所有生成好的文件
// 先上传电视剧和季的
// 再上传集的
func (t *tvShowScrapeImpl) UploadTvshowScrapeFile(mediaFile *models.ScrapeMediaFile) error {
	helpers.AppLogger.Infof("开始处理电视剧 %s 的元数据上传", mediaFile.Name)
	files := t.GetTvshowUploadFiles(mediaFile)
	ok, err := t.MoveLocalTempFileToDest(mediaFile, files)
	if err == nil {
		helpers.AppLogger.Infof("移动本地临时文件到目标位置成功")
		return nil
	}
	if !ok {
		helpers.AppLogger.Errorf("移动本地临时文件到目标位置失败, 失败原因: %v", err)
		return err
	}
	for _, file := range files {
		if !helpers.PathExists(file.SourcePath) {
			helpers.AppLogger.Errorf("本地临时文件 %s 不存在，跳过上传", file.SourcePath)
			continue
		}
		err := models.AddUploadTaskFromMediaFile(mediaFile, t.scrapePath, file.FileName, file.SourcePath, filepath.Join(file.DestPath, file.FileName), file.DestPathId, true)
		if err != nil {
			helpers.AppLogger.Errorf("添加上传任务 %s 失败, 失败原因: %v", file.FileName, err)
		}
	}
	return nil
}

func (t *tvShowScrapeImpl) MakeMediaFromTMDB(mediaFile *models.ScrapeMediaFile, tmdbInfo *models.TmdbInfo) {
	if mediaFile.MediaId != 0 {
		mediaFile.QueryRelation()
	}
	if mediaFile.Media == nil {
		mediaFile.Media = &models.Media{
			ScrapePathId: mediaFile.ScrapePathId,
			MediaType:    mediaFile.MediaType,
			Name:         mediaFile.Name,
			Year:         mediaFile.Year,
			TmdbId:       mediaFile.TmdbId,
			Status:       models.MediaStatusUnScraped,
		}
		helpers.AppLogger.Infof("创建新的Media对象: %s, TMDBID=%d, 类型=%s", mediaFile.Media.Name, mediaFile.Media.TmdbId, mediaFile.Media.MediaType)
	}
	mediaFile.Media.FillInfoByTmdbInfo(tmdbInfo)
	mediaFile.MediaId = mediaFile.Media.ID
	mediaFile.Name = mediaFile.Media.Name
	mediaFile.Year = mediaFile.Media.Year
	mediaFile.Save()
}

func (t *tvShowScrapeImpl) GenrateCategory(mediaFile *models.ScrapeMediaFile) error {
	if !mediaFile.EnableCategory {
		return nil
	}
	categoryName, scrapePathCategory := t.categoryImpl.DoCategory(mediaFile)
	if categoryName == "" && scrapePathCategory == nil {
		helpers.AppLogger.Errorf("根据流派ID和语言确定电视剧的二级分类失败, 文件名: %s", mediaFile.Name)
		mediaFile.Failed("根据流派ID和语言确定电视剧的二级分类失败，停止刮削")
		return errors.New("根据流派ID和语言确定电视剧的二级分类失败")
	}
	mediaFile.CategoryName = categoryName
	mediaFile.ScrapePathCategoryId = scrapePathCategory.ID
	mediaFile.Save()
	helpers.AppLogger.Infof("根据流派ID和语言确定二级分类: %s, 分类目录ID:%s", categoryName, scrapePathCategory.FileId)
	return nil
}

// 生成新的电视剧路径
func (t *tvShowScrapeImpl) GenerateNewTvshowName(mediaFile *models.ScrapeMediaFile) {
	remotePath := mediaFile.GetRemoteTvshowPath()
	oldPathName := filepath.Base(remotePath)
	if mediaFile.ScrapeType == models.ScrapeTypeOnly {
		mediaFile.NewPathName = oldPathName
		return
	}
	folderTemplate := t.scrapePath.FolderNameTemplate
	if folderTemplate == "" && remotePath == "" {
		folderTemplate = "{title} ({year})"
	}
	if t.scrapePath.FolderNameTemplate == "" {
		if remotePath == "" {
			mediaFile.NewPathName = mediaFile.GenerateNameByTemplate(folderTemplate)
		} else {
			mediaFile.NewPathName = oldPathName
		}
	} else {
		mediaFile.NewPathName = mediaFile.GenerateNameByTemplate(t.scrapePath.FolderNameTemplate)
	}
	mediaFile.Media.Path = filepath.Join(mediaFile.DestPath, mediaFile.CategoryName, mediaFile.NewPathName)
	mediaFile.Save()
	mediaFile.Media.Save()
}

func (t *tvShowScrapeImpl) GetTvshowRealName(mediaFile *models.ScrapeMediaFile, name string, filetype string) string {
	if filetype == "nfo" {
		return "tvshow.nfo"
	}
	return name
}

func (t *tvShowScrapeImpl) GenerateTvShowNfo(mediaFile *models.ScrapeMediaFile, localTempPath string, excludeNoImageActor bool) error {
	nfoPath := filepath.Join(localTempPath, "tvshow.nfo")
	rates := []helpers.Rating{
		{
			Name:  "tmdb",
			Max:   10,
			Value: mediaFile.Media.VoteAverage,
			Votes: mediaFile.Media.VoteCount,
		},
	}
	genres := make([]string, 0)
	for _, genre := range mediaFile.Media.Genres {
		genres = append(genres, genre.Name)
	}
	has, result := helpers.ChineseToPinyin(mediaFile.Media.Name)
	originalTitle := mediaFile.Media.OriginalName
	SortTitle := mediaFile.Media.Name
	if has {
		originalTitle = fmt.Sprintf("%s #(%s)", mediaFile.Media.Name, result)
		SortTitle = fmt.Sprintf("%s #(%s)", result, mediaFile.Media.Name)
	}
	tv := &helpers.TVShow{
		Title:         mediaFile.Media.Name,
		OriginalTitle: originalTitle,
		SortTitle:     SortTitle,
		Ratings: struct {
			Rating []helpers.Rating `xml:"rating,omitempty"`
		}{
			Rating: rates,
		},
		UserRating: mediaFile.Media.VoteAverage,
		Outline:    fmt.Sprintf("<![CDATA[%s]]>", mediaFile.Media.Overview),
		Plot:       fmt.Sprintf("<![CDATA[%s]]>", mediaFile.Media.Overview),
		Tagline:    mediaFile.Media.Tagline,
		Year:       mediaFile.Media.Year,
		DateAdded:  time.Now().Format("2006-01-02"),
		Genre:      genres,
		Director:   mediaFile.Media.Director,
		Id:         mediaFile.Media.ImdbId,
		TmdbId:     mediaFile.Media.TmdbId,
		ImdbId:     mediaFile.Media.ImdbId,
		Premiered:  mediaFile.Media.ReleaseDate,
		Aired:      mediaFile.Media.ReleaseDate,
		Uniqueid: []helpers.UniqueId{
			{
				Id:      mediaFile.Media.ImdbId,
				Type:    "imdb",
				Default: true,
			},
			{
				Id:      fmt.Sprintf("%d", mediaFile.TmdbId),
				Type:    "tmdb",
				Default: false,
			},
		},
	}
	if excludeNoImageActor {
		tv.Actor = make([]helpers.Actor, 0)
		for _, actor := range mediaFile.Media.Actors {
			if actor.Thumb != "" {
				tv.Actor = append(tv.Actor, actor)
			}
		}
	} else {
		tv.Actor = mediaFile.Media.Actors
	}
	err := helpers.WriteTVShowNfo(tv, nfoPath)
	if err != nil {
		helpers.AppLogger.Errorf("生成电视剧nfo文件失败，文件路径：%s 错误： %v", nfoPath, err)
		return err
	} else {
		helpers.AppLogger.Infof("生成电视剧nfo文件成功，文件路径：%s", nfoPath)
	}
	return nil
}

// 创建父文件夹，电影是电影目录
func (t *tvShowScrapeImpl) MakeTvshowPath(mediaFile *models.ScrapeMediaFile, categoryMap map[uint]string) error {
	newPathId := ""
	if mediaFile.ScrapeType == models.ScrapeTypeOnly {
		newPathId = mediaFile.TvshowPathId
	} else {
		parentId := mediaFile.DestPathId
		if mediaFile.ScrapePathCategoryId > 0 {
			if category, ok := categoryMap[mediaFile.ScrapePathCategoryId]; ok {
				parentId = category
			}
		}
		destFullPath := mediaFile.GetDestFullTvshowPath()
		helpers.AppLogger.Infof("电视剧文件夹，目标路径：%s，根目录ID：%s", destFullPath, parentId)
		var err error
		newPathId, err = t.renameImpl.CheckAndMkDir(destFullPath, mediaFile.DestPath, mediaFile.DestPathId)
		if err != nil {
			helpers.AppLogger.Errorf("创建电视剧文件夹失败: %v", err)
			return err
		}
	}
	mediaFile.NewPathId = newPathId
	mediaFile.Media.PathId = newPathId
	mediaFile.Save()
	mediaFile.Media.Save()
	t.UpdateNewPathIdToAllEpisode(mediaFile)
	return nil
}

func (t *tvShowScrapeImpl) UpdateTvshowDataToAllEpisode(mediaFile *models.ScrapeMediaFile) error {
	updateData := map[string]interface{}{
		"media_id":                mediaFile.MediaId,
		"tvshow_path_id":          mediaFile.TvshowPathId,
		"name":                    mediaFile.Name,
		"year":                    mediaFile.Year,
		"tmdb_id":                 mediaFile.TmdbId,
		"new_path_name":           mediaFile.NewPathName,
		"new_path_id":             mediaFile.NewPathId,
		"category_name":           mediaFile.CategoryName,
		"scrape_path_category_id": mediaFile.ScrapePathCategoryId,
	}
	affectedRows := db.Db.Table("scrape_media_files").Where("scrape_path_id =? AND tvshow_path = ? AND batch_no = ?", mediaFile.ScrapePathId, mediaFile.TvshowPath, mediaFile.BatchNo).Updates(updateData).RowsAffected
	if affectedRows == 0 {
		helpers.AppLogger.Errorf("批量更新电视剧 %s 的所有集的信息失败, 未更新任何行", mediaFile.Name)
		return errors.New("no rows affected")
	}
	helpers.AppLogger.Infof("批量更新电视剧 %s 的所有集的信息成功，共更新 %d 行", mediaFile.Name, affectedRows)
	return nil
}

func (t *tvShowScrapeImpl) UpdateNewPathIdToAllEpisode(mediaFile *models.ScrapeMediaFile) error {
	affectedRows := db.Db.Table("scrape_media_files").Where("scrape_path_id =? AND tvshow_path_id = ? AND batch_no = ?", mediaFile.ScrapePathId, mediaFile.TvshowPathId, mediaFile.BatchNo).Updates(map[string]interface{}{
		"new_path_id": mediaFile.NewPathId,
	}).RowsAffected
	if affectedRows == 0 {
		helpers.AppLogger.Errorf("批量更新电视剧 %s 的所有集的新目录ID失败, 未更新任何行", mediaFile.Name)
		return errors.New("no rows affected")
	}
	helpers.AppLogger.Infof("批量更新电视剧 %s 的所有集的新目录ID成功，共更新 %d 行", mediaFile.Name, affectedRows)
	return nil
}

func (t *tvShowScrapeImpl) ScrapeFailedAllEdpisode(mediaFile *models.ScrapeMediaFile, failedReason string) error {
	err := db.Db.Table("scrape_media_files").Where("tvshow_path = ? AND batch_no = ?", mediaFile.TvshowPathId, mediaFile.BatchNo).Updates(map[string]interface{}{
		"status":        models.ScrapeMediaStatusScrapeFailed,
		"failed_reason": failedReason,
	}).Error
	if err != nil {
		helpers.AppLogger.Errorf("批量更新电视剧 %s 的所有集为刮削失败状态失败, 失败原因: %v", mediaFile.Name, err)
		return err
	}
	helpers.AppLogger.Infof("批量更新电视剧 %s 的所有集为刮削失败状态成功", mediaFile.Name)
	return nil
}

func (t *tvShowScrapeImpl) RenamedFailedAllEdpisode(mediaFile *models.ScrapeMediaFile, failedReason string) error {
	err := db.Db.Table("scrape_media_files").Where("tvshow_path = ? AND batch_no = ?", mediaFile.TvshowPathId, mediaFile.BatchNo).Updates(map[string]interface{}{
		"status":        models.ScrapeMediaStatusRenameFailed,
		"failed_reason": failedReason,
	}).Error
	if err != nil {
		helpers.AppLogger.Errorf("批量更新电视剧 %s 的所有集为整理失败状态失败, 失败原因: %v", mediaFile.Name, err)
		return err
	}
	helpers.AppLogger.Infof("批量更新电视剧 %s 的所有集为整理失败状态成功", mediaFile.Name)
	return nil
}

func (t *tvShowScrapeImpl) Rollback(mediaFile *models.ScrapeMediaFile) error {
	return nil
}

func (t *tvShowScrapeImpl) RollbackTvShow(mediaFile *models.ScrapeMediaFile) error {
	newBaseName := fmt.Sprintf("%s (%d) {tmdbid-%d}", mediaFile.Name, mediaFile.Year, mediaFile.TmdbId)
	if mediaFile.ScrapeType == models.ScrapeTypeOnly {
		uploadFiles := t.GetTvshowUploadFiles(mediaFile)
		files := make([]models.WillDeleteFile, 0)
		for _, uf := range uploadFiles {
			files = append(files, models.WillDeleteFile{FullFilePath: filepath.Join(uf.DestPath, uf.FileName)})
		}
		err := t.renameImpl.CheckAndDeleteFiles(mediaFile, files)
		if err != nil {
			helpers.AppLogger.Errorf("删除已上传的元数据文失败: %v", err)
			return err
		}
		helpers.AppLogger.Infof("删除已上传的元数据文件成功: %v", files)
	}
	if mediaFile.ScrapeType == models.ScrapeTypeScrapeAndRename || mediaFile.ScrapeType == models.ScrapeTypeOnlyRename {
		parentPath := filepath.Dir(mediaFile.TvshowPath)
		var newPath string
		var pathId string
		var existsPathId string = ""
		if mediaFile.TvshowPath == mediaFile.SourcePath {
			parentPath = mediaFile.SourcePath
			newPath = mediaFile.SourcePath
		} else {
			newPath = filepath.Join(parentPath, newBaseName)
		}
		if mediaFile.RenameType != models.RenameTypeMove && parentPath != mediaFile.SourcePath {
			var eerr error
			existsPathId, eerr = t.renameImpl.ExistsAndRename(mediaFile.TvshowPathId, newBaseName)
			if eerr != nil {
				helpers.AppLogger.Errorf("重命名旧文件夹 %s 失败: %v", mediaFile.TvshowPathId, eerr)
				return eerr
			}
		}
		if existsPathId == "" {
			if parentPath != mediaFile.SourcePath {
				var err error
				pathId, err = t.renameImpl.CheckAndMkDir(newPath, mediaFile.SourcePath, mediaFile.SourcePathId)
				if err != nil {
					helpers.AppLogger.Errorf("创建父文件夹 %s 失败: %v", newPath, err)
					return err
				}
			} else {
				pathId = mediaFile.SourcePathId
				newPath = mediaFile.SourcePath
			}
		} else {
			pathId = existsPathId
		}
		mediaFile.TvshowPath = newPath
		mediaFile.TvshowPathId = pathId
		err := t.UpdateTvshowPathAndIdToAllEpisode(mediaFile)
		if err != nil {
			helpers.AppLogger.Errorf("更新电视剧 %s 的所有集的路径失败: %v", mediaFile.Name, err)
			return err
		}
	}
	mediaFile.Media.Status = models.MediaStatusUnScraped
	mediaFile.Media.Save()
	helpers.AppLogger.Infof("回滚电视剧 %s 成功", mediaFile.Name)
	return nil
}

func (t *tvShowScrapeImpl) UpdateTvshowPathAndIdToAllEpisode(mediaFile *models.ScrapeMediaFile) error {
	updateData := map[string]interface{}{
		"tvshow_path_id": mediaFile.TvshowPathId,
		"tvshow_path":    mediaFile.TvshowPath,
	}
	affectedRows := db.Db.Table("scrape_media_files").Where("scrape_path_id =? AND media_id = ? AND batch_no = ?", mediaFile.ScrapePathId, mediaFile.MediaId, mediaFile.BatchNo).Updates(updateData).RowsAffected
	if affectedRows == 0 {
		helpers.AppLogger.Errorf("批量更新电视剧 %s 的所有集的信息失败, 未更新任何行", mediaFile.Name)
		return errors.New("no rows affected")
	}
	helpers.AppLogger.Infof("批量更新电视剧 %s 的所有集的信息成功，共更新 %d 行", mediaFile.Name, affectedRows)
	return nil
}