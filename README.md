# Game Library — Auto-Sync

A tiny Go worker that keeps a **Notion database of your games** in sync with your
Steam library. Hours played, last session and cover art update on their own —
you just decide what to play next.

- **Zero dependencies.** Pure Go standard library. One `main.go`, no Dockerfile,
  no framework.
- **Never touches your notes.** It only writes the Steam-derived fields. Your
  `Status`, `Rating` and `Notes` are yours — the sync never overwrites them.
- **Cheap to re-run.** After the first sync it only writes what actually changed,
  so refreshes are near-instant.

> Unofficial — not affiliated with Valve Corporation. Steam is a trademark of
> Valve Corporation. This tool only reads public data from your own Steam
> profile via the official Steam Web API.

---

## What it does

- Reads your whole library with `GetOwnedGames` (name, hours played, last played
  date) and flags games played in the **last 2 weeks** via
  `GetRecentlyPlayedGames`.
- **Upserts** each game into Notion using `AppID` as the key: creates the page if
  it's missing, updates hours / date / recent flag if anything changed, and sends
  no request at all when nothing changed.
- Sets `Status` to **Backlog** only when it first creates a page — never again.
- Pulls cover art from the Steam store as the Notion page cover (self-healing:
  a cover the store API can't resolve yet is retried on a later sync).
- Respects Notion's rate limit (~3 req/s) with a 350 ms throttle and automatic
  retry on `429`.

Property names are resolved at runtime, so the same binary works with both the
English and Spanish templates (see [Database schema](#database-schema)).

---

## Quick start (local)

You need Go 1.22+ installed and a Steam account with a **public** profile.

```bash
git clone <this-repo>
cd notion-steam
cp .env.example .env      # then fill in your 4 credentials (see below)
make run-once             # sync once and exit
```

That's it — your Notion database fills up in one pass. Run `make run-once`
whenever you want to refresh it.

Two run modes:

| Command         | Behaviour                                                      |
|-----------------|---------------------------------------------------------------|
| `make run-once` | Sync a single time and exit. Best for manual runs and cron/CI. |
| `make run`      | Sync now, then keep running and re-sync every `SYNC_INTERVAL_HOURS` (default 6). Best for a machine that's always on. |

Without the Makefile you can run it directly (env vars must be set in your shell):

```bash
go run . -once     # single sync
go run .           # loop forever
```

`-once` is also available as `SYNC_ONCE=true` for environments where passing a
flag is awkward (schedulers, containers).

---

## Credentials

Copy `.env.example` to `.env` and fill in:

| Variable        | How to get it                                                                 |
|-----------------|-------------------------------------------------------------------------------|
| `STEAM_API_KEY` | https://steamcommunity.com/dev/apikey (you can enter `localhost` as domain).  |
| `STEAM_ID64`    | Your 17-digit SteamID64 (from your profile URL, or look it up at steamid.io). |
| `NOTION_TOKEN`  | Create an internal integration at https://www.notion.so/my-integrations and copy the token. |
| `NOTION_DB_ID`  | The 32-hex-character string in your database URL, **before** the `?v=`.        |

Optional:

| Variable              | Default | Meaning                                  |
|-----------------------|---------|------------------------------------------|
| `SYNC_INTERVAL_HOURS` | `6`     | Hours between syncs in loop mode.        |
| `SYNC_ONCE`           | –       | `true` = sync once and exit (like `-once`). |

Two things people miss:

1. **Make your Steam profile public.** Steam → Settings → Privacy → set both your
   profile *and* "game details" to Public. Otherwise Steam returns 0 games.
2. **Connect the integration to your database.** On the database page: `···` menu
   → Connections → add your integration. Skipping this is what causes a `404`.

---

## Database schema

If you're using the Notion template, this is already set up. If you're building
your own database, it must have **exactly** these properties (names can be in
English or Spanish; types are the contract):

| Property       | Type      | Owner              |
|----------------|-----------|--------------------|
| Name           | Title     | sync (on create)   |
| AppID          | Number    | sync (on create)   |
| Hours Played   | Number    | sync               |
| Last Played    | Date      | sync               |
| Recent         | Checkbox  | sync               |
| Status         | Select    | you (sync sets "Backlog" once, on create) |
| Rating         | Select    | you                |
| Notes          | Text      | you                |

Any page **without** an `AppID` is treated as user-created and left completely
alone. Suggested views: a gallery filtered by `Recent` ("Now Playing"), a backlog
table sorted by hours, a board grouped by `Status`.

---

## Keeping it in sync automatically

`make run-once` is manual by design. For a hands-off refresh you have two options:

### GitHub Actions (recommended — no server, no card)

The repo ships with a ready-made workflow
([`.github/workflows/sync.yml`](.github/workflows/sync.yml)) that runs
`go run . -once` every 6 hours on GitHub's free runners.

1. **Fork this repo** (a fork is enough — you don't need to change any code).
2. In your fork: **Settings → Secrets and variables → Actions → New repository
   secret**, and add the 4 credentials: `STEAM_API_KEY`, `STEAM_ID64`,
   `NOTION_TOKEN`, `NOTION_DB_ID`.
3. Open the **Actions** tab and click **"I understand my workflows, go ahead and
   enable them"** — GitHub disables scheduled workflows in forks by default.
4. Test it: Actions → **Sync** → **Run workflow**. A green run means your Notion
   database is now updating itself.

Two GitHub quirks to know about:

- Scheduled runs can start a few minutes late — GitHub queues them best-effort.
- On public repos, GitHub **pauses schedules after 60 days without commits**.
  It emails you first; one click ("Enable workflow") — or any commit — resumes it.

### Railway / a small VPS (advanced)

Deploy `make run` as a 24/7 process. Nixpacks auto-detects Go from `go.mod`, no
Dockerfile needed. Uses ~10–15 MB of RAM. Set the 4 credentials as environment
variables.

---

## License

MIT.
