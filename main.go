package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"gopkg.in/yaml.v3"
)

type MatrixConfig struct {
	Homeserver  string `yaml:"homeserver"`
	UserID      string `yaml:"user_id"`
	AccessToken string `yaml:"access_token"`
	Username    string `yaml:"username"`
	Password    string `yaml:"password"`
	RoomID      string `yaml:"room_id"`
}

type QBittorrentConfig struct {
	URL          string `yaml:"url"`
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
	PollInterval int    `yaml:"poll_interval"`
	// HTTPUsername/HTTPPassword are for HTTP Basic Auth on the reverse proxy
	// sitting in front of qBittorrent (e.g. nginx, openresty). Leave empty if
	// qBittorrent is accessed directly.
	HTTPUsername string `yaml:"http_username"`
	HTTPPassword string `yaml:"http_password"`
}

type Config struct {
	Matrix      MatrixConfig      `yaml:"matrix"`
	QBittorrent QBittorrentConfig `yaml:"qbittorrent"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.QBittorrent.PollInterval <= 0 {
		cfg.QBittorrent.PollInterval = 30
	}
	return &cfg, nil
}

func main() {
	cfgPath := "config.yaml"
	if len(os.Args) > 1 {
		cfgPath = os.Args[1]
	}

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}

	bot, err := NewMatrixBot(cfg.Matrix)
	if err != nil {
		log.Fatalf("creating matrix bot: %v", err)
	}

	monitor, err := NewMonitor(cfg.QBittorrent, bot)
	if err != nil {
		log.Fatalf("creating qbittorrent monitor: %v", err)
	}

	monitor.RegisterCommands(bot)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go monitor.Start(ctx)

	log.Println("Bot started")
	bot.Start(ctx)
	log.Println("Bot stopped")
}
