package router

import (
	"tg-download-bot/internal/config"
)

// Router 目标路由：仅提供目标查询与默认目标（来源分流已移除）
type Router struct {
	cfg *config.Config
}

// New 创建路由器
func New(cfg *config.Config) *Router {
	return &Router{cfg: cfg}
}

// Target 返回目标定义（不存在返回 nil）
func (r *Router) Target(name string) *config.Target {
	t, ok := r.cfg.Targets[name]
	if !ok {
		return nil
	}
	return &t
}

// DefaultTarget 默认目标名（超时兜底用）
func (r *Router) DefaultTarget() string {
	return r.cfg.DefaultTarget
}
