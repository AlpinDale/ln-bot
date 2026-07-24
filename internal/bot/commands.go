package bot

import (
	"context"
	"fmt"
	"sort"
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
			Name:        "series",
			Description: "All known volumes and release dates for a series",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "name",
					Description:  "Series name (start typing for suggestions)",
					Required:     true,
					Autocomplete: true},
			},
		},
		{
			Name:        "post",
			Description: "Post a specific volume — to everyone, or just yourself",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "series",
					Description:  "Series (start typing for suggestions)",
					Required:     true,
					Autocomplete: true},
				{Type: discordgo.ApplicationCommandOptionString, Name: "volume",
					Description:  "Volume (suggestions scoped to the chosen series)",
					Required:     true,
					Autocomplete: true},
				{Type: discordgo.ApplicationCommandOptionBoolean, Name: "public",
					Description: "Show to everyone in the channel (default: only you)"},
			},
		},
		{
			Name:        "sources",
			Description: "Registered release sources and their last scrape",
		},
		{
			Name:        "scrape",
			Description: "Admin: full-catalog scrape + announce pass (slow)",
			Options: []*discordgo.ApplicationCommandOption{
				{Type: discordgo.ApplicationCommandOptionString, Name: "source",
					Description: "Scrape one source only (default: all)",
					Choices:     b.sourceChoices()},
			},
		},
		{
			Name:        "archive",
			Description: "Admin: post the full release history to this channel (slow)",
		},
	}
}

// sourceChoices builds the /scrape source dropdown from the enabled
// sources — label is the publisher name, value is the source key.
func (b *Bot) sourceChoices() []*discordgo.ApplicationCommandOptionChoice {
	var choices []*discordgo.ApplicationCommandOptionChoice
	for _, s := range b.sources() {
		if !b.cfg.SourceEnabled(s.Name()) {
			continue
		}
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
			Name:  s.Publisher(),
			Value: s.Name(),
		})
	}
	return choices
}

func (b *Bot) registerCommands() error {
	// After Open() the gateway READY normally populates State.User, but a
	// reconnect race can leave it nil — guard rather than panic.
	if b.session.State == nil || b.session.State.User == nil {
		return fmt.Errorf("gateway session not ready (no user in state)")
	}
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
	case discordgo.InteractionApplicationCommandAutocomplete:
		b.handleAutocomplete(s, i)
	case discordgo.InteractionMessageComponent:
		b.handleComponent(s, i)
	}
}

// handleAutocomplete serves live suggestions as the user types. It backs
// /series (`name`) and /post, whose `volume` box is scoped to the `series`
// already picked in the same invocation. Must answer within ~3s.
func (b *Bot) handleAutocomplete(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	var focusedName, focused string
	for _, o := range data.Options {
		if o.Focused {
			focusedName, focused = o.Name, o.StringValue()
			break
		}
	}
	focused = strings.TrimSpace(focused)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var choices []*discordgo.ApplicationCommandOptionChoice
	switch {
	case data.Name == "series", data.Name == "post" && focusedName == "series":
		choices = b.seriesChoices(ctx, focused)
	case data.Name == "post" && focusedName == "volume":
		choices = b.volumeChoices(ctx, optionValue(data.Options, "series"), focused)
	}

	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{Choices: choices},
	}); err != nil {
		b.log.Error("autocomplete respond failed", "err", err)
	}
}

// seriesChoices returns up to 25 series titles matching what's typed.
func (b *Bot) seriesChoices(ctx context.Context, typed string) []*discordgo.ApplicationCommandOptionChoice {
	names, err := b.store.DistinctSeries(ctx, typed, 25)
	if err != nil {
		b.log.Error("series autocomplete query failed", "err", err)
	}
	out := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(names))
	for _, n := range names {
		// Name may carry an ellipsis (display); Value must stay a real
		// prefix so long titles still resolve on lookup.
		out = append(out, &discordgo.ApplicationCommandOptionChoice{Name: truncate(n, 100), Value: choiceValue(n)})
	}
	return out
}

// choiceValue caps s to Discord's 100-char option-value limit without an
// ellipsis, keeping it a genuine prefix of the original so a longer stored
// title still resolves via prefix match.
func choiceValue(s string) string {
	rs := []rune(s)
	if len(rs) > 100 {
		return string(rs[:100])
	}
	return s
}

// resolveSeries maps a (possibly 100-char-truncated) series value to the
// stored series title(s) it identifies: an exact case-insensitive match
// wins; otherwise titles containing the value as a substring — which
// recovers a long title from its prefix. Empty when nothing matches.
func (b *Bot) resolveSeries(ctx context.Context, input string) []string {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil
	}
	if rels, err := b.store.ReleasesForSeries(ctx, input); err == nil && len(rels) > 0 {
		return []string{rels[0].SeriesTitle}
	}
	cands, err := b.store.DistinctSeries(ctx, input, 25)
	if err != nil {
		b.log.Error("series resolve failed", "err", err)
	}
	return cands
}

// volumeChoices returns the volumes/editions of seriesInput, filtered by
// what's typed — each choice's value is the release ID, so submission maps
// straight to a row and the user can't pick something out of scope.
func (b *Bot) volumeChoices(ctx context.Context, seriesInput, typed string) []*discordgo.ApplicationCommandOptionChoice {
	cands := b.resolveSeries(ctx, seriesInput)
	if len(cands) == 0 {
		return nil // no series chosen/matched yet — nothing to scope to
	}
	rels, err := b.store.ReleasesForSeries(ctx, cands[0])
	if err != nil {
		b.log.Error("volume autocomplete query failed", "err", err)
	}
	typed = strings.ToLower(typed)
	// Prefix matches first; rels arrive pre-sorted (date, format), and a
	// stable sort preserves that order within each rank.
	type scored struct {
		r    model.Release
		rank int
	}
	var xs []scored
	for _, r := range rels {
		vt := strings.ToLower(stripSeriesPrefix(r.VolumeTitle, r.SeriesTitle))
		if typed != "" && !strings.Contains(vt, typed) {
			continue
		}
		rank := 1
		if typed == "" || strings.HasPrefix(vt, typed) {
			rank = 0
		}
		xs = append(xs, scored{r, rank})
	}
	sort.SliceStable(xs, func(i, j int) bool { return xs[i].rank < xs[j].rank })

	out := make([]*discordgo.ApplicationCommandOptionChoice, 0, 25)
	for _, x := range xs {
		if len(out) == 25 {
			break
		}
		vol := stripSeriesPrefix(x.r.VolumeTitle, x.r.SeriesTitle)
		label := truncate(fmt.Sprintf("%s · %s · %s",
			vol, x.r.ReleaseDate.Format(dateLayout), DisplayFormat(x.r.Format)), 100)
		out = append(out, &discordgo.ApplicationCommandOptionChoice{
			Name: label, Value: strconv.FormatInt(x.r.ID, 10),
		})
	}
	return out
}

// optionValue reads a slash-command option's string value by name (used to
// read one option while a different one is focused during autocomplete).
func optionValue(opts []*discordgo.ApplicationCommandInteractionDataOption, name string) string {
	for _, o := range opts {
		if o.Name == name {
			return o.StringValue()
		}
	}
	return ""
}

func (b *Bot) handleCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	name := i.ApplicationCommandData().Name
	b.log.Info("command", "name", name)

	// /post manages its own response: it may be public (non-ephemeral) and
	// carry a delete button, which the shared ephemeral-defer flow can't do.
	if name == "post" {
		b.cmdPost(s, i)
		return
	}

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

	// The remaining commands are quick SQLite queries; /scrape returns
	// immediately and does its work in the background. A short guard is
	// enough to keep the interaction responsive.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var reply string
	var components []discordgo.MessageComponent
	var embeds []*discordgo.MessageEmbed
	switch name {
	case "upcoming":
		reply, components = b.cmdWindow(ctx, i, 0, +1)
	case "recent":
		reply, components = b.cmdWindow(ctx, i, -1, 0)
	case "releases":
		reply, components = b.cmdReleases(ctx, i)
	case "series":
		reply, embeds, components = b.cmdSeries(ctx, i)
	case "sources":
		reply = b.cmdSources(ctx)
	case "scrape":
		reply = b.cmdScrape(ctx, i)
	case "archive":
		reply = b.cmdArchive(ctx, i)
	default:
		reply = "Unknown command."
	}

	if _, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content:    reply,
		Embeds:     embeds,
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
	data := i.MessageComponentData()
	// The /series disambiguation dropdown: the picked value is the series
	// title to render.
	if data.CustomID == seriesPickID {
		if len(data.Values) > 0 {
			b.handleSeriesPick(s, i, data.Values[0])
		}
		return
	}
	// Delete button on a public /post message — scoped to its poster.
	if owner, ok := strings.CutPrefix(data.CustomID, postDelPrefix); ok {
		b.handlePostDelete(s, i, owner)
		return
	}
	q, ok := parsePageID(data.CustomID)
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

// seriesPickID is the custom_id of the /series disambiguation dropdown.
const seriesPickID = "srspick"

// seriesDescBudget caps the volume list in a /series embed, leaving room
// under Discord's 4096-char description limit for an overflow note.
const seriesDescBudget = 3800

// cmdSeries answers /series: show every known volume/edition of a series
// with its release date. The autocomplete usually hands us an exact title,
// but a free-typed value is matched fuzzily — one hit renders directly,
// several offer a dropdown to disambiguate.
func (b *Bot) cmdSeries(ctx context.Context, i *discordgo.InteractionCreate) (string, []*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	name := strings.TrimSpace(options(i)["name"].StringValue())
	if name == "" {
		return "Type a series name — start typing to get suggestions.", nil, nil
	}
	cands := b.resolveSeries(ctx, name)
	switch len(cands) {
	case 0:
		return fmt.Sprintf("No series found matching **%s**.", truncate(name, 100)), nil, nil
	case 1:
		rels, err := b.store.ReleasesForSeries(ctx, cands[0])
		if err != nil {
			b.log.Error("series query failed", "err", err)
			return "Query failed — check the logs.", nil, nil
		}
		if len(rels) == 0 {
			return fmt.Sprintf("No series found matching **%s**.", truncate(name, 100)), nil, nil
		}
		return "", []*discordgo.MessageEmbed{renderSeriesEmbed(rels)}, nil
	default:
		return b.seriesPicker(name, cands)
	}
}

// seriesPicker renders a dropdown of candidate series when a free-typed
// name matches more than one.
func (b *Bot) seriesPicker(query string, cands []string) (string, []*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	opts := make([]discordgo.SelectMenuOption, 0, len(cands))
	for _, c := range cands {
		// Label may be ellipsized for display; the value must stay a real
		// prefix so long titles resolve when picked.
		opts = append(opts, discordgo.SelectMenuOption{Label: truncate(c, 100), Value: choiceValue(c)})
	}
	row := discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.SelectMenu{
			MenuType:    discordgo.StringSelectMenu,
			CustomID:    seriesPickID,
			Placeholder: "Select a series…",
			Options:     opts,
		},
	}}
	return fmt.Sprintf("Multiple series match **%s** — pick one:", truncate(query, 100)),
		nil, []discordgo.MessageComponent{row}
}

// handleSeriesPick renders the chosen series in place of the dropdown.
func (b *Bot) handleSeriesPick(s *discordgo.Session, i *discordgo.InteractionCreate, sel string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var rels []model.Release
	if cands := b.resolveSeries(ctx, sel); len(cands) > 0 {
		var err error
		if rels, err = b.store.ReleasesForSeries(ctx, cands[0]); err != nil {
			b.log.Error("series pick query failed", "err", err)
		}
	}
	data := &discordgo.InteractionResponseData{
		Flags:      discordgo.MessageFlagsEphemeral,
		Components: []discordgo.MessageComponent{}, // drop the dropdown
	}
	if len(rels) == 0 {
		data.Content = "Couldn't load that series."
	} else {
		data.Embeds = []*discordgo.MessageEmbed{renderSeriesEmbed(rels)}
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: data,
	}); err != nil {
		b.log.Error("series pick update failed", "err", err)
	}
}

// renderSeriesEmbed builds the series card: a chronological list of every
// volume/edition (linked where a URL exists) plus publisher/format summary.
// rels must be non-empty and is expected pre-sorted by the store.
func renderSeriesEmbed(rels []model.Release) *discordgo.MessageEmbed {
	title := rels[0].SeriesTitle

	var pubs, formats []string
	seenPub, seenFmt := map[string]bool{}, map[string]bool{}
	for _, r := range rels {
		if r.Publisher != "" && !seenPub[r.Publisher] {
			seenPub[r.Publisher] = true
			pubs = append(pubs, r.Publisher)
		}
		if f := DisplayFormat(r.Format); !seenFmt[f] {
			seenFmt[f] = true
			formats = append(formats, f)
		}
	}

	var sb strings.Builder
	shown := 0
	for _, r := range rels {
		vol := stripSeriesPrefix(r.VolumeTitle, title)
		date := r.ReleaseDate.Format(dateLayout)
		f := DisplayFormat(r.Format)
		var line string
		if clean, ok := cleanURL(r.URL); ok {
			line = fmt.Sprintf("- `%s` [%s](<%s>) · %s\n", date, vol, clean, f)
		} else {
			line = fmt.Sprintf("- `%s` %s · %s\n", date, vol, f)
		}
		if sb.Len()+len(line) > seriesDescBudget {
			break
		}
		sb.WriteString(line)
		shown++
	}
	if shown < len(rels) {
		fmt.Fprintf(&sb, "…and %d more.", len(rels)-shown)
	}

	return &discordgo.MessageEmbed{
		Title:       truncate(title, embedTitleMax),
		Description: sb.String(),
		Color:       0x5865F2, // blurple
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Publisher", Value: truncate(orDash(strings.Join(pubs, ", ")), embedFieldMax), Inline: true},
			{Name: "Known releases", Value: strconv.Itoa(len(rels)), Inline: true},
			{Name: "Formats", Value: truncate(orDash(strings.Join(formats, ", ")), embedFieldMax), Inline: true},
		},
	}
}

// stripSeriesPrefix drops a leading series name (and trailing separators)
// from a volume title so the list doesn't repeat the series on every line.
// Falls back to the full title if nothing meaningful remains.
func stripSeriesPrefix(vol, series string) string {
	if series == "" || !strings.HasPrefix(strings.ToLower(vol), strings.ToLower(series)) {
		return vol
	}
	v := strings.TrimLeft(vol[len(series):], " ,:-–—")
	if v == "" {
		return vol
	}
	return v
}

// postDelPrefix precedes the poster's user ID in a delete button's
// custom_id, so the message survives restarts and only its poster can
// remove it.
const postDelPrefix = "postdel:"

// cmdPost answers /post: render the chosen volume as a card, either
// ephemerally (only the invoker sees it) or publicly in the channel with a
// poster-scoped delete button. It handles its own interaction response.
func (b *Bot) cmdPost(s *discordgo.Session, i *discordgo.InteractionCreate) {
	opts := options(i)
	public := false
	if o, ok := opts["public"]; ok {
		public = o.BoolValue()
	}

	var rel model.Release
	found := false
	if o, ok := opts["volume"]; ok {
		if id, err := strconv.ParseInt(o.StringValue(), 10, 64); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			rel, found, err = b.store.ReleaseByID(ctx, id)
			if err != nil {
				b.log.Error("post lookup failed", "err", err)
			}
		}
	}
	if !found {
		b.respondEphemeral(s, i, "Pick a volume from the suggestions (choose a series first, then a volume).")
		return
	}

	resp := &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Embeds: []*discordgo.MessageEmbed{releaseCard(rel)}},
	}
	if public {
		resp.Data.Components = []discordgo.MessageComponent{discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{discordgo.Button{
				Label:    "Delete",
				Style:    discordgo.DangerButton,
				Emoji:    &discordgo.ComponentEmoji{Name: "🗑️"},
				CustomID: postDelPrefix + invokerID(i),
			}},
		}}
	} else {
		resp.Data.Flags = discordgo.MessageFlagsEphemeral
	}
	if err := s.InteractionRespond(i.Interaction, resp); err != nil {
		b.log.Error("post respond failed", "err", err)
	}
}

// handlePostDelete removes a public /post message, but only when the
// clicker is the user who posted it (encoded in the button's custom_id).
func (b *Bot) handlePostDelete(s *discordgo.Session, i *discordgo.InteractionCreate, ownerID string) {
	if invokerID(i) != ownerID {
		b.respondEphemeral(s, i, "Only the person who posted this can delete it.")
		return
	}
	// Ack the click, then delete the message via the interaction token. For
	// a component interaction the "@original" message is the one the button
	// sits on, and the token-based delete works even where the bot lacks
	// channel access (a plain channel delete returns 403 Missing Access).
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredMessageUpdate,
	}); err != nil {
		b.log.Error("post delete ack failed", "err", err)
		return
	}
	if err := s.InteractionResponseDelete(i.Interaction); err != nil {
		b.log.Error("post delete failed", "err", err)
	}
}

// releaseCard builds a standalone embed for one release, with URLs
// sanitized the same way as channel announcements.
func releaseCard(r model.Release) *discordgo.MessageEmbed {
	embed := &discordgo.MessageEmbed{
		Title:       truncate(r.VolumeTitle, embedTitleMax),
		Description: fmt.Sprintf("**%s**", r.Publisher),
		Color:       0x5865F2, // blurple
		Fields: []*discordgo.MessageEmbedField{
			{Name: "Series", Value: truncate(orDash(r.SeriesTitle), embedFieldMax), Inline: true},
			{Name: "Format", Value: DisplayFormat(r.Format), Inline: true},
			{Name: "Release date", Value: r.ReleaseDate.Format(dateLayout), Inline: true},
		},
		Footer: &discordgo.MessageEmbedFooter{Text: "source: " + r.SourceKey},
	}
	if clean, ok := cleanURL(r.URL); ok {
		embed.URL = clean
	}
	if clean, ok := cleanURL(r.CoverURL); ok {
		embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: clean}
	}
	return embed
}

// respondEphemeral sends a one-off ephemeral reply to an interaction.
func (b *Bot) respondEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: content, Flags: discordgo.MessageFlagsEphemeral},
	}); err != nil {
		b.log.Error("ephemeral respond failed", "err", err)
	}
}

// invokerID returns the user ID behind an interaction, whether it arrived
// from a guild member or a DM user.
func invokerID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
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

// cmdScrape kicks off a full-catalog scrape in the background and
// returns immediately: a backfill runs far longer than Discord's
// interaction window, so the summary is posted to the alert channel when
// it finishes rather than as an interaction reply.
func (b *Bot) cmdScrape(_ context.Context, i *discordgo.InteractionCreate) string {
	if !b.isAdmin(i) {
		return "You're not allowed to run `/scrape`."
	}

	// Optional single-source filter; absent means every enabled source.
	var only []string
	scope := "all sources"
	if o, ok := options(i)["source"]; ok {
		key := o.StringValue()
		only = []string{key}
		scope = b.publisherName(key)
	}

	if !b.scraping.CompareAndSwap(false, true) {
		return "A scrape is already running — hang tight."
	}

	go func() {
		defer b.scraping.Store(false)
		b.log.Info("manual scrape started", "scope", scope)
		summary, err := b.pipeline(b.rootCtx, source.ModeFull, only)
		if err != nil {
			b.log.Error("manual scrape failed", "err", err)
			summary = "Scrape failed: " + err.Error()
		} else {
			b.log.Info("manual scrape finished", "summary", summary)
		}
		if _, err := b.session.ChannelMessageSend(b.cfg.Discord.AlertChannelID,
			"🔄 "+summary); err != nil {
			b.log.Error("scrape summary post failed", "err", err)
		}
	}()

	return fmt.Sprintf("🔄 Full scrape of **%s** started — this can take a while. "+
		"I'll post a summary here when it's done.", scope)
}

// publisherName resolves a source key to its display name, falling back
// to the key itself.
func (b *Bot) publisherName(key string) string {
	for _, s := range b.sources() {
		if s.Name() == key {
			return s.Publisher()
		}
	}
	return key
}

// archiveDelay paces archive posts just over Discord's ~5-per-5s
// per-channel message limit, so we never trigger 429s (a flood of which
// can escalate to a Cloudflare-level block).
const archiveDelay = 1100 * time.Millisecond

// cmdArchive backfills the whole release history into the alert channel
// in date order, one message per release. It reuses alerted_at as the
// "already posted" marker, so it shares state with the daily alerts and
// never double-posts. Long-running and fire-and-forget, like /scrape.
func (b *Bot) cmdArchive(ctx context.Context, i *discordgo.InteractionCreate) string {
	if !b.isAdmin(i) {
		return "You're not allowed to run `/archive`."
	}
	if !b.archiving.CompareAndSwap(false, true) {
		return "An archive run is already in progress."
	}

	// Only the historical record up to today — future releases are left
	// for the daily announcer to post on their release day.
	today := model.DateOnly(time.Now().In(b.loc))
	pending, err := b.store.UnpostedReleases(ctx, today)
	if err != nil {
		b.archiving.Store(false)
		b.log.Error("archive query failed", "err", err)
		return "Couldn't read the backlog — check the logs."
	}
	if len(pending) == 0 {
		b.archiving.Store(false)
		return "Nothing to archive — every release has already been posted here."
	}

	// The run outlives Discord's ~15-min interaction window, so the private
	// completion notice goes to the invoker as a DM rather than an ephemeral
	// followup (which would have expired by then).
	uid := invokerID(i)

	go func() {
		defer b.archiving.Store(false)
		b.log.Info("archive started", "count", len(pending))
		posted := 0
		for _, r := range pending {
			select {
			case <-b.rootCtx.Done():
				b.log.Info("archive interrupted", "posted", posted, "total", len(pending))
				return
			case <-time.After(archiveDelay):
			}
			if err := b.PostRelease(b.rootCtx, r); err != nil {
				b.log.Error("archive post failed", "title", r.VolumeTitle, "err", err)
				continue // leave unposted so a re-run retries it
			}
			if err := b.store.MarkAlerted(b.rootCtx, r.ID, time.Now()); err != nil {
				b.log.Error("archive mark failed", "id", r.ID, "err", err)
			}
			posted++
		}
		b.log.Info("archive finished", "posted", posted, "total", len(pending))
		if err := b.dmUser(uid, fmt.Sprintf("📚 Archive complete — posted %d release(s) to the alert channel in date order.", posted)); err != nil {
			b.log.Error("archive summary DM failed", "err", err)
		}
	}()

	etaMin := max((len(pending)*int(archiveDelay/time.Millisecond)/1000+59)/60, 1)
	return fmt.Sprintf("📚 Archiving %d release(s) to the alert channel in date order — about ~%d min "+
		"(paced under Discord's rate limit). I'll DM you when it's done.", len(pending), etaMin)
}

// dmUser sends content to userID's direct-message channel. Used for private
// notices that outlive the interaction token (e.g. a finished archive run).
func (b *Bot) dmUser(userID, content string) error {
	ch, err := b.session.UserChannelCreate(userID)
	if err != nil {
		return fmt.Errorf("open DM channel: %w", err)
	}
	if _, err := b.session.ChannelMessageSend(ch.ID, content); err != nil {
		return fmt.Errorf("send DM: %w", err)
	}
	return nil
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

// truncate shortens s to at most n runes (never splitting a multibyte
// character), appending an ellipsis when it cuts.
func truncate(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	if n <= 1 {
		return string(rs[:n])
	}
	return string(rs[:n-1]) + "…"
}
