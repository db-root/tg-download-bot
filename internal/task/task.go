package task

import (
	"sync"
	"time"
)

// Status 任务状态
type Status int

const (
	StatusExtracting Status = iota // 提取媒体信息中
	StatusAwaitKind                // 等待选择下载类型（视频/图片/全部）
	StatusAwaitPath                // 等待用户选择推送路径
	StatusAwaitName                // 等待用户确认/修改文件名
	StatusDownloading              // 下载中
	StatusPushing                  // 推送中（移动/rsync）
	StatusDone                     // 完成
	StatusFailed                   // 失败
	StatusCancelled                // 已取消
)

func (s Status) String() string {
	switch s {
	case StatusExtracting:
		return "提取中"
	case StatusAwaitKind:
		return "等待选择类型"
	case StatusAwaitPath:
		return "等待选择路径"
	case StatusAwaitName:
		return "等待确认名称"
	case StatusDownloading:
		return "下载中"
	case StatusPushing:
		return "推送中"
	case StatusDone:
		return "完成"
	case StatusFailed:
		return "失败"
	case StatusCancelled:
		return "已取消"
	}
	return "未知"
}

// MediaKind 媒体类型
type MediaKind string

const (
	KindVideo MediaKind = "video"
	KindPhoto MediaKind = "photo"
	KindFile  MediaKind = "file"
)

// MediaItem 单个媒体文件
type MediaItem struct {
	Kind       MediaKind
	DocID      int64 // document: tg.Document.ID; photo: tg.Photo.ID
	AccessHash int64
	FileRef    []byte
	ThumbType  string // photo 用：选择的尺寸 type
	Size       int64
	MimeType   string
	RawName    string // 原始文件名（photo 为空）
	Ext        string // 扩展名（photo 固定 .jpg）
}

// Media 媒体集合（一条消息/一个相册）
type Media struct {
	Items   []*MediaItem
	Caption string // 说明文字（取第一条带 caption 的）
}

// HasKind 是否包含指定类型
func (m *Media) HasKind(k MediaKind) bool {
	for _, it := range m.Items {
		if it.Kind == k {
			return true
		}
	}
	return false
}

// Count 各类型数量
func (m *Media) Count() map[MediaKind]int {
	out := map[MediaKind]int{}
	for _, it := range m.Items {
		out[it.Kind]++
	}
	return out
}

// Task 一个转发消息对应一个任务
type Task struct {
	ID              int64
	ChatID          int64
	TriggerMsgID    int64 // 触发任务的原始消息 ID
	PromptMsgID     int64 // 当前交互提示消息 ID（可编辑推进）
	Status          Status
	Media           *Media
	KindFilter      MediaKind // ""=全部
	DetectedName    string    // 引擎识别出的名字（含扩展名；folder 模式为文件夹名）
	FinalName       string    // 确认后的名字
	FolderMode      bool      // 多文件：下载到以 FinalName 命名的文件夹
	Target          string    // 推送目标名
	AwaitSince      time.Time
	NameEditPending bool // 等待用户发送新文件名
	Err             error
	Cancel          chan struct{} // 取消信号（下载中可用）
}

// Manager 任务管理器：存储 + 状态推进 + 下载并发信号量
type Manager struct {
	mu       sync.Mutex
	nextID   int64
	tasks    map[int64]*Task
	sem      chan struct{} // 下载并发控制
	timeouts timeouts
}

type timeouts struct {
	pathMin int
	nameMin int
}

// New 创建管理器
func New(maxConcurrent int, pathTimeoutMin, nameTimeoutMin int) *Manager {
	return &Manager{
		nextID: 1,
		tasks:  make(map[int64]*Task),
		sem:    make(chan struct{}, maxConcurrent),
		timeouts: timeouts{
			pathMin: pathTimeoutMin,
			nameMin: nameTimeoutMin,
		},
	}
}

// Create 创建任务
func (m *Manager) Create(chatID, triggerMsgID int64) *Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := &Task{
		ID:           m.nextID,
		ChatID:       chatID,
		TriggerMsgID: triggerMsgID,
		Status:       StatusExtracting,
		Cancel:       make(chan struct{}),
	}
	m.nextID++
	m.tasks[t.ID] = t
	return t
}

// Get 获取任务
func (m *Manager) Get(id int64) *Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tasks[id]
}

// Update 原子更新任务（f 内可安全修改字段）
func (m *Manager) Update(id int64, f func(t *Task)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tasks[id]; ok {
		f(t)
	}
}

// SetStatus 设置状态；进入等待态时记录 AwaitSince
func (m *Manager) SetStatus(id int64, s Status) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tasks[id]; ok {
		t.Status = s
		if s == StatusAwaitKind || s == StatusAwaitPath || s == StatusAwaitName {
			t.AwaitSince = time.Now()
		}
	}
}

// Acquire 获取下载并发槽（阻塞）
func (m *Manager) Acquire() {
	m.sem <- struct{}{}
}

// Release 释放下载并发槽
func (m *Manager) Release() {
	<-m.sem
}

// ExpiredAwait 返回所有已超时的等待态任务
func (m *Manager) ExpiredAwait() []*Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	var out []*Task
	for _, t := range m.tasks {
		var limit time.Duration
		switch t.Status {
		case StatusAwaitKind, StatusAwaitPath:
			limit = time.Duration(m.timeouts.pathMin) * time.Minute
		case StatusAwaitName:
			limit = time.Duration(m.timeouts.nameMin) * time.Minute
		default:
			continue
		}
		if now.Sub(t.AwaitSince) >= limit {
			out = append(out, t)
		}
	}
	return out
}

// Active 进行中任务（等待/下载/推送，未结束）
func (m *Manager) Active() []*Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Task
	for _, t := range m.tasks {
		switch t.Status {
		case StatusDone, StatusFailed, StatusCancelled:
			continue
		}
		out = append(out, t)
	}
	return out
}

// All 返回所有任务
func (m *Manager) All() []*Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		out = append(out, t)
	}
	return out
}

// Remove 移除已完成/失败的任务（释放内存）
func (m *Manager) Remove(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tasks, id)
}
