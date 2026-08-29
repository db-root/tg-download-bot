# tg-download-bot

Telegram 媒体下载机器人：把群里的视频/图片/文件**转发给 bot**，交互式确认（下载类型 + 推送路径 + 文件名）后自动下载，并推送到本机目录或远端服务器（rsync，支持密码/密钥认证）。

基于 Go + [gotgproto](https://github.com/celestix/gotgproto)（gotd/td MTProto），**无 20MB 下载限制**，支持 HTTP/SOCKS5 代理。

## 使用流程

```
① 转发视频/图片/文件（相册自动聚合）给 bot
② 含图片 → 选下载类型：[🎬仅视频] [🖼仅图片] [📦全部下载]
③ 选推送目标：[📁 飞机] [📁 电视剧] ...
④ 确认/修改名称：[✅确认] [✏️改名]
⑤ 自动下载（后台队列，并发可配）→ 自动推送 → 完成通知
```

- **全部下载** → 自动建文件夹（文件夹名=识别名），视频图片整体推送（目标端保留文件夹名）
- **任何步骤可点 [❌取消]**，下载中取消立即中断
- **超时兜底**：路径选择超时 → 推默认目标；名称超时 → 用识别名

## 命令（中文菜单已自动注册）

```
/log           查看当天日志（末尾150行）
/tasks         下载中任务
/all_tasks     全部任务
/history       下载历史
/clear_history 清空下载历史（不动文件）
/cleanup       清理缓存（.part残留）
```

## 配置

两个 YAML（密钥分离，均不入 git）：

- **config.example.yaml** → 复制为 `config.yaml`：目标/命名规则/超时/代理等
- **secrets.yaml**：api_id / api_hash / bot_token / ssh_passwords

```bash
cp config.example.yaml config.yaml
vi config.yaml     # 改目标目录、代理等
vi secrets.yaml    # 填 api_id/api_hash/bot_token/SSH密码
```

### 配置项说明

| 项 | 说明 |
|---|---|
| `bot.proxy` | 代理，如 `http://127.0.0.1:7890` / `socks5://...`，留空直连 |
| `bot.max_concurrent_downloads` | 同时下载数（防风控，默认 2） |
| `bot.path_timeout_min` / `name_timeout_min` | 交互超时（分钟） |
| `targets.*` | 推送目标：`local`=本机移动，`rsync`=远端服务器（host/user/path/port） |
| `secrets.ssh_passwords` | 目标名 → SSH 密码（有密码用 sshpass，有 ssh_key 用密钥） |
| `default_target` | 路径选择超时的兜底目标 |
| `rename.*` | 命名规则：集数模式正则+模板、清洗垃圾、质量标签、兜底策略 |

### 命名规则（多源提取）

1. caption 显式指令：`#重命名 我的剧名` / `重命名: xxx`
2. caption 模式匹配：`权力的游戏 S03E05`、`繁花 第12集`
3. 原始文件名清洗 + 模式匹配
4. 兜底：日期前缀 + 清洗后文件名

## 运行

### 本地直接运行

```bash
go build -o tg-download-bot ./cmd/tg-download-bot
./tg-download-bot                        # 默认读 ./config.yaml
./tg-download-bot -config /path/config.yaml
```

### Docker 部署（推荐）

```bash
# 1. 准备配置（首次）
cp config.example.yaml config.yaml
vi config.yaml && vi secrets.yaml

# 2. 构建 + 启动
docker compose up -d --build

# 3. 查看日志
docker compose logs -f
```

- 数据（session/下载/日志/历史）挂载在 `./data`
- 配置只读挂载，改配置后 `docker compose restart`
- 数据（session/下载/日志/历史）挂载在 `./data`
- 配置只读挂载，改配置后 `docker compose restart`
- 镜像已含 rsync/sshpass/ssh，远端推送开箱即用

> ⚠️ **本地运行与 Docker 二选一**：两种方式都是 bot 长连接，同时跑会互相抢占消息（被踢下线）。用 Docker 前先停掉本地进程（`pkill -x tg-download-bot`）。

## 自动清理（data 目录）

程序启动时 + 每天自动执行一次，无需手动干预：

| 内容 | 策略 |
|---|---|
| 推送后的空文件夹 | 完成后自动删除（rsync `--remove-source-files` 只删文件不删目录的补漏） |
| 失败任务残留 | 未推送出去的文件/文件夹自动删除 |
| 中断下载的 `.part` | 超过 `part_age_hours`（默认 24h）自动删除 |
| 按天日志 | 超过 `log_keep_days`（默认 7 天）自动删除 |
| 下载历史 | 超过 `history_keep`（默认 500 条）自动裁剪 |

`session.db`（登录态）不会清理，删了需重新登录。

## 目录结构

```
cmd/tg-download-bot/   入口
internal/
  config/           YAML 配置加载（config + secrets 分离）
  bot/              bot 接入 + 交互流程 + 任务流转 + 命令
  task/             任务状态机（含取消/相册聚合）
  naming/           命名引擎
  router/           目标查询
  downloader/       MTProto 下载（文档/图片）
  history/          下载历史持久化
  proxy/            代理拨号（HTTP CONNECT / SOCKS5）
Dockerfile / docker-compose.yaml   容器部署
```

## 已知边界

- 图片消息支持单图/相册；文件/视频单条或相册内混合
- 受保护内容（群禁转发）Telegram 层面拦截，无法下载
- bot 账号仅处理转发消息，风险低；建议保持并发限制默认值
