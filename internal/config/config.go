package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen string `yaml:"listen"`

	Telegram struct {
		Token            string `yaml:"token"`
		ChatID           int64  `yaml:"chat_id"`
		AllowedUserIDs   []int64 `yaml:"allowed_user_ids"`
		DefaultTimeoutS  int    `yaml:"default_timeout_seconds"`
	} `yaml:"telegram"`

	Storage struct {
		SQLitePath string `yaml:"sqlite_path"`
	} `yaml:"storage"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	// Env expansion for token
	c.Telegram.Token = os.ExpandEnv(c.Telegram.Token)
	if c.Telegram.Token == "" || strings.Contains(c.Telegram.Token, "$") {
		return nil, fmt.Errorf("telegram.token missing (set env var)")
	}
	if c.Telegram.ChatID == 0 {
		return nil, fmt.Errorf("telegram.chat_id missing")
	}
	if c.Listen == "" {
		c.Listen = ":8765"
	}
	if c.Telegram.DefaultTimeoutS == 0 {
		c.Telegram.DefaultTimeoutS = 1800 // 30 min
	}
	return &c, nil
}