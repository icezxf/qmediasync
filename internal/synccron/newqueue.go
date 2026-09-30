package synccron

import (
	"Q115-STRM/internal/helpers"
	"Q115-STRM/internal/models"
	"Q115-STRM/internal/scrape"
	"Q115-STRM/internal/syncstrm"
	ws "Q115-STRM/internal/websocket"
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
)

type SyncTaskType string

const (
	SyncTaskTypeStrm     SyncTaskType = "STRM同步"
	SyncTaskTypeScrape   SyncTaskType = "刮削整理"
	SyncTaskTypeAVScrape SyncTaskType = "AV刮削"
)

// AVScanHandler 由 main.go 注册，避免 synccron 和 avscrape 循环依赖
var AVScanHandler func(pathID uint) error

func logInfo(format string, args ...interface{}) {
	if helpers.AppLogger != nil {
		helpers.AppLogger.Infof(format, args...)
	}
}

func logError(format string, args ...interface{}) {
	if helpers.AppLogger != nil {
		helpers.AppLogger.Errorf(format, args...)
	}
}

const (
	QueueStatusRunning = "running"
	QueueStatusPaused  = "paused"
	QueueStatusStopped = "stopped"
)

const (
	TaskStatusNone    = 0
	TaskStatusWaiting = 1
	TaskStatusRunning = 2
)

type NewSyncTask struct {
	ID           uint
	TaskType     SyncTaskType
	SourcePath   string
	SourcePathId string
	TargetPath   string
	IsFile       bool
	SourceType   models.SourceType
	AccountId    uint
}

func (t *NewSyncTask) Key() string {
	if t.ID > 0 {
		return fmt.Sprintf("%d-%s", t.ID, t.TaskType)
	}
	return fmt.Sprintf("%s-%s", t.SourcePathId, t.TaskType)
}

type NewSyncQueuePerType struct {
	sourceType   models.SourceType
	taskChan     chan *NewSyncTask
	waitingQueue map[string]*NewSyncTask
	currentTask  *NewSyncTask
	status       string
	mutex        sync.RWMutex
	ctx          context.Context
	cancelFunc   context.CancelFunc
	runningFlag  int32
	scrapeInstance *scrape.Scrape
	strmSync       *syncstrm.SyncStrm
}

func NewQueuePerType(sourceType models.SourceType) *NewSyncQueuePerType {
	ctx, cancel := context.WithCancel(context.Background())
	return &NewSyncQueuePerType{
		sourceType:   sourceType,
		taskChan:     make(chan *NewSyncTask, 50),
		waitingQueue: make(map[string]*NewSyncTask),
		status:       QueueStatusRunning,
		ctx:          ctx,
		cancelFunc:   cancel,
	}
}

func (q *NewSyncQueuePerType) isTaskExists(task *NewSyncTask) bool {
	q.mutex.RLock()
	defer q.mutex.RUnlock()
	key := task.Key()
	if _, exists := q.waitingQueue[key]; exists {
		return true
	}
	if q.currentTask != nil && q.currentTask.Key() == key {
		return true
	}
	return false
}

func (q *NewSyncQueuePerType) AddTask(task *NewSyncTask) error {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	if q.isTaskExistsUnsafe(task) {
		return fmt.Errorf("任务已存在: 类型=%s, ID=%d", task.TaskType, task.ID)
	}
	if len(q.waitingQueue) >= cap(q.taskChan) {
		return fmt.Errorf("任务队列已满: 类型=%s, ID=%d", task.TaskType, task.ID)
	}
	q.waitingQueue[task.Key()] = task
	if q.status == QueueStatusRunning {
		select {
		case q.taskChan <- task:
			logInfo("任务已加入队列: 类型=%s, ID=%d", task.TaskType, task.ID)
		default:
			delete(q.waitingQueue, task.Key())
			return fmt.Errorf("任务队列已满: 类型=%s, ID=%d", task.TaskType, task.ID)
		}
		q.startProcessorIfNotRunningUnsafe()
	} else {
		logInfo("任务已加入暂停队列: 类型=%s, ID=%d", task.TaskType, task.ID)
	}
	return nil
}

func (q *NewSyncQueuePerType) isTaskExistsUnsafe(task *NewSyncTask) bool {
	key := task.Key()
	if _, exists := q.waitingQueue[key]; exists {
		return true
	}
	if q.currentTask != nil && q.currentTask.Key() == key {
		return true
	}
	return false
}

func (q *NewSyncQueuePerType) startProcessorIfNotRunningUnsafe() {
	logInfo("队列运行状态：%d", atomic.LoadInt32(&q.runningFlag))
	if atomic.CompareAndSwapInt32(&q.runningFlag, 0, 1) {
		go q.process()
	}
}

func (q *NewSyncQueuePerType) StartProcessor() {
	q.mutex.Lock()
	defer q.mutex.Unlock()
	q.startProcessorIfNotRunningUnsafe()
}

func (q *NewSyncQueuePerType) process() {
	logInfo("队列处理协程已启动: SourceType=%s", q.sourceType)
	defer atomic.StoreInt32(&q.runningFlag, 0)
	for {
		select {
		case <-q.ctx.Done():
			logInfo("队列处理协程已停止: SourceType=%s", q.sourceType)
			return
		case task, ok := <-q.taskChan:
			if !ok {
				logInfo("任务通道已关闭: SourceType=%s", q.sourceType)
				return
			}
			q.mutex.Lock()
			if _, exists := q.waitingQueue[task.Key()]; !exists {
				logInfo("任务已被取消，跳过处理: 类型=%s, ID=%d", task.TaskType, task.ID)
				q.mutex.Unlock()
				continue
			}
			q.currentTask = task
			delete(q.waitingQueue, task.Key())
			q.mutex.Unlock()

			logInfo("开始处理任务: 类型=%s, ID=%d", task.TaskType, task.ID)
			q.executeTask(task)

			q.mutex.Lock()
			q.currentTask = nil
			q.mutex.Unlock()
			logInfo("任务处理完成: 类型=%s, ID=%d", task.TaskType, task.ID)
		}
	}
}

func (q *NewSyncQueuePerType) executeTask(task *NewSyncTask) {
	defer func() {
		if r := recover(); r != nil {
			stack := make([]byte, 4096)
			length := runtime.Stack(stack, false)
			stackStr := string(stack[:length])
			logError("任务执行异常: 类型=%s, ID=%d, 错误=%v\n堆栈信息:\n%s",
				task.TaskType, task.ID, r, stackStr)
		}
	}()
	switch task.TaskType {
	case SyncTaskTypeStrm:
		q.executeStrmSync(task)
	case SyncTaskTypeScrape:
		q.executeScrape(task)
	case SyncTaskTypeAVScrape:
		q.executeAVScrape(task)
	}
}

func (q *NewSyncQueuePerType) executeAVScrape(task *NewSyncTask) {
	var avPath models.AVPath
	if err := models.DB().First(&avPath, task.ID).Error; err != nil {
		logError("获取AV刮削目录失败，ID=%d, 错误=%v", task.ID, err)
		return
	}
	if avPath.SourceType != q.sourceType {
		logError("AV刮削目录类型不匹配: 预期=%s, 实际=%s", q.sourceType, avPath.SourceType)
		return
	}
	if AVScanHandler == nil {
		logError("AVScanHandler 未注册，跳过 AV 刮削任务")
		return
	}
	logInfo("开始执行AV刮削任务: ID=%d, 目录=%s", task.ID, avPath.SourcePath)
	ws.BroadcastEvent(ws.EventScraperTaskStart, map[string]any{
		"task_id":   task.ID,
		"path_name": avPath.SourcePath,
	})
	if err := AVScanHandler(task.ID); err != nil {
		logError("AV刮削任务执行失败: ID=%d, 错误=%v", task.ID, err)
		ws.BroadcastEvent(ws.EventScraperTaskComplete, map[string]any{
			"task_id":   task.ID,
			"path_name": avPath.SourcePath,
			"success":   false,
		})
		return
	}
	logInfo("AV刮削任务执行成功: ID=%d", task.ID)
	ws.BroadcastEvent(ws.EventScraperTaskComplete, map[string]any{
		"task_id":   task.ID,
		"path_name": avPath.SourcePath,
		"success":   true,
	})
}