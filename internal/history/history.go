package history

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Entry 一条下载历史
type Entry struct {
	Time     string `json:"time"` // 完成时间（本地时区）
	TaskID   int64  `json:"task_id"`
	Name     string `json:"name"`   // 文件名或文件夹名
	Size     int64  `json:"size"`   // 字节
	Target   string `json:"target"` // 推送目标名
	Dest     string `json:"dest"`   // 推送位置描述
	Status   string `json:"status"` // done / failed / cancelled
	Duration int64  `json:"duration_sec"`
}

// Store 历史存储（jsonl 追加写）
type Store struct {
	mu   sync.Mutex
	path string
}

// New 创建历史存储
func New(path string) (*Store, error) {
	s := &Store{path: path}
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return nil, err
	}
	return s, nil
}

// Append 追加一条记录
func (s *Store) Append(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	return err
}

// Recent 返回最近 n 条（从后往前）
func (s *Store) Recent(n int) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var entries []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(entries) > n {
		entries = entries[len(entries)-n:]
	}
	return entries, nil
}

// Clear 清空历史
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.Remove(s.path) // 不存在则忽略
}

// Now 当前时间字符串
func Now() string {
	return time.Now().Format("2006-01-02 15:04:05")
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			if i == 0 {
				return "/"
			}
			return path[:i]
		}
	}
	return "."
}
