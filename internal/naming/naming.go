package naming

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"tg-download-bot/internal/config"
)

// Source 命名信息源
type Source string

const (
	SourceCaptionCmd     Source = "caption_cmd"     // caption 显式指令 #重命名
	SourceCaptionPattern Source = "caption_pattern" // caption 模式匹配
	SourceFileName       Source = "filename"        // 原始文件名清洗+匹配
	SourceFallback       Source = "fallback"        // 兜底
)

// Result 命名结果
type Result struct {
	Name   string // 最终文件名（含扩展名）
	Source Source // 信息源
}

// Engine 命名引擎
type Engine struct {
	patterns []compiledPattern
	strip    []*regexp.Regexp
	quality  []string
	fallback string
}

type compiledPattern struct {
	re       *regexp.Regexp
	template string
	season   int
	episode  int
}

// New 创建命名引擎
func New(cfg config.Rename) (*Engine, error) {
	e := &Engine{
		quality:  cfg.Quality,
		fallback: cfg.Fallback,
	}
	for _, p := range cfg.Patterns {
		re, err := regexp.Compile(p.Regex)
		if err != nil {
			return nil, fmt.Errorf("命名正则编译失败 %q: %w", p.Regex, err)
		}
		e.patterns = append(e.patterns, compiledPattern{re: re, template: p.Template})
	}
	for _, s := range cfg.Strip {
		re, err := regexp.Compile(s)
		if err != nil {
			return nil, fmt.Errorf("清洗正则编译失败 %q: %w", s, err)
		}
		e.strip = append(e.strip, re)
	}
	return e, nil
}

// Rename 根据 caption 和原始文件名生成规范名
// caption: 转发消息的说明文字（可能为空）
// rawName: 原始文件名（document 自带；video 消息为空）
// ext: 扩展名（如 .mp4），为空时从 rawName 推断
func (e *Engine) Rename(caption, rawName, ext string) Result {
	if ext == "" && rawName != "" {
		ext = filepath.Ext(rawName)
	}
	if ext == "" {
		ext = ".mp4"
	}
	ext = strings.ToLower(ext)

	// ① caption 显式指令
	if name, ok := e.parseExplicit(caption); ok {
		return Result{Name: name + ext, Source: SourceCaptionCmd}
	}

	// ② caption 模式匹配
	if name, ok := e.matchFromText(caption, ext); ok {
		return Result{Name: name, Source: SourceCaptionPattern}
	}

	// ③ 原始文件名清洗+匹配
	if rawName != "" {
		if name, ok := e.matchFromText(rawName, ext); ok {
			return Result{Name: name, Source: SourceFileName}
		}
		cleaned := e.clean(rawName)
		// 清洗后至少要去掉扩展名再拼（clean 保留 ext 也行，直接返回）
		if cleaned != "" {
			return Result{Name: cleaned, Source: SourceFileName}
		}
	}

	// ④ 兜底
	return Result{Name: e.fallbackName(rawName, ext), Source: SourceFallback}
}

// parseExplicit 解析 "#重命名 xxx" 或 "重命名: xxx" 指令
func (e *Engine) parseExplicit(caption string) (string, bool) {
	c := strings.TrimSpace(caption)
	for _, prefix := range []string{"#重命名", "重命名:", "重命名：", "#rename"} {
		if strings.HasPrefix(strings.ToLower(c), strings.ToLower(prefix)) {
			name := strings.TrimSpace(c[len(prefix):])
			name = e.clean(name)
			if name != "" {
				return name, true
			}
		}
	}
	return "", false
}

// matchFromText 在文本中尝试匹配集数模式，返回渲染后的文件名
func (e *Engine) matchFromText(text, ext string) (string, bool) {
	cleanText := e.clean(text)
	if cleanText == "" {
		return "", false
	}
	for _, p := range e.patterns {
		m := p.re.FindStringSubmatch(cleanText)
		if m == nil {
			continue
		}
		// 剧名 = 匹配位置之前的文本（去掉分隔符）
		loc := p.re.FindStringIndex(cleanText)
		namePart := strings.TrimSpace(cleanText[:loc[0]])
		namePart = strings.Trim(namePart, "._-— ")
		if namePart == "" {
			namePart = "未命名"
		}
		season, episode := 0, 0
		if len(m) > 1 {
			fmt.Sscanf(m[1], "%d", &season)
		}
		if len(m) > 2 {
			fmt.Sscanf(m[2], "%d", &episode)
		} else if len(m) == 2 {
			// 只有一组数字：优先当集数（第x集模式），若文本含 Sxx 则可能是季
			episode = season
			season = 0
		}
		quality := e.extractQuality(cleanText)
		return renderTemplate(p.template, namePart, season, episode, quality, ext), true
	}
	return "", false
}

// renderTemplate 渲染模板占位符
func renderTemplate(tpl, name string, season, episode int, quality, ext string) string {
	r := strings.NewReplacer(
		"{name}", name,
		"{season}", fmt.Sprintf("%02d", season),
		"{episode}", fmt.Sprintf("%02d", episode),
		"{quality}", quality,
		"{ext}", ext,
	)
	out := r.Replace(tpl)
	// 处理空质量标签时的残留分隔符（如 ".S01E05..mp4" -> ".S01E05.mp4"）
	out = strings.ReplaceAll(out, "..", ".")
	return out
}

// extractQuality 从文本提取质量标签
func (e *Engine) extractQuality(text string) string {
	upper := strings.ToUpper(text)
	for _, q := range e.quality {
		if strings.Contains(upper, strings.ToUpper(q)) {
			return q
		}
	}
	return ""
}

// clean 清洗文件名：去 strip 垃圾、非法字符、折叠空白
func (e *Engine) clean(s string) string {
	out := s
	for _, re := range e.strip {
		out = re.ReplaceAllString(out, "")
	}
	// 非法字符替换（兼容 Windows 路径）
	replacer := strings.NewReplacer(
		"\\", "_", "/", "_", ":", "_", "*", "_", "?", "_", `"`, "_", "<", "_", ">", "_", "|", "_",
	)
	out = replacer.Replace(out)
	// 折叠空白与多余分隔符
	out = regexp.MustCompile(`\s+`).ReplaceAllString(out, " ")
	out = regexp.MustCompile(`\s*[._-]+\s*`).ReplaceAllString(out, ".")
	out = strings.Trim(out, " ._-")
	return out
}

// fallbackName 兜底命名：日期前缀 + 清洗后的原名
func (e *Engine) fallbackName(rawName, ext string) string {
	base := e.clean(rawName)
	if base == "" {
		base = "video"
	}
	return time.Now().Format("2006-01-02") + "_" + base
}

// SplitExt 把文件名拆成 (去掉扩展名的主名, 扩展名)
func SplitExt(name string) (string, string) {
	ext := filepath.Ext(name)
	return strings.TrimSuffix(name, ext), ext
}
