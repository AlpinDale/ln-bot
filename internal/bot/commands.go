package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/alpindale/ln-bot/internal/model"
	"github.com/alpindale/ln-bot/internal/source"
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
			Description: "Admin: full-catalog scrape + announce pass (slow)",
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
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		b.handleCommand(s, i)
	case discordgo.InteractionMessageComponent:
		b.handleComponent(s, i)
	}
}

func (b *Bot) handleCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
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
	var components []discordgo.MessageComponent
	switch name {
	case "upcoming":
		reply, components = b.cmdWindow(ctx, i, 0, +1)
	case "recent":
		reply, components = b.cmdWindow(ctx, i, -1, 0)
	case "releases":
		reply, components = b.cmdReleases(ctx, i)
	case "sources":
		reply = b.cmdSources(ctx)
	case "scrape":
		reply = b.cmdScrape(ctx, i)
	default:
		reply = "Unknown command."
	}

	if _, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content:    reply,
		Components: components,
		Flags:      discordgo.MessageFlagsEphemeral,
	}); err != nil {
		b.log.Error("followup failed", "cmd", name, "err", err)
	}
}

// handleComponent serves the pagination buttons: the custom_id carries
// the full query, so each press re-queries the store and edits the
// message in place. Stateless — survives restarts, holds no sessions.
func (b *Bot) handleComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	q, ok := parsePageID(i.MessageComponentData().CustomID)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	content, components, err := b.renderReleasePage(ctx, q)
	if err != nil {
		b.log.Error("page query failed", "err", err)
		content, components = "Query failed — check the logs.", nil
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content:    content,
			Components: components,
			Flags:      discordgo.MessageFlagsEphemeral,
		},
	}); err != nil {
		b.log.Error("page update failed", "err", err)
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
func (b *Bot) cmdWindow(ctx context.Context, i *discordgo.InteractionCreate, backDays, fwdDays int) (string, []discordgo.MessageComponent) {
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
	return b.releasePageReply(ctx, pageQuery{
		From: model.DateOnly(from), To: model.DateOnly(to), Publisher: publisher,
	})
}

func (b *Bot) cmdReleases(ctx context.Context, i *discordgo.InteractionCreate) (string, []discordgo.MessageComponent) {
	opts := options(i)
	from, err := time.ParseInLocation(dateLayout, opts["date"].StringValue(), time.UTC)
	if err != nil {
		return "Invalid `date` — use YYYY-MM-DD.", nil
	}
	to := from
	if o, ok := opts["end"]; ok {
		if to, err = time.ParseInLocation(dateLayout, o.StringValue(), time.UTC); err != nil {
			return "Invalid `end` — use YYYY-MM-DD.", nil
		}
		if to.Before(from) {
			return "`end` is before `date`.", nil
		}
	}
	publisher := ""
	if o, ok := opts["publisher"]; ok {
		publisher = o.StringValue()
	}
	return b.releasePageReply(ctx, pageQuery{From: from, To: to, Publisher: publisher})
}

func (b *Bot) releasePageReply(ctx context.Context, q pageQuery) (string, []discordgo.MessageComponent) {
	content, components, err := b.renderReleasePage(ctx, q)
	if err != nil {
		b.log.Error("query failed", "err", err)
		return "Query failed — check the logs.", nil
	}
	return content, components
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
	summary, err := b.pipeline(ctx, source.ModeFull)
	if err != nil {
		return "Scrape failed: " + err.Error()
	}
	return summary
}

// --- pagination ---
//
// Long release lists are split into pages navigated with ◀/▶ buttons.
// The buttons' custom_id encodes the whole query (dates, publisher,
// page), so no server-side pagination state exists.

const (
	pageIDPrefix   = "rel"
	pageCharBudget = 1700 // per page, leaving room for the header
	pageMaxLines   = 15
	pubIDMaxLen    = 40 // custom_id total limit is 100 chars
)

type pageQuery struct {
	From, To  time.Time
	Publisher string
	Page      int
}

func encodePageID(q pageQuery, page int) string {
	// ':' is the field separator; publisher goes last and is sanitized.
	pub := strings.ReplaceAll(q.Publisher, ":", "")
	if len(pub) > pubIDMaxLen {
		pub = pub[:pubIDMaxLen]
	}
	return fmt.Sprintf("%s:%s:%s:%d:%s", pageIDPrefix,
		q.From.Format(dateLayout), q.To.Format(dateLayout), page, pub)
}

func parsePageID(id string) (pageQuery, bool) {
	parts := strings.SplitN(id, ":", 5)
	if len(parts) != 5 || parts[0] != pageIDPrefix {
		return pageQuery{}, false
	}
	from, err1 := time.ParseInLocation(dateLayout, parts[1], time.UTC)
	to, err2 := time.ParseInLocation(dateLayout, parts[2], time.UTC)
	page, err3 := strconv.Atoi(parts[3])
	if err1 != nil || err2 != nil || err3 != nil || page < 0 {
		return pageQuery{}, false
	}
	return pageQuery{From: from, To: to, Publisher: parts[4], Page: page}, true
}

// renderReleasePage runs the query and renders one page plus its
// navigation buttons (nil components when everything fits on one page).
func (b *Bot) renderReleasePage(ctx context.Context, q pageQuery) (string, []discordgo.MessageComponent, error) {
	releases, err := b.store.ReleasesBetween(ctx, q.From, q.To, q.Publisher)
	if err != nil {
		return "", nil, err
	}

	label := "Releases on " + q.From.Format(dateLayout)
	if !q.To.Equal(q.From) {
		label = fmt.Sprintf("Releases %s → %s", q.From.Format(dateLayout), q.To.Format(dateLayout))
	}
	if q.Publisher != "" {
		label += " · " + q.Publisher
	}
	if len(releases) == 0 {
		return label + ": nothing found.", nil, nil
	}

	pages := paginateReleases(releases)
	page := min(q.Page, len(pages)-1)

	var sb strings.Builder
	fmt.Fprintf(&sb, "**%s** — %d release(s)", label, len(releases))
	if len(pages) > 1 {
		fmt.Fprintf(&sb, " · page %d/%d", page+1, len(pages))
	}
	sb.WriteString("\n")
	for _, line := range pages[page] {
		sb.WriteString(line)
	}

	if len(pages) == 1 {
		return sb.String(), nil, nil
	}
	nav := discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{
			Label: "◀", Style: discordgo.SecondaryButton,
			CustomID: encodePageID(q, page-1), Disabled: page == 0,
		},
		discordgo.Button{
			Label: fmt.Sprintf("%d / %d", page+1, len(pages)),
			Style: discordgo.SecondaryButton,
			// Never pressed (disabled) but still needs a unique ID.
			CustomID: pageIDPrefix + ":indicator", Disabled: true,
		},
		discordgo.Button{
			Label: "▶", Style: discordgo.SecondaryButton,
			CustomID: encodePageID(q, page+1), Disabled: page == len(pages)-1,
		},
	}}
	return sb.String(), []discordgo.MessageComponent{nav}, nil
}

// paginateReleases renders releases to lines and groups them into pages
// under the char budget and line cap.
func paginateReleases(releases []model.Release) [][]string {
	var pages [][]string
	var cur []string
	curLen := 0
	for _, r := range releases {
		title := r.VolumeTitle
		if r.URL != "" {
			title = fmt.Sprintf("[%s](<%s>)", r.VolumeTitle, r.URL)
		}
		line := fmt.Sprintf("- `%s` %s — %s (%s)\n",
			r.ReleaseDate.Format(dateLayout), title, r.Publisher, DisplayFormat(r.Format))
		if len(cur) > 0 && (curLen+len(line) > pageCharBudget || len(cur) >= pageMaxLines) {
			pages = append(pages, cur)
			cur, curLen = nil, 0
		}
		cur = append(cur, line)
		curLen += len(line)
	}
	return append(pages, cur)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
