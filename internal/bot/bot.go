package bot

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/sessionMaker"
	"github.com/celestix/gotgproto/types"
	"github.com/glebarez/sqlite"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"

	"tg-media-bot/internal/config"
	"tg-media-bot/internal/downloader"
	"tg-media-bot/internal/history"
	"tg-media-bot/internal/naming"
	"tg-media-bot/internal/proxy"
	"tg-media-bot/internal/router"
	"tg-media-bot/internal/task"
)

// Bot 机器人主体
type Bot struct {
	cfg       *config.Config
	client    *gotgproto.Client
	sharedCtx *ext.Context // 复用的发送上下文（goroutine 安全）
	mgr       *task.Manager
	namer     *naming.Engine
	router    *router.Router
	chatNames map[int64]string
	history   *history.Store

	// 文件日志（按天）
	logMu    sync.Mutex
	logFile  *os.File
	logDay   string

	// 相册聚合 GroupedID -> 聚合
	albumMu sync.Mutex
	albums  map[int64]*albumAgg
}

type albumAgg struct {
	groupID int64
	chatID  int64
	msgs    []*tg.Message
	timer   *time.Timer
}

// New 创建 Bot
func New(cfg *config.Config) (*Bot, error) {
	namer, err := naming.New(cfg.Rename)
	if err != nil {
		return nil, err
	}
	hs, err := history.New(cfg.Bot.HistoryFile)
	if err != nil {
		return nil, fmt.Errorf("初始化历史存储: %w", err)
	}
	b := &Bot{
		cfg:       cfg,
		mgr:       task.New(cfg.Bot.MaxConcurrentDownloads, cfg.Bot.PathTimeoutMin, cfg.Bot.NameTimeoutMin),
		namer:     namer,
		router:    router.New(cfg),
		chatNames: make(map[int64]string),
		history:   hs,
		albums:    make(map[int64]*albumAgg),
	}
	if cfg.Bot.LogFile != "" {
		b.rotateLog()
	}

	sessionPath := cfg.Bot.SessionFile
	if !strings.HasSuffix(sessionPath, ".db") {
		sessionPath += ".db"
	}
	// 确保数据目录存在（session / 下载目录）
	if err := os.MkdirAll(filepath.Dir(sessionPath), 0o755); err != nil {
		return nil, fmt.Errorf("创建 session 目录: %w", err)
	}
	if err := os.MkdirAll(cfg.Bot.DownloadDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建下载目录: %w", err)
	}

	// 代理配置（可选）
	opts := &gotgproto.ClientOpts{
		Session: sessionMaker.SqlSession(sqlite.Open(sessionPath)),
	}
	if cfg.Bot.Proxy != "" {
		dial, err := proxy.DialFunc(cfg.Bot.Proxy)
		if err != nil {
			return nil, fmt.Errorf("代理配置无效: %w", err)
		}
		opts.Resolver = dcs.Plain(dcs.PlainOptions{Dial: dial})
		b.logf("已配置代理: %s", maskProxy(cfg.Bot.Proxy))
	}

	client, err := gotgproto.NewClient(
		cfg.Secrets.APIID,
		cfg.Secrets.APIHash,
		gotgproto.ClientTypeBot(cfg.Secrets.BotToken),
		opts,
	)
	if err != nil {
		return nil, fmt.Errorf("创建 bot 客户端: %w", err)
	}
	b.client = client
	b.sharedCtx = client.CreateContext()
	client.Dispatcher.AddHandler(handlers.NewAnyUpdate(b.handleUpdate))
	return b, nil
}

// Run 启动并阻塞
func (b *Bot) Run(ctx context.Context) error {
	go b.timeoutLoop(ctx)
	go func() {
		<-ctx.Done()
		b.client.Stop()
	}()
	b.setCommands()
	b.logf("bot 已启动，等待转发消息...")
	return b.client.Idle()
}

// Close 释放资源
func (b *Bot) Close() {
	if b.client != nil {
		b.client.Stop()
	}
	b.logMu.Lock()
	if b.logFile != nil {
		b.logFile.Close()
	}
	b.logMu.Unlock()
}

// ---------- 更新分发 ----------

func (b *Bot) handleUpdate(ctx *ext.Context, update *ext.Update) error {
	if update.CallbackQuery != nil {
		return b.handleCallback(ctx, update)
	}
	if update.EffectiveMessage != nil {
		return b.handleMessage(ctx, update)
	}
	return nil
}

// ---------- 消息处理 ----------

func (b *Bot) handleMessage(ctx *ext.Context, update *ext.Update) error {
	msg := update.EffectiveMessage
	if msg == nil || msg.Out {
		return nil
	}
	chatID := peerID(msg.PeerID)
	if chatID == 0 {
		return nil
	}
	b.rememberChatName(chatID, update.Entities)
	b.logf("收到消息: chat=%d 媒体=%T 文本=%q", chatID, msg.Media, msg.Text)

	// 命令处理
	if strings.HasPrefix(msg.Text, "/") {
		return b.handleCommand(chatID, msg.Text)
	}

	// 改名输入
	if b.applyRenameInput(chatID, msg.Text) {
		return nil
	}

	// 相册消息：按 GroupedID 聚合
	if msg.GroupedID != 0 {
		b.addToAlbum(chatID, msg.Message)
		return nil
	}

	// 单条媒体
	items := extractItems(msg.Message)
	if len(items) == 0 {
		b.sendMsg(chatID, "❌ 请转发视频/图片/文件给我", nil)
		return nil
	}
	b.createTask(chatID, int64(msg.ID), &task.Media{Items: items, Caption: msg.Text})
	return nil
}

// ---------- 相册聚合 ----------

func (b *Bot) addToAlbum(chatID int64, msg *tg.Message) {
	b.albumMu.Lock()
	agg, ok := b.albums[msg.GroupedID]
	if !ok {
		agg = &albumAgg{groupID: msg.GroupedID, chatID: chatID}
		b.albums[msg.GroupedID] = agg
		agg.timer = time.AfterFunc(2*time.Second, func() { b.flushAlbum(msg.GroupedID) })
	}
	agg.msgs = append(agg.msgs, msg)
	b.albumMu.Unlock()
}

func (b *Bot) flushAlbum(groupID int64) {
	b.albumMu.Lock()
	agg, ok := b.albums[groupID]
	if ok {
		delete(b.albums, groupID)
	}
	b.albumMu.Unlock()
	if !ok {
		return
	}
	var items []*task.MediaItem
	caption := ""
	for _, m := range agg.msgs {
		items = append(items, extractItems(m)...)
		if caption == "" && m.Message != "" {
			caption = m.Message
		}
	}
	if len(items) == 0 {
		b.sendMsg(agg.chatID, "❌ 该相册没有可下载的媒体", nil)
		return
	}
	firstMsgID := int64(0)
	if len(agg.msgs) > 0 {
		firstMsgID = int64(agg.msgs[0].ID)
	}
	b.createTask(agg.chatID, firstMsgID, &task.Media{Items: items, Caption: caption})
}

// ---------- 任务创建与交互 ----------

// createTask 建任务并进入第一步交互
func (b *Bot) createTask(chatID, triggerMsgID int64, media *task.Media) {
	t := b.mgr.Create(chatID, triggerMsgID)
	b.mgr.Update(t.ID, func(tt *task.Task) { tt.Media = media })

	// 命名识别
	res := b.namer.Rename(media.Caption, firstRawName(media), "")
	b.mgr.Update(t.ID, func(tt *task.Task) {
		tt.DetectedName = res.Name
		tt.FinalName = res.Name
	})

	counts := media.Count()
	desc := describeMedia(counts)

	// 含图片：先选下载类型
	if media.HasKind(task.KindPhoto) {
		b.mgr.SetStatus(t.ID, task.StatusAwaitKind)
		txt := fmt.Sprintf("📥 收到任务 #%d\n\n%s\n\n请选择下载内容：", t.ID, desc)
		sent, err := b.sendMsg(chatID, txt, b.kindKeyboard(t.ID))
		if err == nil {
			b.mgr.Update(t.ID, func(tt *task.Task) { tt.PromptMsgID = int64(sent.ID) })
		}
		b.logf("任务 #%d 创建（含图片），等待选择类型", t.ID)
		return
	}

	b.askPath(t)
}

// askPath 路径选择
func (b *Bot) askPath(t *task.Task) {
	b.mgr.SetStatus(t.ID, task.StatusAwaitPath)
	txt := fmt.Sprintf("📍 请选择推送目标（任务 #%d）：", t.ID)
	if t.PromptMsgID != 0 {
		b.editMsg(t.ChatID, int(t.PromptMsgID), txt, b.pathKeyboard(t.ID))
	} else {
		sent, err := b.sendMsg(t.ChatID, txt, b.pathKeyboard(t.ID))
		if err == nil {
			b.mgr.Update(t.ID, func(tt *task.Task) { tt.PromptMsgID = int64(sent.ID) })
		}
	}
	b.logf("任务 #%d 等待选择路径", t.ID)
}

// askName 名称确认
func (b *Bot) askName(t *task.Task) {
	b.mgr.SetStatus(t.ID, task.StatusAwaitName)
	b.editMsg(t.ChatID, int(t.PromptMsgID),
		fmt.Sprintf("📝 识别名称：\n%s\n\n确认或修改：", t.DetectedName),
		b.nameKeyboard(t.ID))
}

// ---------- 回调处理 ----------

func (b *Bot) handleCallback(ctx *ext.Context, update *ext.Update) error {
	cb := update.CallbackQuery
	if cb == nil {
		return nil
	}
	chatID := peerID(cb.Peer)
	data := string(cb.Data)
	b.logf("收到回调: chat=%d data=%q", chatID, data)
	b.answerCallback(cb)

	parts := strings.Split(data, ":")
	if len(parts) < 3 || parts[0] != "t" {
		return nil
	}
	tid, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return nil
	}
	t := b.mgr.Get(tid)
	if t == nil {
		b.sendMsg(chatID, "⚠️ 任务不存在或已过期", nil)
		return nil
	}
	action := parts[2]

	switch action {
	case "cancel": // 取消任务
		b.cancelTask(t, "用户取消")

	case "kind": // 选择下载类型
		kind := task.MediaKind(parts[3])
		b.mgr.Update(tid, func(tt *task.Task) { tt.KindFilter = kind })
		label := kindLabel(kind)
		b.editMsg(chatID, int(t.PromptMsgID), fmt.Sprintf("🎯 下载内容：%s", label), nil)
		b.askPath(t)

	case "path": // 选择推送路径
		target := parts[3]
		b.mgr.Update(tid, func(tt *task.Task) { tt.Target = target })
		b.editMsg(chatID, int(t.PromptMsgID), fmt.Sprintf("📍 目标已选择：%s", target), nil)
		b.askName(t)
		b.logf("任务 #%d 已选路径 %s", tid, target)

	case "name": // 名称确认/修改
		switch parts[3] {
		case "ok":
			b.mgr.Update(tid, func(tt *task.Task) { tt.FinalName = tt.DetectedName })
			b.startDownload(t)
		case "edit":
			b.mgr.Update(tid, func(tt *task.Task) { tt.NameEditPending = true })
			b.editMsg(chatID, int(t.PromptMsgID),
				"✏️ 请直接发送新名称（不含扩展名；全部下载时为文件夹名），例如：\n权力的游戏.S03E05", nil)
		}
	}
	return nil
}

// ---------- 下载 + 推送 ----------

func (b *Bot) startDownload(t *task.Task) {
	items := filterItems(t.Media.Items, t.KindFilter)
	if len(items) == 0 {
		b.failTask(t, "没有符合所选类型的媒体")
		return
	}
	folderMode := len(items) > 1

	b.mgr.SetStatus(t.ID, task.StatusDownloading)
	dest := filepath.Join(b.cfg.Bot.DownloadDir, t.FinalName)
	if folderMode {
		b.editMsg(t.ChatID, int(t.PromptMsgID), fmt.Sprintf("⏳ 开始下载 #%d：%s（%d 个文件 → 文件夹）", t.ID, t.FinalName, len(items)), nil)
	} else {
		b.editMsg(t.ChatID, int(t.PromptMsgID), fmt.Sprintf("⏳ 开始下载 #%d：%s", t.ID, t.FinalName), nil)
	}

	go func() {
		b.mgr.Acquire()
		defer b.mgr.Release()

		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(b.cfg.Bot.DownloadTimeoutMin)*time.Minute)
		defer cancel()
		// 任务取消联动
		go func() {
			select {
			case <-t.Cancel:
				cancel()
			case <-ctx.Done():
			}
		}()

		var totalSize int64
		if folderMode {
			if err := os.MkdirAll(dest, 0o755); err != nil {
				b.failTask(t, fmt.Sprintf("创建文件夹失败：%v", err))
				return
			}
			for i, it := range items {
				name := itemFileName(it, i+1)
				n, err := b.downloadOne(ctx, it, filepath.Join(dest, name))
				if err != nil {
					b.failTask(t, fmt.Sprintf("下载 %s 失败：%v", name, err))
					return
				}
				totalSize += n
			}
		} else {
			n, err := b.downloadOne(ctx, items[0], dest)
			if err != nil {
				b.failTask(t, fmt.Sprintf("下载失败：%v", err))
				return
			}
			totalSize = n
		}

		// 推送
		b.mgr.SetStatus(t.ID, task.StatusPushing)
		target := b.router.Target(t.Target)
		if target == nil {
			b.failTask(t, fmt.Sprintf("目标 %q 不存在", t.Target))
			return
		}
		destDesc, err := b.push(ctx, t.Target, target, dest, t.FinalName, folderMode)
		if err != nil {
			b.failTask(t, fmt.Sprintf("推送失败：%v", err))
			return
		}

		b.mgr.SetStatus(t.ID, task.StatusDone)
		kindNote := ""
		if folderMode {
			kindNote = fmt.Sprintf("📂 %d 个文件\n", len(items))
		}
		b.sendMsg(t.ChatID, fmt.Sprintf("✅ 任务 #%d 完成\n%s📦 %s\n💾 %s\n📍 已推送至：%s",
			t.ID, kindNote, t.FinalName, humanSize(totalSize), destDesc), nil)
		_ = b.history.Append(history.Entry{
			Time:   history.Now(),
			TaskID: t.ID,
			Name:   t.FinalName,
			Size:   totalSize,
			Target: t.Target,
			Dest:   destDesc,
			Status: "done",
		})
		b.logf("任务 #%d 完成: %s -> %s", t.ID, t.FinalName, destDesc)
	}()
}

func (b *Bot) downloadOne(ctx context.Context, it *task.MediaItem, dest string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return 0, err
	}
	if it.Kind == task.KindPhoto {
		photo := &tg.Photo{
			ID:            it.DocID,
			AccessHash:    it.AccessHash,
			FileReference: it.FileRef,
		}
		return downloader.DownloadPhoto(ctx, b.client.API(), photo, dest)
	}
	doc := &tg.Document{
		ID:            it.DocID,
		AccessHash:    it.AccessHash,
		FileReference: it.FileRef,
		Size:          it.Size,
	}
	return downloader.Download(ctx, b.client.API(), doc, dest)
}

// push 推送（folderMode 时整体推送文件夹，保留文件夹名）
func (b *Bot) push(ctx context.Context, targetName string, target *config.Target, src, name string, folderMode bool) (string, error) {
	switch target.Type {
	case "local":
		if err := os.MkdirAll(target.Path, 0o755); err != nil {
			return "", err
		}
		dest := filepath.Join(target.Path, name)
		if err := os.Rename(src, dest); err != nil {
			return "", err
		}
		return dest, nil

	case "rsync":
		key := target.SSHKey
		if key == "" {
			key = b.cfg.Secrets.SSHKey
		}
		sshCmd := fmt.Sprintf("ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -p %d", target.Port)
		if key != "" {
			sshCmd = fmt.Sprintf("ssh -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -p %d", key, target.Port)
		}
		// 文件夹整体推送：源不带尾斜杠 → 目标保留文件夹名
		args := []string{"-avz", "--remove-source-files", "-e", sshCmd,
			src, fmt.Sprintf("%s@%s:%s", target.User, target.Host, target.Path)}

		var cmd *exec.Cmd
		if pw := b.cfg.Secrets.SSHPasswords[targetName]; pw != "" {
			full := append([]string{"-p", pw, "rsync"}, args...)
			cmd = exec.CommandContext(ctx, "sshpass", full...)
		} else {
			cmd = exec.CommandContext(ctx, "rsync", args...)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("rsync 失败: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		return fmt.Sprintf("%s@%s:%s/%s", target.User, target.Host, target.Path, name), nil
	}
	return "", fmt.Errorf("未知目标类型 %q", target.Type)
}

func (b *Bot) failTask(t *task.Task, reason string) {
	b.mgr.SetStatus(t.ID, task.StatusFailed)
	b.mgr.Update(t.ID, func(tt *task.Task) { tt.Err = fmt.Errorf("%s", reason) })
	b.sendMsg(t.ChatID, fmt.Sprintf("❌ 任务 #%d %s", t.ID, reason), nil)
	_ = b.history.Append(history.Entry{
		Time:   history.Now(),
		TaskID: t.ID,
		Name:   t.FinalName,
		Target: t.Target,
		Status: "failed",
	})
	b.logf("任务 #%d 失败: %s", t.ID, reason)
}

func (b *Bot) cancelTask(t *task.Task, reason string) {
	b.mgr.SetStatus(t.ID, task.StatusCancelled)
	select {
	case <-t.Cancel:
	default:
		close(t.Cancel)
	}
	b.editMsg(t.ChatID, int(t.PromptMsgID), fmt.Sprintf("❌ 任务 #%d 已取消（%s）", t.ID, reason), nil)
	_ = b.history.Append(history.Entry{
		Time:   history.Now(),
		TaskID: t.ID,
		Name:   t.FinalName,
		Target: t.Target,
		Status: "cancelled",
	})
	b.logf("任务 #%d 已取消", t.ID)
}

// ---------- 超时兜底 ----------

func (b *Bot) timeoutLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.rotateLog()
			for _, t := range b.mgr.ExpiredAwait() {
				switch t.Status {
				case task.StatusAwaitKind:
					// 类型超时：全部下载
					b.mgr.Update(t.ID, func(tt *task.Task) { tt.KindFilter = "" })
					b.editMsg(t.ChatID, int(t.PromptMsgID), "⏰ 未收到类型选择，按「全部下载」处理", nil)
					b.askPath(t)
				case task.StatusAwaitPath:
					// 路径超时：推送到默认目标
					def := b.router.DefaultTarget()
					b.mgr.Update(t.ID, func(tt *task.Task) { tt.Target = def })
					b.editMsg(t.ChatID, int(t.PromptMsgID), fmt.Sprintf("⏰ 未收到路径选择，自动推送至默认目标：%s", def), nil)
					b.askName(t)
					b.logf("任务 #%d 路径超时，默认 -> %s", t.ID, def)
				case task.StatusAwaitName:
					b.mgr.Update(t.ID, func(tt *task.Task) { tt.FinalName = tt.DetectedName })
					b.editMsg(t.ChatID, int(t.PromptMsgID), fmt.Sprintf("⏰ 未收到名称确认，使用识别名：%s，开始下载", t.DetectedName), nil)
					b.startDownload(t)
				}
			}
		}
	}
}

// ---------- 命令处理 ----------

func (b *Bot) handleCommand(chatID int64, text string) error {
	cmd := strings.TrimSpace(strings.SplitN(text, " ", 2)[0])
	cmd = strings.TrimPrefix(cmd, "/")
	if i := strings.Index(cmd, "@"); i >= 0 {
		cmd = cmd[:i] // 去掉 @botname
	}
	switch cmd {
	case "log":
		return b.cmdLog(chatID)
	case "tasks":
		return b.cmdTasks(chatID)
	case "all_tasks":
		return b.cmdAllTasks(chatID)
	case "history":
		return b.cmdHistory(chatID)
	case "clear_history":
		return b.cmdClearHistory(chatID)
	case "cleanup":
		return b.cmdCleanup(chatID)
	case "start", "help":
		b.sendMsg(chatID, "📥 转发视频/图片/文件给我即可自动下载\n\n命令：\n/log 查看当天日志\n/tasks 下载中任务\n/all_tasks 全部任务\n/history 下载历史\n/clear_history 清空历史\n/cleanup 清理缓存", nil)
	}
	return nil
}

func (b *Bot) cmdLog(chatID int64) error {
	b.logMu.Lock()
	path := b.logPath()
	b.logMu.Unlock()
	lines, err := readTail(path, 150)
	if err != nil {
		b.sendMsg(chatID, fmt.Sprintf("❌ 读取日志失败：%v", err), nil)
		return nil
	}
	if len(lines) == 0 {
		b.sendMsg(chatID, "📄 日志为空", nil)
		return nil
	}
	msg := "📄 当天日志（末尾）：\n```\n" + strings.Join(lines, "\n") + "\n```"
	b.sendMsg(chatID, truncateMsg(msg), nil)
	return nil
}

func (b *Bot) cmdTasks(chatID int64) error {
	active := b.mgr.Active()
	if len(active) == 0 {
		b.sendMsg(chatID, "⏳ 当前没有进行中的任务", nil)
		return nil
	}
	var sb strings.Builder
	sb.WriteString("⏳ 进行中任务：\n")
	for _, t := range active {
		fmt.Fprintf(&sb, "#%d [%s] %s\n", t.ID, t.Status.String(), t.FinalName)
	}
	b.sendMsg(chatID, strings.TrimSuffix(sb.String(), "\n"), nil)
	return nil
}

func (b *Bot) cmdAllTasks(chatID int64) error {
	all := b.mgr.All()
	if len(all) == 0 {
		b.sendMsg(chatID, "📋 暂无任务", nil)
		return nil
	}
	var sb strings.Builder
	sb.WriteString("📋 全部任务：\n")
	for _, t := range all {
		fmt.Fprintf(&sb, "#%d [%s] %s\n", t.ID, t.Status.String(), t.FinalName)
	}
	b.sendMsg(chatID, truncateMsg(strings.TrimSuffix(sb.String(), "\n")), nil)
	return nil
}

func (b *Bot) cmdHistory(chatID int64) error {
	entries, err := b.history.Recent(20)
	if err != nil {
		b.sendMsg(chatID, fmt.Sprintf("❌ 读取历史失败：%v", err), nil)
		return nil
	}
	if len(entries) == 0 {
		b.sendMsg(chatID, "📜 暂无下载历史", nil)
		return nil
	}
	var sb strings.Builder
	sb.WriteString("📜 下载历史（最近 20 条）：\n")
	for _, e := range entries {
		fmt.Fprintf(&sb, "%s #%d [%s] %s → %s\n", e.Time, e.TaskID, e.Status, e.Name, e.Target)
	}
	b.sendMsg(chatID, truncateMsg(strings.TrimSuffix(sb.String(), "\n")), nil)
	return nil
}

func (b *Bot) cmdClearHistory(chatID int64) error {
	if err := b.history.Clear(); err != nil {
		b.sendMsg(chatID, fmt.Sprintf("❌ 清空失败：%v", err), nil)
		return nil
	}
	b.sendMsg(chatID, "🗑 下载历史已清空（文件不受影响）", nil)
	return nil
}

func (b *Bot) cmdCleanup(chatID int64) error {
	dir := b.cfg.Bot.DownloadDir
	var removed []string
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if strings.HasSuffix(info.Name(), ".part") {
			os.Remove(path)
			removed = append(removed, info.Name())
		}
		return nil
	})
	if len(removed) == 0 {
		b.sendMsg(chatID, "🧹 缓存干净，没有可清理的残留", nil)
		return nil
	}
	b.sendMsg(chatID, fmt.Sprintf("🧹 已清理 %d 个残留文件：\n%s", len(removed), strings.Join(removed, "\n")), nil)
	return nil
}

// setCommands 注册中文命令菜单
func (b *Bot) setCommands() {
	req := &tg.BotsSetBotCommandsRequest{
		Scope:     &tg.BotCommandScopeDefault{},
		LangCode:  "zh",
		Commands: []tg.BotCommand{
			{Command: "log", Description: "查看日志（当天）"},
			{Command: "tasks", Description: "下载中任务"},
			{Command: "all_tasks", Description: "全部任务"},
			{Command: "history", Description: "下载历史"},
			{Command: "clear_history", Description: "清空下载历史"},
			{Command: "cleanup", Description: "清理缓存"},
		},
	}
	if _, err := b.client.API().BotsSetBotCommands(context.Background(), req); err != nil {
		b.logf("注册命令菜单失败: %v", err)
	} else {
		b.logf("命令菜单已注册")
	}
}

// ---------- 改名输入处理 ----------

func (b *Bot) applyRenameInput(chatID int64, text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	for _, t := range b.mgr.All() {
		if t.ChatID == chatID && t.NameEditPending {
			name := strings.TrimSpace(text)
			_, ext := naming.SplitExt(t.DetectedName)
			if ext == "" {
				ext = ".mp4"
			}
			final := name + ext
			b.mgr.Update(t.ID, func(tt *task.Task) { tt.FinalName = final; tt.NameEditPending = false })
			b.editMsg(t.ChatID, int(t.PromptMsgID), fmt.Sprintf("✏️ 名称已更新：%s\n开始下载...", final), nil)
			b.startDownload(t)
			return true
		}
	}
	return false
}

// ---------- 工具 ----------

func (b *Bot) rememberChatName(chatID int64, entities *tg.Entities) {
	if _, ok := b.chatNames[chatID]; ok {
		return
	}
	var name string
	if entities != nil {
		if c, ok := entities.Chats[chatID]; ok {
			name = c.GetTitle()
		} else if u, ok := entities.Users[chatID]; ok {
			if u.Username != "" {
				name = "@" + u.Username
			} else {
				name = strings.TrimSpace(u.FirstName + " " + u.LastName)
			}
		}
	}
	if name != "" {
		b.chatNames[chatID] = name
		b.logf("会话 %d 名称: %s", chatID, name)
	}
}

// sendMsg 发送文本消息（可带键盘）
func (b *Bot) sendMsg(chatID int64, text string, kbd tg.ReplyMarkupClass) (*types.Message, error) {
	req := &tg.MessagesSendMessageRequest{Message: text, NoWebpage: true}
	if kbd != nil {
		req.ReplyMarkup = kbd
	}
	return b.sharedCtx.SendMessage(chatID, req)
}

// editMsg 编辑提示消息；失败则重发一条
func (b *Bot) editMsg(chatID int64, msgID int, text string, kbd tg.ReplyMarkupClass) {
	req := &tg.MessagesEditMessageRequest{ID: msgID, Message: text}
	if kbd != nil {
		req.ReplyMarkup = kbd
	}
	if _, err := b.sharedCtx.EditMessage(chatID, req); err != nil {
		if sent, err2 := b.sendMsg(chatID, text, kbd); err2 == nil {
			for _, t := range b.mgr.All() {
				if t.ChatID == chatID && int(t.PromptMsgID) == msgID {
					b.mgr.Update(t.ID, func(tt *task.Task) { tt.PromptMsgID = int64(sent.ID) })
				}
			}
		}
	}
}

func (b *Bot) answerCallback(cb *tg.UpdateBotCallbackQuery) {
	_, _ = b.sharedCtx.AnswerCallback(&tg.MessagesSetBotCallbackAnswerRequest{
		QueryID: cb.QueryID,
		Alert:   false,
	})
}

// ---------- 键盘 ----------

// kindKeyboard 下载类型选择
func (b *Bot) kindKeyboard(tid int64) tg.ReplyMarkupClass {
	return &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{
		{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "🎬 仅视频", Data: []byte(fmt.Sprintf("t:%d:kind:video", tid))},
			&tg.KeyboardButtonCallback{Text: "🖼 仅图片", Data: []byte(fmt.Sprintf("t:%d:kind:photo", tid))},
		}},
		{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "📦 全部下载", Data: []byte(fmt.Sprintf("t:%d:kind:all", tid))},
			&tg.KeyboardButtonCallback{Text: "❌ 取消", Data: []byte(fmt.Sprintf("t:%d:cancel", tid))},
		}},
	}}
}

// pathKeyboard 路径选择键盘
func (b *Bot) pathKeyboard(tid int64) tg.ReplyMarkupClass {
	var rows []tg.KeyboardButtonRow
	for _, n := range b.cfg.TargetNames() {
		rows = append(rows, tg.KeyboardButtonRow{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "📁 " + n, Data: []byte(fmt.Sprintf("t:%d:path:%s", tid, n))},
		}})
	}
	rows = append(rows, tg.KeyboardButtonRow{Buttons: []tg.KeyboardButtonClass{
		&tg.KeyboardButtonCallback{Text: "❌ 取消", Data: []byte(fmt.Sprintf("t:%d:cancel", tid))},
	}})
	return &tg.ReplyInlineMarkup{Rows: rows}
}

// nameKeyboard 名称确认键盘
func (b *Bot) nameKeyboard(tid int64) tg.ReplyMarkupClass {
	return &tg.ReplyInlineMarkup{Rows: []tg.KeyboardButtonRow{
		{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "✅ 确认", Data: []byte(fmt.Sprintf("t:%d:name:ok", tid))},
			&tg.KeyboardButtonCallback{Text: "✏️ 改名", Data: []byte(fmt.Sprintf("t:%d:name:edit", tid))},
		}},
		{Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonCallback{Text: "❌ 取消", Data: []byte(fmt.Sprintf("t:%d:cancel", tid))},
		}},
	}}
}

// ---------- 日志（控制台 + 按天文件） ----------

func (b *Bot) logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("%s", msg)
	b.logMu.Lock()
	defer b.logMu.Unlock()
	if b.logFile == nil {
		return
	}
	fmt.Fprintf(b.logFile, "%s %s\n", time.Now().Format("2006/01/02 15:04:05"), msg)
}

func (b *Bot) logPath() string {
	if b.cfg.Bot.LogFile == "" {
		return ""
	}
	dir := filepath.Dir(b.cfg.Bot.LogFile)
	base := filepath.Base(b.cfg.Bot.LogFile)
	return filepath.Join(dir, time.Now().Format("2006-01-02")+"_"+base)
}

// rotateLog 按天切换日志文件
func (b *Bot) rotateLog() {
	if b.cfg.Bot.LogFile == "" {
		return
	}
	day := time.Now().Format("2006-01-02")
	b.logMu.Lock()
	defer b.logMu.Unlock()
	if b.logDay == day && b.logFile != nil {
		return
	}
	path := b.logPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	if b.logFile != nil {
		b.logFile.Close()
	}
	b.logFile = f
	b.logDay = day
}

// ---------- 纯工具 ----------

func peerID(p tg.PeerClass) int64 {
	switch v := p.(type) {
	case *tg.PeerUser:
		return v.UserID
	case *tg.PeerChat:
		return v.ChatID
	case *tg.PeerChannel:
		return v.ChannelID
	}
	return 0
}

// extractItems 从消息提取媒体项
func extractItems(msg *tg.Message) []*task.MediaItem {
	var items []*task.MediaItem
	switch md := msg.Media.(type) {
	case *tg.MessageMediaDocument:
		doc, ok := md.Document.(*tg.Document)
		if !ok {
			return nil
		}
		it := &task.MediaItem{
			Kind:       kindOfDoc(doc),
			DocID:      doc.ID,
			AccessHash: doc.AccessHash,
			FileRef:    doc.FileReference,
			Size:       doc.Size,
			MimeType:   doc.MimeType,
			RawName:    downloader.FileNameFromDoc(doc),
		}
		it.Ext = filepath.Ext(it.RawName)
		if it.Ext == "" {
			it.Ext = guessExt(it.MimeType)
		}
		items = append(items, it)
	case *tg.MessageMediaPhoto:
		photo, ok := md.Photo.(*tg.Photo)
		if !ok {
			return nil
		}
		items = append(items, &task.MediaItem{
			Kind:       task.KindPhoto,
			DocID:      photo.ID,
			AccessHash: photo.AccessHash,
			FileRef:    photo.FileReference,
			ThumbType:  downloader.PhotoThumbType(photo),
			Size:       photoSize(photo),
			Ext:        ".jpg",
		})
	}
	return items
}

func kindOfDoc(doc *tg.Document) task.MediaKind {
	for _, attr := range doc.Attributes {
		switch attr.(type) {
		case *tg.DocumentAttributeVideo:
			return task.KindVideo
		case *tg.DocumentAttributeAudio:
			return task.KindFile
		}
	}
	if strings.HasPrefix(doc.MimeType, "video/") {
		return task.KindVideo
	}
	return task.KindFile
}

func photoSize(p *tg.Photo) int64 {
	var best int
	for _, s := range p.Sizes {
		if ps, ok := s.(*tg.PhotoSize); ok {
			if ps.Size > best {
				best = ps.Size
			}
		}
	}
	return int64(best)
}

// filterItems 按类型过滤媒体
func filterItems(items []*task.MediaItem, kind task.MediaKind) []*task.MediaItem {
	if kind == "" || kind == "all" {
		return items
	}
	var out []*task.MediaItem
	for _, it := range items {
		if it.Kind == kind {
			out = append(out, it)
		}
	}
	return out
}

// itemFileName 文件夹模式下内部文件名
func itemFileName(it *task.MediaItem, idx int) string {
	if it.Kind == task.KindPhoto {
		return fmt.Sprintf("photo_%02d%s", idx, it.Ext)
	}
	if it.RawName != "" {
		return it.RawName
	}
	return fmt.Sprintf("file_%02d%s", idx, it.Ext)
}

func firstRawName(media *task.Media) string {
	for _, it := range media.Items {
		if it.RawName != "" {
			return it.RawName
		}
	}
	return ""
}

func describeMedia(counts map[task.MediaKind]int) string {
	var parts []string
	if v := counts[task.KindVideo]; v > 0 {
		parts = append(parts, fmt.Sprintf("🎬 视频 %d 个", v))
	}
	if p := counts[task.KindPhoto]; p > 0 {
		parts = append(parts, fmt.Sprintf("🖼 图片 %d 张", p))
	}
	if f := counts[task.KindFile]; f > 0 {
		parts = append(parts, fmt.Sprintf("📄 文件 %d 个", f))
	}
	return strings.Join(parts, " ")
}

func kindLabel(k task.MediaKind) string {
	switch k {
	case task.KindVideo:
		return "🎬 仅视频"
	case task.KindPhoto:
		return "🖼 仅图片"
	default:
		return "📦 全部下载"
	}
}

// readTail 读文件末尾 n 行
func readTail(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

// truncateMsg Telegram 消息长度限制 ~4096，截断
func truncateMsg(s string) string {
	const max = 3900
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n...(截断)"
}

func guessExt(mime string) string {
	switch mime {
	case "video/mp4":
		return ".mp4"
	case "video/x-matroska":
		return ".mkv"
	case "video/webm":
		return ".webm"
	case "video/quicktime":
		return ".mov"
	case "application/pdf":
		return ".pdf"
	case "application/zip":
		return ".zip"
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	default:
		return ".mp4"
	}
}

func sourceLabel(s naming.Source) string {
	switch s {
	case naming.SourceCaptionCmd:
		return "caption 指令"
	case naming.SourceCaptionPattern:
		return "caption 识别"
	case naming.SourceFileName:
		return "文件名识别"
	default:
		return "兜底命名"
	}
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// maskProxy 脱敏显示代理地址（隐藏认证信息）
func maskProxy(p string) string {
	u, err := url.Parse(p)
	if err != nil || u.User == nil {
		return p
	}
	u.User = url.User(u.User.Username())
	return u.String()
}

