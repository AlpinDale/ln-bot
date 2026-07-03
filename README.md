# ln-bot

A Discord bot that posts **officially licensed English light-novel releases** to a channel on their release day, and answers slash commands (`/upcoming`, `/recent`, `/releases`, …). It scrapes 7 publishers once a day and runs as a single Docker container.

Sources: J-Novel Club, Yen Press, Seven Seas, Kodansha USA, VIZ Media, Cross Infinite World, Hanashi Media.

## Requirements

- A Linux host with **Docker** and **Docker Compose**
- A **Discord** account and a server you can manage
- A free **Cloudflare** account (only for the Seven Seas source — see step 4)

## 1. Create the Discord bot

1. Open the [Discord Developer Portal](https://discord.com/developers/applications) → **New Application** → name it → **Create**.
2. Left sidebar → **Bot** → **Reset Token** → **Copy**. **Save it now as it's shown only once.** This is your `LNBOT_DISCORD_TOKEN`.
3. Still on the **Bot** page, leave every **Privileged Gateway Intent OFF**. This bot doesn't need any.

## 2. Invite the bot to your server

1. Left sidebar → **OAuth2** → **URL Generator**.
2. Under **Scopes**, tick **exactly two**: **`bot`** and **`applications.commands`**.

   > ⚠️ **The redirect-URI trap:** you don't need a redirect URI to invite a bot. That field is only for website logins. If the page starts demanding you "select a redirect URL," it means you ticked an extra scope (e.g. `identify`). Untick it so only `bot` + `applications.commands` remain, and the demand disappears. Ignore the Redirects section entirely.

3. Under **Bot Permissions**, tick: **View Channels**, **Send Messages**, **Embed Links**.
4. Copy the **Generated URL** at the very bottom of the page. Open it in your browser, choose your server, and click **Authorize**.

## 3. Get the Discord IDs

Enable Developer Mode first: Discord → **User Settings** (gear) → **Advanced** → **Developer Mode** ON. Then right-click to copy each ID:

- **Server ID** — right-click your server's icon → **Copy Server ID** → this is `guild_id`
- **Channel ID** — right-click the channel you want alerts in → **Copy Channel ID** → this is `alert_channel_id`
- **Your User ID** — right-click your own name → **Copy User ID** → goes in `admin_ids` (who may run `/scrape` and `/archive`)

IDs are long numbers like `1424843754254237769`.

## 4. Get the Cloudflare Account ID + token (for Seven Seas)

Seven Seas' site blocks datacenter IPs, so the bot fetches it through Cloudflare's URL Scanner. You need a (free) account ID and API token:

1. [Cloudflare dashboard](https://dash.cloudflare.com) → select your account. The **Account ID** is in the URL: `dash.cloudflare.com/`**`<account_id>`**`/…` → this is `LNBOT_CF_ACCOUNT`.
2. **My Profile → API Tokens → Create Token → Create Custom Token**. Add permission **Account → URL Scanner → Edit**. Set **Account Resources → Include → your account**. Create, then **copy the token** (shown once) → this is `LNBOT_CF_TOKEN`.

> Skip this only if you disable the `sevenseas` source in `config.yaml`. Otherwise it can't fetch Seven Seas from a server.

## 5. Configure

```bash
git clone https://github.com/AlpinDale/ln-bot && cd ln-bot
cp config.example.yaml config.yaml
```

Edit **`config.yaml`** and set `discord.guild_id`, `discord.alert_channel_id`, `discord.admin_ids`, and `schedule.timezone` (e.g. `America/Los_Angeles`). Everything else has sane defaults.

Create a **`.env`** file for the secrets:

```
LNBOT_DISCORD_TOKEN=your-bot-token
LNBOT_CF_ACCOUNT=your-cloudflare-account-id
LNBOT_CF_TOKEN=your-cloudflare-token
```

## 6. Start it

The database lives in `./data`, written by the container's non-root user, so create it with the right owner first:

```bash
mkdir -p data && sudo chown -R 65532:65532 data
docker compose up -d --build
```

## 7. Watch the logs

```bash
docker compose logs -f lnbot
```

You should see `discord connected` followed by `lnbot running`. (If you see `unable to open database file`, redo the `chown` above.)

## 8. First run

In your Discord server:

- **`/scrape`** — populate the database with the full catalog. Runs in the background and posts a summary to the alert channel when done (can take a while).
- **`/archive`** *(optional)* — post the entire release history to the channel in date order.
- **`/upcoming`**, **`/recent`**, **`/releases date:…`**, **`/sources`** — query commands.

After that it scrapes automatically once a day (default 9 AM in your `schedule.timezone`) and posts that day's releases to the channel. Only admins (`admin_ids`) can run `/scrape` and `/archive`.
