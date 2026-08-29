package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"tg-download-bot/internal/bot"
	"tg-download-bot/internal/config"
)

func main() {
	configPath := flag.String("config", "config.yaml", "配置文件路径（与 secrets.yaml 同目录）")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("配置加载失败: %v", err)
	}
	if cfg.Secrets.BotToken == "" || strings.Contains(cfg.Secrets.BotToken, "…") {
		log.Fatalf("secrets.yaml 中 bot_token 缺失或不完整（完整格式: 数字:字母数字）")
	}
	if cfg.Secrets.APIID == 0 || cfg.Secrets.APIHash == "" {
		log.Fatalf("secrets.yaml 中 api_id/api_hash 缺失")
	}

	b, err := bot.New(cfg)
	if err != nil {
		log.Fatalf("bot 初始化失败: %v", err)
	}
	defer b.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := b.Run(ctx); err != nil {
		log.Fatalf("bot 运行失败: %v", err)
	}
	log.Println("bot 已退出")
}
