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

type Database struct {
	Path string `yaml:"path"`
}

type HTTP struct {
	UserAgent      string `yaml:"user_agent"`
	MinDelayMS     int    `yaml:"min_delay_ms"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
	// HostDelayMS raises the delay for specific hosts (robots.txt
	// Crawl-delay compliance). Keys are bare hostnames without "www.".
	HostDelayMS map[string]int `yaml:"host_delay_ms"`
	// BrowserTLSHosts request a browser-shaped TLS handshake for hosts
	// whose Cloudflare protection gates on JA3 fingerprint.
	BrowserTLSHosts []string `yaml:"browser_tls_hosts"`
	// FlareSolverrURL points at a FlareSolverr instance for solving
	// Cloudflare managed challenges. Overridable via LNBOT_FLARESOLVERR_URL.
	FlareSolverrURL string `yaml:"flaresolverr_url"`
	// FlareSolverrHosts route through FlareSolverr when its URL is set.
	FlareSolverrHosts []string `yaml:"flaresolverr_hosts"`
}

// SourceConfig holds per-source settings. Extra keys are preserved so
// individual plugins can define their own options later.
type SourceConfig struct {
	Enabled bool `yaml:"enabled"`
}

// Load reads the YAML file at path, applies environment overrides and
// defaults, and validates the non-Discord parts. Callers that connect
// to Discord must also call ValidateDiscord.
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
	if v := os.Getenv("LNBOT_FLARESOLVERR_URL"); v != "" {
		cfg.HTTP.FlareSolverrURL = v
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ValidateDiscord checks the fields required to run the Discord bot.
// Kept separate so scrape-only modes (-oneshot) work without a token.
func (c *Config) ValidateDiscord() error {
	if c.Discord.Token == "" {
		return fmt.Errorf("LNBOT_DISCORD_TOKEN environment variable is required")
	}
	if c.Discord.GuildID == "" {
		return fmt.Errorf("discord.guild_id is required")
	}
	if c.Discord.AlertChannelID == "" {
		return fmt.Errorf("discord.alert_channel_id is required")
	}
	return nil
}

func (c *Config) applyDefaults() {
	if c.Schedule.Cron == "" {
		c.Schedule.Cron = "0 9 * * *"
	}
	if c.Schedule.Timezone == "" {
		c.Schedule.Timezone = "UTC"
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
	if c.HTTP.HostDelayMS == nil {
		c.HTTP.HostDelayMS = map[string]int{}
	}
	// viz.com's robots.txt sets Crawl-delay: 2 — enforce it even when
	// the config omits it.
	if c.HTTP.HostDelayMS["viz.com"] < 2000 {
		c.HTTP.HostDelayMS["viz.com"] = 2000
	}
	// Seven Seas' Cloudflare gates on TLS fingerprint from residential
	// IPs (browser-TLS suffices) but throws a full managed challenge from
	// datacenter IPs — so it's wired for both: FlareSolverr when
	// available, browser-TLS otherwise.
	if !contains(c.HTTP.BrowserTLSHosts, "sevenseasentertainment.com") {
		c.HTTP.BrowserTLSHosts = append(c.HTTP.BrowserTLSHosts, "sevenseasentertainment.com")
	}
	if !contains(c.HTTP.FlareSolverrHosts, "sevenseasentertainment.com") {
		c.HTTP.FlareSolverrHosts = append(c.HTTP.FlareSolverrHosts, "sevenseasentertainment.com")
	}
	if c.Sources == nil {
		c.Sources = map[string]SourceConfig{}
	}
}

func (c *Config) validate() error {
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

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// SourceEnabled reports whether the named source is enabled in config.
// Sources absent from the config are disabled.
func (c *Config) SourceEnabled(name string) bool {
	sc, ok := c.Sources[name]
	return ok && sc.Enabled
}
