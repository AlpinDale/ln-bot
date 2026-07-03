// Command lnbot runs the LN release Discord bot: a persistent gateway
// connection plus a daily scrape/announce schedule.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // embedded zoneinfo for scratch/distroless images

	"github.com/robfig/cron/v3"

	"github.com/alpindale/ln-bot/internal/announcer"
	"github.com/alpindale/ln-bot/internal/bot"
	"github.com/alpindale/ln-bot/internal/config"
	"github.com/alpindale/ln-bot/internal/scraper"
	"github.com/alpindale/ln-bot/internal/source"
	_ "github.com/alpindale/ln-bot/internal/source/all" // plugin manifest
	"github.com/alpindale/ln-bot/internal/source/fetch"
	"github.com/alpindale/ln-bot/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.yaml", "path to config file")
	oneshot := flag.String("oneshot", "", "run a single scrape (\"incremental\" or \"full\") without Discord, then exit")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(log)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	st, err := store.Open(cfg.Database.Path)
	if err != nil {
		return err
	}
	defer st.Close()

	client := fetch.New(fetch.Options{
		UserAgent: cfg.HTTP.UserAgent,
		MinDelay:  time.Duration(cfg.HTTP.MinDelayMS) * time.Millisecond,
		Timeout:   time.Duration(cfg.HTTP.TimeoutSeconds) * time.Second,
	})
	enabledSources := func() []source.Source { return source.Enabled(cfg.SourceEnabled) }
	scr := scraper.New(st, client, enabledSources, log)

	// Scrape-only mode for testing sources: no Discord, no announcing.
	if *oneshot != "" {
		mode := source.ModeIncremental
		if *oneshot == "full" {
			mode = source.ModeFull
		} else if *oneshot != "incremental" {
			return fmt.Errorf("-oneshot must be \"incremental\" or \"full\", got %q", *oneshot)
		}
		res, err := scr.RunAll(context.Background(), mode)
		if err != nil {
			return err
		}
		log.Info("oneshot done", "sources", res.Sources, "fetched", res.Fetched,
			"new", res.New, "failures", res.Failures)
		return nil
	}

	if err := cfg.ValidateDiscord(); err != nil {
		return err
	}

	// The announcer's poster is the bot; break the construction cycle by
	// declaring the pipeline first as a closure over late-bound vars.
	var ann *announcer.Announcer
	pipeline := func(ctx context.Context, mode source.Mode) (string, error) {
		res, err := scr.RunAll(ctx, mode)
		if err != nil {
			return "", err
		}
		posted, err := ann.Run(ctx)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(
			"Scraped %d source(s): %d release(s) listed, %d new, %d failure(s). Announced %d.",
			res.Sources, res.Fetched, res.New, res.Failures, posted), nil
	}

	b, err := bot.New(cfg, st, pipeline, source.All, log)
	if err != nil {
		return err
	}
	ann = announcer.New(st, b, cfg.Location(), cfg.Announce.LookbackDays, log)

	if err := b.Start(); err != nil {
		return err
	}
	defer b.Stop()

	// Daily schedule in the configured timezone.
	c := cron.New(cron.WithLocation(cfg.Location()))
	_, err = c.AddFunc(cfg.Schedule.Cron, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		summary, err := pipeline(ctx, source.ModeIncremental)
		if err != nil {
			log.Error("scheduled run failed", "err", err)
			return
		}
		log.Info("scheduled run finished", "summary", summary)
	})
	if err != nil {
		return fmt.Errorf("bad cron spec %q: %w", cfg.Schedule.Cron, err)
	}
	c.Start()
	defer c.Stop()

	log.Info("lnbot running",
		"cron", cfg.Schedule.Cron,
		"tz", cfg.Schedule.Timezone,
		"sources_enabled", len(enabledSources()),
		"sources_registered", len(source.All()))

	// Block until SIGINT/SIGTERM.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Info("shutting down")
	// Deferred: cron stop (waits for no new jobs), bot close, store close.
	return nil
}
