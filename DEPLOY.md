# tg-download-bot 部署说明

Telegram 媒体下载机器人：转发视频/图片/文件给 bot，交互确认后自动下载并推送到本机或远端服务器。

- **GitHub**: https://github.com/db-root/tg-download-bot
- **Docker Hub 镜像**: `bin12121/tg-download-bot:latest`（含 rsync/sshpass/ssh，开箱即用）

## 仓库结构

```
tg-download-bot/
├── docker-compose.yaml      Docker Compose 编排（默认从 Docker Hub 拉镜像）
├── Dockerfile               镜像构建文件（改代码后本地构建用）
├── config.example.yaml      主配置模板（复制为 config.yaml）
├── secrets.example.yaml     密钥配置模板（复制为 secrets.yaml）
├── DEPLOY.md                本文档
└── README.md                完整帮助文档
```

## 快速开始（推荐：Docker 在线拉取）

```bash
# 1. 克隆仓库（或只取下面两个配置模板）
git clone https://github.com/db-root/tg-download-bot.git
cd tg-download-bot

# 2. 准备两个配置文件
cp config.example.yaml config.yaml      # 改推送目标/代理等
cp secrets.example.yaml secrets.yaml    # 填密钥（见下方"密钥获取"）

# 3. 启动（自动从 Docker Hub 拉取 bin12121/tg-download-bot:latest）
docker compose up -d

# 4. 查看日志
docker compose logs -f
```

> 配置目录结构：`config.yaml`、`secrets.yaml`、`data/`（运行数据）与 `docker-compose.yaml` 放同一目录。
> 本地直接运行（免 Docker）：`go build -o tg-download-bot ./cmd/tg-download-bot && ./tg-download-bot`

## 离线部署（无外网/内网环境）

1. 从 release 获取镜像包 `tg-download-bot-0.1.0.tar.gz`
2. 导入镜像：`docker load < tg-download-bot-0.1.0.tar.gz`
3. 在 `docker-compose.yaml` 中把 `image: bin12121/tg-download-bot:latest` 改为 `image: tg-download-bot:0.1.0`（或去掉 build 段注释本地构建）
4. 其余步骤同"快速开始"

## 密钥获取（secrets.yaml 三个必填项）

### 1. api_id / api_hash
```
登录 https://my.telegram.org → API development tools → Create new application
表单随意填（URL 可留空，Platform 选 Desktop）
创建后得到：api_id（数字）+ api_hash（32位十六进制）
```

> ⚠️⚠️ **特别声明：my.telegram.org 需要纯净的境外 IP 访问**
> - 国内 IP / 污染的代理出口 / 虚拟号账号都会被拒绝，仅报通用 `ERROR`
> - **测试 IP 纯净度：https://ipdata.co/**（查看网络类型、威胁等级）
> - 报 ERROR 排查顺序：① 换纯净境外 IP ② 完成手机号验证码 ③ 账号非虚拟号 ④ 等风控冷却

### 2. bot_token
```
Telegram 内搜索 @BotFather → /newbot → 设置名称和用户名（用户名以 bot 结尾）
得到形如：123456789:AAE...（完整 token）
```

### 3. SSH 密码（可选）
`ssh_passwords` 里键名对应 `config.yaml` 的 `targets` 目标名，值为目标服务器 SSH 密码（rsync 推送用）。也可改用 SSH 密钥（`ssh_key` 填绝对路径，优先级高于密码）。

## config.yaml 关键配置

```yaml
bot:
  proxy: "http://127.0.0.1:7890"   # 代理（MTProto 必须显式配置，支持 http/socks5），留空直连
  max_concurrent_downloads: 2       # 并发下载数（防风控）
  path_timeout_min: 5               # 选路径超时（超时推 default_target）
  name_timeout_min: 5               # 确认名称超时

targets:                            # 推送目标
  我的服务器:
    type: rsync                     # local=本机 | rsync=远端
    host: "你的服务器IP"
    port: 22
    user: root
    path: /你的保存目录
    ssh_key: ""                     # 留空则用 secrets 里的 ssh_passwords

default_target: 我的服务器           # 路径选择超时的兜底目标

rename:                             # 命名规则（正则+模板，详见 README）
```

## 使用

- 转发媒体给 bot → 按按钮操作（含图片先选类型，任意步骤可 ❌取消）
- 「全部下载」→ 自动建文件夹整体推送（目标端保留文件夹名）
- 中文命令：`/log` `/tasks` `/all_tasks` `/history` `/clear_history` `/cleanup`

## 常见问题

| 问题 | 处理 |
|---|---|
| 容器起不来 / 提示 token 无效 | 检查 secrets.yaml 三件套是否完整（api_id 数字、api_hash 32位、token 完整无省略号） |
| my.telegram.org 报 ERROR | 见上方"特别声明"：换纯净境外 IP |
| 下载大文件失败 | 确认走 MTProto（本程序已内置）；检查代理连通性 |
| rsync 推送失败 | 确认 22 端口可达、密码/密钥正确；日志 `docker compose logs` 有详细报错 |
