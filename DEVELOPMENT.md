# tg-download-bot 开发文档

面向开发者：架构、开发/正式切换流程、构建发布、踩坑记录。

- 仓库：https://github.com/db-root/tg-download-bot
- 镜像：`bin12121/tg-download-bot:latest`（Docker Hub）
- 用户文档：见 `README.md`（使用）与 `DEPLOY.md`（部署）

## 技术栈

- Go 1.24+（本机 1.26 亦可编译；Docker 构建固定 golang:1.24-alpine）
- `gotgproto v1.0.0-beta22`（封装 gotd/td，MTProto 客户端）
- `glebarez/sqlite`（纯 Go SQLite，session 持久化，CGO_ENABLED=0 可编译）
- `gopkg.in/yaml.v3`（配置加载）
- Docker 运行镜像：alpine + rsync/sshpass/openssh-client/tzdata

## 目录结构

```
cmd/tg-download-bot/   入口（flag: -config 指定配置路径）
internal/
  bot/                bot 接入 + 交互流程 + 任务流转 + 命令 + 自动清理
  task/               任务状态机（Extracting→AwaitKind→AwaitPath→AwaitName→Downloading→Pushing→Done/Failed/Cancelled）、并发信号量、相册聚合
  naming/             命名引擎（caption 指令 > caption 模式 > 文件名清洗 > 日期兜底）
  router/             目标查询 + default_target 兜底
  downloader/         MTProto 下载（文档 InputDocumentFileLocation / 图片 InputPhotoFileLocation）
  history/            下载历史 jsonl 持久化（追加写 + Trim 裁剪）
  proxy/              HTTP CONNECT 手写 + SOCKS5（x/net/proxy）
  config/             config.yaml + secrets.yaml 双文件加载（secrets 独立防误提交）
```

## 开发 / 正式切换流程（重要）

本机开发用本地编译二进制，正式运行用 Docker Compose（同一份 `config.yaml` / `secrets.yaml` / `data/`，session 登录态共用，互不冲突）。

```bash
# ── 开发验证（迭代时）──
docker compose down        # 1. 停正式版容器
./tg-download-bot          # 2. 跑本地编译的开发版（改代码后先 go build）
# ...验证新功能，日志 /tmp/tgmb.log + data/logs/...

# ── 验证完回到正式版 ──
# 1. 停掉本地进程（Ctrl+C 或 kill $(pgrep -x tg-download-bot)）
# 2. 回到正式
docker compose up -d       # 拉 bin12121/tg-download-bot:latest 启动
```

> ⚠️ 本地进程与容器**不能同时跑**（两条 MTProto 长连接互抢消息，被踢下线）。

改配置：编辑 `config.yaml` / `secrets.yaml` → `docker compose restart`（容器只读挂载）。

## 构建

```bash
# 本地编译（开发用）
go build -o tg-download-bot ./cmd/tg-download-bot

# Docker 镜像（发布用，需能访问镜像源）
docker build -t bin12121/tg-download-bot:0.1.0 .
docker tag bin12121/tg-download-bot:0.1.0 bin12121/tg-download-bot:latest
```

## 发布流程（发版清单）

1. 代码验证通过（本地开发流程）
2. `git add -A && git commit`（等用户确认提交习惯）
3. 打 tag：`git tag v0.1.0 && git push origin v0.1.0`
4. 构建镜像 + 推 Docker Hub：
   ```bash
   docker build -t bin12121/tg-download-bot:<版本> .
   docker tag bin12121/tg-download-bot:<版本> bin12121/tg-download-bot:latest
   docker push bin12121/tg-download-bot:<版本>
   docker push bin12121/tg-download-bot:latest
   ```
5. 同步 `release/v0.1.0/` 交付物（tar.gz 重新 `docker save` 导出）
6. 同步 README.md / DEPLOY.md / DEVELOPMENT.md 中的版本与链接
7. 切回正式版容器：`docker compose up -d`

## 关键架构决策 / 踩坑记录

1. **20MB 限制**：HTTP Bot API 下载上限 20MB → 必须 MTProto（gotd/td）。需要 api_id+api_hash+bot_token 三件套（my.telegram.org 需纯净境外 IP，见 DEPLOY.md）
2. **gotgproto v1.0.0-beta22 API**：
   - `gotgproto.ClientTypeBot(token)`（不是 ClientType{BotToken}）
   - `ClientOpts.Session = sessionMaker.SqlSession(sqlite.Open(path))`（data 目录必须先建，否则 sqlite 报 out of memory(14)）
   - handler：`client.Dispatcher.AddHandler(handlers.NewAnyUpdate(fn))`；消息在 `update.EffectiveMessage`
   - goroutine 里发消息用 `client.CreateContext()` 复用（sharedCtx.SendMessage/EditMessage）
3. **回调数据解析**：按钮 data `t:1:name:ok` 按 `:` 分割，注意 case 匹配层级（parts[2]=action，parts[3]=子操作）
4. **tg v0.132 Peer 无 AccessHash**：AccessHash 只在 InputPeer 里 → 用 gotgproto chatID 直传，不构造 InputPeer
5. **downloader API**：`d.Download(api, loc).WithThreads(4).ToPath(ctx, dest)`（不是旧版 Stream）
6. **代理**：MTProto TCP 长连接不吃 HTTP_PROXY 环境变量 → `ClientOpts.Resolver = dcs.Plain(PlainOptions{Dial: 代理拨号})`；HTTP 代理手写 CONNECT（bufio.ReadResponse），SOCKS5 用 x/net/proxy
7. **rsync 密码认证**：rsync 不支持密码参数 → `sshpass -p` 包装；ssh 加 `-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null`；推送用 `--remove-source-files`（只删文件不删目录 → 任务结束自动删空文件夹壳）
8. **相册聚合**：GroupedID != 0 挂起 2s 聚齐再建任务（timer goroutine 无 ext.Context → 统一 sharedCtx）
9. **BotsSetBotCommands** 注册中文命令菜单（客户端缓存，重开聊天/重启客户端才显示）
10. **按天日志**：log 包输出控制台 + 自写文件（rotateLog 按天切换）；/log 命令读当天文件末尾
11. **data 自动清理**（启动时 + 每天一次，配置项 log_keep_days/history_keep/part_age_hours）：空文件夹壳、失败残留、超龄 .part、旧日志、历史裁剪；session.db 不清（删了要重登）
12. **Docker 构建**：CGO_ENABLED=0（glebarez/sqlite 纯 Go）；.dockerignore 排除 data/config/secrets

## rsync 推送链路（容器版）

容器内 rsync → ssh（密钥认证）→ 远端目标（即使目标是宿主机自己也不用挂载目录，rsync 走 ssh 协议由远端进程写盘）。

- **密钥来源**：`docker-compose.yaml` 挂载 `./ssh:/root/.ssh:ro`（部署目录下放 id_rsa + id_rsa.pub，权限 600；本地仓库的 `ssh/` 被 .gitignore 忽略，绝不提交）
- **配置**：`secrets.yaml` 的 `ssh_key: /root/.ssh/id_rsa`（容器内路径），或 targets 里按目标覆盖
- ⚠️ **大坑**：开发机连服务器一直走本机 `~/.ssh/id_rsa` 密钥认证，`ssh_passwords` 里配的密码其实是**无效的**（密钥优先，密码从没被用过）→ 容器里没有密钥时密码认证必然失败，**rsync 推送静默不可用**。症状：`Permission denied`。修法：挂载密钥/配 ssh_key，不要依赖密码

## dbroot 生产部署（增量更新原则）

dbroot（`<NAS_HOST>`，部署目录 `<DEPLOY_DIR>/tg-download-bot/`）是**正式上线环境**，已包含用户配置/密钥/data（session 登录态、历史、日志）。

**更新版本时只增量，绝不覆盖配置与数据**：
- ❌ 不推送/不覆盖：`config.yaml`、`secrets.yaml`、`data/`（含 session.db 登录态！覆盖会导致重新登录）
- ✅ 只更新：镜像（`docker compose pull && docker compose up -d`，compose 里 image 指向 `bin12121/tg-download-bot:latest`，拉新镜像自动重建）
- 文档（README/DEPLOY/DEVELOPMENT）可按需推送，不影响运行
- 验证：`docker logs tg-download-bot | tail` 看到"bot 已启动"即正常

## 配置文件同步（本地开发 ↔ 生产，只增量不覆盖）

本地开发目录的 `config.yaml` 与生产部署目录的 `config.yaml` **各自独立维护，曾发生分叉**（生产后来加过 target、改过路径，本地不知道）。

**权威基准 = 生产运行中的 config.yaml**（用户实际在用的配置）。

### 同步流程（配置变更 / 发版前检查）

1. **拉生产配置到本地做对比**：
   ```bash
   scp root@<NAS_HOST>:<DEPLOY_DIR>/config.yaml /tmp/tgb-prod-current.yaml
   diff config.yaml /tmp/tgb-prod-current.yaml   # 看分叉
   ```
2. **列差异给用户确认**：分三类——① 生产有本地缺（新增 target）② 路径不一致（以生产为准，验证目录存在：`ssh ... "ls -d <path>"`）③ 一致不用动。
3. **用户确认后只在差异项上增量修改本地**（补 target 块 / 改 path），**绝不整体覆盖**。
4. **校验**：YAML 解析通过 + `diff`（去注释）与生产一致。
5. 若反向（生产需要改）：先在远端 `cp config.yaml config.yaml.bak-<版本>` 备份，再增量改，校验后 `docker compose restart`。

### secrets.yaml

- 生产/本地结构一致（api_id/api_hash/bot_token/ssh_key）；只在生产维护实际值，本地可留同结构副本
- **永不进 git、永不进发布产物**；对比时只看键结构不看值

### 下次发版时

本地 config 与生产对齐后：发版流程照常（镜像+文档），配置若本轮没变更就**不推**；有变更就按上面 1→4 步增量同步，同步完再重启容器。配置文件变更建议顺带更新 `config.example.yaml`（进 git 的模板）保持一致。

## 已知坑：MESSAGE_EMPTY（/log 无反馈）

**症状**：/log 命令收到消息但无任何回复；其他短命令（/tasks 等）正常。
**根因**：Telegram 服务端对纯文本消息做 markdown 预处理。日志内容里的 `*`（如 `*tg.MessageMediaDocument`）、`_`、`` ` `` 未闭合 → 实体解析异常 → 服务端报 `rpc error 400: MESSAGE_EMPTY`（消息被判空），sendMsg 错误被静默吞掉。
**修复**：`cleanLogLines` 把 `*`→`·`、`_`→`-`、`` ` ``→`'` 后再发送；sendMsg/editMsg 失败现在会 logf 错误（v0.1.1）。
**教训**：所有 sendMsg 的错误都要记录，否则此类问题不可见。

## 安全红线

- 仓库是 **public**：代码/文档**严禁出现**真实 IP、内网路径、密钥、token、密码
- `secrets.yaml` / `config.yaml`（本地实际值）git 忽略，永不提交
- 示例文件（config.example.yaml / secrets.example.yaml）只放示例值
- git 历史重写过敏感信息（filter-branch），**不要再把真实值提交进历史**
