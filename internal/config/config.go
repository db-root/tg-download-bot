package config

import (
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

// Secrets 密钥配置（secrets.yaml，git 忽略）
type Secrets struct {
	APIID        int               `yaml:"api_id"`
	APIHash      string            `yaml:"api_hash"`
	BotToken     string            `yaml:"bot_token"`
	SSHKey       string            `yaml:"ssh_key"`
	SSHPasswords map[string]string `yaml:"ssh_passwords"` // 目标名 -> SSH 密码
}

// Bot 机器人行为配置
type Bot struct {
	SessionFile            string `yaml:"session_file"`
	DownloadDir            string `yaml:"download_dir"`
	MaxConcurrentDownloads int    `yaml:"max_concurrent_downloads"`
	DownloadTimeoutMin     int    `yaml:"download_timeout_min"`
	PathTimeoutMin         int    `yaml:"path_timeout_min"`
	NameTimeoutMin         int    `yaml:"name_timeout_min"`
	Proxy                  string `yaml:"proxy"`
	LogFile                string `yaml:"log_file"`       // 文件日志（按天滚动），空=仅控制台
	HistoryFile            string `yaml:"history_file"`   // 下载历史文件
	LogKeepDays            int    `yaml:"log_keep_days"`  // 日志保留天数（0=默认7）
	HistoryKeep            int    `yaml:"history_keep"`   // 历史保留条数（0=默认500）
	PartAgeHours           int    `yaml:"part_age_hours"` // .part 残留超时小时（0=默认24）
}

// Target 推送目标
type Target struct {
	Type   string `yaml:"type"` // local | rsync
	Path   string `yaml:"path"`
	Host   string `yaml:"host"`
	Port   int    `yaml:"port"`
	User   string `yaml:"user"`
	SSHKey string `yaml:"ssh_key"`
}

// RenamePattern 命名模式
type RenamePattern struct {
	Regex    string `yaml:"regex"`
	Template string `yaml:"template"`
}

// Rename 命名规则
type Rename struct {
	Patterns []RenamePattern `yaml:"patterns"`
	Strip    []string        `yaml:"strip"`
	Quality  []string        `yaml:"quality"`
	Fallback string          `yaml:"fallback"`
}

// Config 主配置
type Config struct {
	Bot           Bot               `yaml:"bot"`
	Targets       map[string]Target `yaml:"targets"`
	DefaultTarget string            `yaml:"default_target"`
	Rename        Rename            `yaml:"rename"`
	Secrets       Secrets           `yaml:"-"`
}

// Load 加载 config.yaml + secrets.yaml（secrets 单独读，避免误提交）
func Load(configPath string) (*Config, error) {
	cfg := &Config{}
	if err := readYAML(configPath, cfg); err != nil {
		return nil, fmt.Errorf("加载配置 %s: %w", configPath, err)
	}
	// 密钥文件与主配置同目录
	secretsPath := fileWithSuffix(configPath, "secrets.yaml")
	if err := readYAML(secretsPath, &cfg.Secrets); err != nil {
		return nil, fmt.Errorf("加载密钥 %s: %w", secretsPath, err)
	}
	cfg.applyDefaults()
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Bot.MaxConcurrentDownloads <= 0 {
		c.Bot.MaxConcurrentDownloads = 2
	}
	if c.Bot.DownloadTimeoutMin <= 0 {
		c.Bot.DownloadTimeoutMin = 120
	}
	if c.Bot.PathTimeoutMin <= 0 {
		c.Bot.PathTimeoutMin = 5
	}
	if c.Bot.NameTimeoutMin <= 0 {
		c.Bot.NameTimeoutMin = 5
	}
	if c.Bot.SessionFile == "" {
		c.Bot.SessionFile = "data/session"
	}
	if c.Bot.DownloadDir == "" {
		c.Bot.DownloadDir = "data/downloads"
	}
	if c.Bot.HistoryFile == "" {
		c.Bot.HistoryFile = "data/history.jsonl"
	}
	if c.Bot.LogKeepDays <= 0 {
		c.Bot.LogKeepDays = 7
	}
	if c.Bot.HistoryKeep <= 0 {
		c.Bot.HistoryKeep = 500
	}
	if c.Bot.PartAgeHours <= 0 {
		c.Bot.PartAgeHours = 24
	}
	if c.Rename.Fallback == "" {
		c.Rename.Fallback = "date_prefix"
	}
}

// TargetNames 返回所有目标名（排序保证键盘顺序稳定）
func (c *Config) TargetNames() []string {
	names := make([]string, 0, len(c.Targets))
	for name := range c.Targets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func readYAML(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, out)
}

// fileWithSuffix 把 /path/config.yaml 换成同目录下的 suffix 文件名
func fileWithSuffix(configPath, suffix string) string {
	dir := "."
	for i := len(configPath) - 1; i >= 0; i-- {
		if configPath[i] == '/' {
			dir = configPath[:i]
			break
		}
	}
	return dir + "/" + suffix
}
