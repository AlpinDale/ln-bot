// Package config loads and validates the bot configuration from a YAML
// file, with secrets supplied via environment variables.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration for the bot.
type Config struct {
	Discord  Discord                 `yaml:"discord"`
	Schedule Schedule                `yaml:"schedule"`
	Announce Announce                `yaml:"announce"`
	Database Database                `yaml:"database"`
	HTTP     HTTP                    `yaml:"http"`
	Sources  map[string]SourceConfig `yaml:"sources"`
}

type Discord struct {
	// Token is read from LNBOT_DISCORD_TOKEN, never from the file.
	Token          string   `yaml:"-"`
	GuildID        string   `yaml:"guild_id"`
	AlertChannelID string   `yaml:"alert_channel_id"`
	AdminIDs       []string `yaml:"admin_ids"`
}

type Schedule struct {
	Cron     string `yaml:"cron"`
	Timezone string `yaml:"timezone"`
}

type Announce struct {
	LookbackDays int `yaml:"lookback_days"`
}

type Database struct {
	Path string `yaml:"path"`
}

type HTTP struct {
	UserAgent      string `yaml:"user_agent"`
	MinDelayMS     int    `yaml:"min_delay_ms"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

// SourceConfig holds per-source settings. Extra keys are preserved so
// individual plugins can define their own options later.
type SourceConfig struct {
	Enabled bool `yaml:"enabled"`
}

// Load reads the YAML file at path, applies environment overrides and
// defaults, and validates the result.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.applyDefaults()
	cfg.Discord.Token = os.Getenv("LNBOT_DISCORD_TOKEN")
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Schedule.Cron == "" {
		c.Schedule.Cron = "0 9 * * *"
	}
	if c.Schedule.Timezone == "" {
		c.Schedule.Timezone = "UTC"
	}
	if c.Announce.LookbackDays <= 0 {
		c.Announce.LookbackDays = 3
	}
	if c.Database.Path == "" {
		c.Database.Path = "data/lnbot.db"
	}
	if c.HTTP.UserAgent == "" {
		c.HTTP.UserAgent = "ln-release-bot/1.0 (personal release tracker)"
	}
	if c.HTTP.MinDelayMS <= 0 {
		c.HTTP.MinDelayMS = 1500
	}
	if c.HTTP.TimeoutSeconds <= 0 {
		c.HTTP.TimeoutSeconds = 30
	}
	if c.Sources == nil {
		c.Sources = map[string]SourceConfig{}
	}
}

func (c *Config) validate() error {
	if c.Discord.Token == "" {
		return fmt.Errorf("LNBOT_DISCORD_TOKEN environment variable is required")
	}
	if c.Discord.GuildID == "" {
		return fmt.Errorf("discord.guild_id is required")
	}
	if c.Discord.AlertChannelID == "" {
		return fmt.Errorf("discord.alert_channel_id is required")
	}
	if _, err := time.LoadLocation(c.Schedule.Timezone); err != nil {
		return fmt.Errorf("schedule.timezone: %w", err)
	}
	return nil
}

// Location returns the configured timezone. validate() guarantees it parses.
func (c *Config) Location() *time.Location {
	loc, _ := time.LoadLocation(c.Schedule.Timezone)
	return loc
}

// SourceEnabled reports whether the named source is enabled in config.
// Sources absent from the config are disabled.
func (c *Config) SourceEnabled(name string) bool {
	sc, ok := c.Sources[name]
	return ok && sc.Enabled
}
