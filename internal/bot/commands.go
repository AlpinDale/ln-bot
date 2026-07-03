package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/alpindale/ln-bot/internal/model"
)

const dateLayout = "2006-01-02"

func (b *Bot) commandDefinitions() []*discordgo.ApplicationCommand {
	minDays, maxDays := float64(1), float64(90)
	return []*discordgo.ApplicationCommand{
		{
			Name:        "upcoming",
			Description: "Light novel releases coming up",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionInteger, Name: "days",
					Description: "How many days ahead (default 7)", MinValue: &minDays, MaxValue: maxDays},
				{Type: discordgo.ApplicationCommandOptionString, Name: "publisher",
					Description: "Filter by publisher name"},
			},
		},
		{
			Name:        "recent",
			Description: "Light novel releases from the past days",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionInteger, Name: "days",
					Description: "How many days back (default 7)", MinValue: &minDays, MaxValue: maxDays},
				{Type: discordgo.ApplicationCommandOptionString, Name: "publisher",
					Description: "Filter by publisher name"},
			},
		},
		{
			Name:        "releases",
			Description: "Releases on a date or in a date range",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "date",
					Description: "Date (YYYY-MM-DD)", Required: true},
				{Type: discordgo.ApplicationCommandOptionString, Name: "end",
					Description: "End of range (YYYY-MM-DD, inclusive)"},
				{Type: discordgo.ApplicationCommandOptionString, Name: "publisher",
					Description: "Filter by publisher name"},
			},
		},
		{
			Name:        "sources",
			Description: "Registered release sources and their last scrape",
		},
		{
			Name:        "scrape",
			Description: "Admin: run a scrape + announce pass now",
		},
	}
}

func (b *Bot) registerCommands() error {
	appID := b.session.State.User.ID
	// Clear GLOBAL commands: ours are guild-scoped, so anything global
	// is stale (e.g. left over from a repurposed application ID).
	// Removals can take up to an hour to disappear from clients.
	if _, err := b.session.ApplicationCommandBulkOverwrite(appID, "",
		[]*discordgo.ApplicationCommand{}); err != nil {
		return fmt.Errorf("clear global commands: %w", err)
	}
	_, err := b.session.ApplicationCommandBulkOverwrite(
		appID, b.cfg.Discord.GuildID, b.commandDefinitions())
	if err != nil {
		return fmt.Errorf("register commands: %w", err)
	}
	return nil
}

func (b *Bot) handleInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	name := i.ApplicationCommandData().Name
	b.log.Info("command", "name", name)

	// Everything is answered via deferred response so slow paths
	// (scrape) and fast paths share one shape. Replies are ephemeral —
	// only release announcements belong in the channel for everyone.
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Flags: discordgo.MessageFlagsEphemeral,
		},
	}); err != nil {
		b.log.Error("defer failed", "cmd", name, "err", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var reply string
	switch name {
	case "upcoming":
		reply = b.cmdWindow(ctx, i, 0, +1)
	case "recent":
		reply = b.cmdWindow(ctx, i, -1, 0)
	case "releases":
		reply = b.cmdReleases(ctx, i)
	case "sources":
		reply = b.cmdSources(ctx)
	case "scrape":
		reply = b.cmdScrape(ctx, i)
	default:
		reply = "Unknown command."
	}

	if _, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: reply,
		Flags:   discordgo.MessageFlagsEphemeral,
	}); err != nil {
		b.log.Error("followup failed", "cmd", name, "err", err)
	}
}

func options(i *discordgo.InteractionCreate) map[string]*discordgo.ApplicationCommandInteractionDataOption {
	out := map[string]*discordgo.ApplicationCommandInteractionDataOption{}
	for _, o := range i.ApplicationCommandData().Options {
		out[o.Name] = o
	}
	return out
}

// cmdWindow implements /upcoming (dir=+1) and /recent (dir=-1): a window
// of N days on one side of today.
func (b *Bot) cmdWindow(ctx context.Context, i *discordgo.InteractionCreate, backDays, fwdDays int) string {
	opts := options(i)
	days := 7
	if o, ok := opts["days"]; ok {
		days = int(o.IntValue())
	}
	publisher := ""
	if o, ok := opts["publisher"]; ok {
		publisher = o.StringValue()
	}

	today := time.Now().In(b.loc)
	from, to := today, today
	if backDays != 0 {
		from = today.AddDate(0, 0, -days)
	}
	if fwdDays != 0 {
		to = today.AddDate(0, 0, days)
	}
	releases, err := b.store.ReleasesBetween(ctx, model.DateOnly(from), model.DateOnly(to), publisher)
	if err != nil {
		b.log.Error("query failed", "err", err)
		return "Query failed — check the logs."
	}
	label := fmt.Sprintf("Releases %s → %s", from.Format(dateLayout), to.Format(dateLayout))
	return formatReleaseList(label, releases)
}

func (b *Bot) cmdReleases(ctx context.Context, i *discordgo.InteractionCreate) string {
	opts := options(i)
	from, err := time.ParseInLocation(dateLayout, opts["date"].StringValue(), time.UTC)
	if err != nil {
		return "Invalid `date` — use YYYY-MM-DD."
	}
	to := from
	if o, ok := opts["end"]; ok {
		if to, err = time.ParseInLocation(dateLayout, o.StringValue(), time.UTC); err != nil {
			return "Invalid `end` — use YYYY-MM-DD."
		}
		if to.Before(from) {
			return "`end` is before `date`."
		}
	}
	publisher := ""
	if o, ok := opts["publisher"]; ok {
		publisher = o.StringValue()
	}
	releases, err := b.store.ReleasesBetween(ctx, from, to, publisher)
	if err != nil {
		b.log.Error("query failed", "err", err)
		return "Query failed — check the logs."
	}
	label := "Releases on " + from.Format(dateLayout)
	if !to.Equal(from) {
		label = fmt.Sprintf("Releases %s → %s", from.Format(dateLayout), to.Format(dateLayout))
	}
	return formatReleaseList(label, releases)
}

func (b *Bot) cmdSources(ctx context.Context) string {
	last, err := b.store.LastRunPerSource(ctx)
	if err != nil {
		b.log.Error("query failed", "err", err)
		return "Query failed — check the logs."
	}
	var sb strings.Builder
	sb.WriteString("**Sources**\n")
	for _, s := range b.sources() {
		enabled := "disabled"
		if b.cfg.SourceEnabled(s.Name()) {
			enabled = "enabled"
		}
		line := fmt.Sprintf("- `%s` (%s) — %s", s.Name(), s.Publisher(), enabled)
		if run, ok := last[s.Name()]; ok {
			if run.Status == "ok" {
				line += fmt.Sprintf("; last scrape %s, %d releases",
					run.FinishedAt.Format("2006-01-02 15:04 MST"), run.Count)
			} else {
				line += fmt.Sprintf("; last scrape FAILED %s: %s",
					run.FinishedAt.Format("2006-01-02 15:04 MST"), truncate(run.Error, 120))
			}
		} else {
			line += "; never scraped"
		}
		sb.WriteString(line + "\n")
	}
	return sb.String()
}

func (b *Bot) cmdScrape(ctx context.Context, i *discordgo.InteractionCreate) string {
	if !b.isAdmin(i) {
		return "You're not allowed to run `/scrape`."
	}
	summary, err := b.pipeline(ctx)
	if err != nil {
		return "Scrape failed: " + err.Error()
	}
	return summary
}

// formatReleaseList renders releases as markdown lines, respecting
// Discord's 2000-char content limit.
func formatReleaseList(label string, releases []model.Release) string {
	if len(releases) == 0 {
		return label + ": nothing found."
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "**%s** — %d release(s)\n", label, len(releases))
	shown := 0
	for _, r := range releases {
		title := r.VolumeTitle
		if r.URL != "" {
			title = fmt.Sprintf("[%s](<%s>)", r.VolumeTitle, r.URL)
		}
		line := fmt.Sprintf("- `%s` %s — %s (%s)\n",
			r.ReleaseDate.Format(dateLayout), title, r.Publisher, r.Format)
		if sb.Len()+len(line) > 1900 {
			break
		}
		sb.WriteString(line)
		shown++
	}
	if shown < len(releases) {
		fmt.Fprintf(&sb, "…and %d more.", len(releases)-shown)
	}
	return sb.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
