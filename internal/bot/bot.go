// Package bot owns the Discord gateway session: slash command
// registration/handling and posting release alerts.
package bot

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/alpindale/ln-bot/internal/config"
	"github.com/alpindale/ln-bot/internal/model"
	"github.com/alpindale/ln-bot/internal/source"
	"github.com/alpindale/ln-bot/internal/store"
)

// PipelineFunc runs scrape + announce on demand (backs /scrape). When
// only is non-empty, just those source names are scraped. It returns a
// human-readable summary.
type PipelineFunc func(ctx context.Context, mode source.Mode, only []string) (string, error)

// Bot is the Discord layer.
type Bot struct {
	session  *discordgo.Session
	store    *store.Store
	cfg      *config.Config
	loc      *time.Location
	pipeline PipelineFunc
	sources  func() []source.Source // registry view for /sources
	rootCtx  context.Context        // background scrapes run under this
	log      *slog.Logger

	scraping  atomic.Bool // guards against overlapping /scrape runs
	archiving atomic.Bool // guards against overlapping /archive runs
}

// New builds the Bot (does not connect yet). rootCtx bounds background
// work (e.g. /scrape) and is cancelled at shutdown.
func New(cfg *config.Config, st *store.Store, pipeline PipelineFunc, sources func() []source.Source, rootCtx context.Context, log *slog.Logger) (*Bot, error) {
	s, err := discordgo.New("Bot " + cfg.Discord.Token)
	if err != nil {
		return nil, fmt.Errorf("discord session: %w", err)
	}
	// Slash commands + posting need no privileged intents.
	s.Identify.Intents = discordgo.IntentsGuilds
	return &Bot{
		session:  s,
		store:    st,
		cfg:      cfg,
		loc:      cfg.Location(),
		pipeline: pipeline,
		sources:  sources,
		rootCtx:  rootCtx,
		log:      log,
	}, nil
}

// Start opens the gateway connection and registers guild commands.
func (b *Bot) Start() error {
	b.session.AddHandler(b.handleInteraction)
	if err := b.session.Open(); err != nil {
		return fmt.Errorf("open gateway: %w", err)
	}
	if err := b.registerCommands(); err != nil {
		b.session.Close()
		return err
	}
	b.log.Info("discord connected", "guild", b.cfg.Discord.GuildID)
	return nil
}

// Stop closes the gateway session.
func (b *Bot) Stop() error { return b.session.Close() }

// Discord embed field limits.
const (
	embedTitleMax = 256
	embedFieldMax = 1024
)

// PostRelease implements announcer.Poster: one embed per release to the
// alert channel. Fields are clamped to Discord's embed limits and URLs
// validated, so no single release can fail the whole post.
func (b *Bot) PostRelease(_ context.Context, r model.Release) error {
	embed := &discordgo.MessageEmbed{
		Title:       truncate(r.VolumeTitle, embedTitleMax),
		Description: fmt.Sprintf("**%s** — out now!", r.Publisher),
		Color:       0x57F287, // green
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Series", Value: truncate(orDash(r.SeriesTitle), embedFieldMax), Inline: true},
			{Name: "Format", Value: DisplayFormat(r.Format), Inline: true},
			{Name: "Release date", Value: r.ReleaseDate.Format("2006-01-02"), Inline: true},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: "source: " + r.SourceKey},
	}
	// Discord rejects the whole embed on a malformed url/thumbnail, so
	// normalize (percent-encoding raw spaces/Unicode) and only set them
	// when the result is a valid absolute http(s) URL.
	if clean, ok := cleanURL(r.URL); ok {
		embed.URL = clean
		if clean != r.URL {
			b.log.Info("normalized release URL", "source", r.SourceKey, "from", r.URL, "to", clean)
		}
	} else if r.URL != "" {
		b.log.Warn("dropping unusable release URL", "source", r.SourceKey, "title", r.VolumeTitle, "url", r.URL)
	}
	if clean, ok := cleanURL(r.CoverURL); ok {
		embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: clean}
	} else if r.CoverURL != "" {
		b.log.Warn("dropping unusable cover URL", "source", r.SourceKey, "title", r.VolumeTitle, "url", r.CoverURL)
	}
	_, err := b.session.ChannelMessageSendEmbed(b.cfg.Discord.AlertChannelID, embed)
	return err
}

// cleanURL validates s as an absolute http(s) URL and returns it
// normalized — percent-encoding raw spaces and non-ASCII so Discord (and
// browsers) accept it. ok is false for empty, relative, or unparseable
// URLs, or non-http(s) schemes.
func cleanURL(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	return u.String(), true
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// DisplayFormat renders a release format for humans; "unknown" (e.g.
// Yen Press, whose calendar doesn't split editions) shows as a dash.
func DisplayFormat(f string) string {
	if f == "" || f == model.FormatUnknown {
		return "—"
	}
	return f
}

// isAdmin reports whether the interaction invoker matches admin_ids by
// user ID or by any of their role IDs.
func (b *Bot) isAdmin(i *discordgo.InteractionCreate) bool {
	ids := map[string]bool{}
	for _, id := range b.cfg.Discord.AdminIDs {
		ids[id] = true
	}
	if i.Member != nil {
		if ids[i.Member.User.ID] {
			return true
		}
		for _, role := range i.Member.Roles {
			if ids[role] {
				return true
			}
		}
	} else if i.User != nil && ids[i.User.ID] {
		return true
	}
	return false
}
