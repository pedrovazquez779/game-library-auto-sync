// game-library-auto-sync — worker that syncs your Steam library
// to a Notion database. Designed to run 24/7 on Railway.
//
// Required environment variables:
//
//	STEAM_API_KEY        — https://steamcommunity.com/dev/apikey
//	STEAM_ID64           — your SteamID64 (public profile)
//	NOTION_TOKEN         — Notion internal integration token
//	NOTION_DB_ID         — database ID (from Notion URL)
//
// Optional:
//
//	SYNC_INTERVAL_HOURS  — hours between syncs (default: 6)
//	SYNC_ONCE            — "true" to sync once and exit (same as -once)
//
// Flags:
//
//	-once                — run a single sync and exit (no ticker loop)
//
// Stdlib only, no dependencies.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	notionAPI     = "https://api.notion.com/v1"
	notionVersion = "2022-06-28"
	// Notion allows ~3 req/s; 350ms between writes gives us headroom.
	notionThrottle = 350 * time.Millisecond
)

// ---------- Steam API ----------

type steamGame struct {
	AppID           int    `json:"appid"`
	Name            string `json:"name"`
	PlaytimeForever int    `json:"playtime_forever"` // minutos
	RtimeLastPlayed int64  `json:"rtime_last_played"`
}

type steamOwnedResp struct {
	Response struct {
		GameCount int         `json:"game_count"`
		Games     []steamGame `json:"games"`
	} `json:"response"`
}

func fetchOwnedGames(apiKey, steamID string) ([]steamGame, error) {
	q := url.Values{}
	q.Set("key", apiKey)
	q.Set("steamid", steamID)
	q.Set("include_appinfo", "1")
	q.Set("include_played_free_games", "1")

	u := "https://api.steampowered.com/IPlayerService/GetOwnedGames/v1/?" + q.Encode()
	resp, err := http.Get(u)
	if err != nil {
		return nil, fmt.Errorf("GetOwnedGames: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GetOwnedGames status %d: %s", resp.StatusCode, body)
	}

	var out steamOwnedResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("GetOwnedGames decode: %w", err)
	}
	if out.Response.GameCount == 0 && len(out.Response.Games) == 0 {
		return nil, fmt.Errorf("Steam returned 0 games: is your profile and 'game details' set to public?")
	}
	return out.Response.Games, nil
}

func fetchRecentAppIDs(apiKey, steamID string) (map[int]bool, error) {
	q := url.Values{}
	q.Set("key", apiKey)
	q.Set("steamid", steamID)

	u := "https://api.steampowered.com/IPlayerService/GetRecentlyPlayedGames/v1/?" + q.Encode()
	resp, err := http.Get(u)
	if err != nil {
		return nil, fmt.Errorf("GetRecentlyPlayedGames: %w", err)
	}
	defer resp.Body.Close()

	var out steamOwnedResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("GetRecentlyPlayedGames decode: %w", err)
	}
	recent := make(map[int]bool, len(out.Response.Games))
	for _, g := range out.Response.Games {
		recent[g.AppID] = true
	}
	return recent, nil
}

func coverURL(appID int) string {
	return fmt.Sprintf("https://cdn.cloudflare.steamstatic.com/steam/apps/%d/header.jpg", appID)
}

// Steam's store API allows roughly 200 requests / 5 min per IP. We space
// header-image lookups conservatively so a first full sync (which re-resolves
// every legacy cover at once) stays well under that.
const coverThrottle = 1500 * time.Millisecond

// coverResolver fetches a game's real header image from the Steam store API.
// Newer titles (demos, playtests, recent releases) no longer expose the
// guessable cdn.../steam/apps/{id}/header.jpg path; their art lives under a
// content-hashed /store_item_assets/ URL that only the store API knows.
type coverResolver struct {
	http *http.Client
	last time.Time
}

func newCoverResolver() *coverResolver {
	return &coverResolver{http: &http.Client{Timeout: 15 * time.Second}}
}

// resolve returns the store API's header_image for appID, or "" if the API is
// unavailable, rate-limited, or has no art (the caller decides the fallback).
func (c *coverResolver) resolve(appID int) string {
	if d := coverThrottle - time.Since(c.last); d > 0 {
		time.Sleep(d)
	}
	defer func() { c.last = time.Now() }()

	u := fmt.Sprintf("https://store.steampowered.com/api/appdetails?appids=%d&filters=basic", appID)
	resp, err := c.http.Get(u)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "" // 429/5xx: skip for now, a later sync retries
	}
	var out map[string]struct {
		Success bool `json:"success"`
		Data    struct {
			HeaderImage string `json:"header_image"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ""
	}
	entry, ok := out[strconv.Itoa(appID)]
	if !ok || !entry.Success {
		return ""
	}
	return entry.Data.HeaderImage
}

// resolveCover picks the cover for a page we're about to create: the store
// API's real header image, or the legacy guessed URL as a fallback. The
// fallback self-heals on a later sync (coverNeedsResolve flags it) once the
// store API answers, so a page is never left coverless.
func (c *coverResolver) resolveCover(appID int) string {
	if u := c.resolve(appID); u != "" {
		return u
	}
	return coverURL(appID)
}

// coverNeedsResolve reports whether an existing page's cover should be
// (re)fetched: it's empty, or it's the legacy cdn.../steam/apps/{id}/header.jpg
// URL that 404s for newer titles. Already-resolved covers (under
// /store_item_assets/) and any user-set cover are left untouched.
func coverNeedsResolve(current string) bool {
	return current == "" || strings.Contains(current, "steamstatic.com/steam/apps/")
}

// ---------- Notion API ----------

// propNames holds the actual property names found in the user's database.
// They are resolved at runtime against propAliases, so the same binary
// works with the English template, the Spanish one, or future locales.
type propNames struct {
	Name       string
	AppID      string
	Hours      string
	LastPlayed string
	Recent     string
	Status     string
}

// propAliases: logical field → accepted names (any locale) + expected type.
var propAliases = []struct {
	field    string // which propNames field it fills
	notionTy string // expected Notion property type
	names    []string
}{
	{"Name", "title", []string{"Name", "Nombre"}},
	{"AppID", "number", []string{"AppID", "App ID"}},
	{"Hours", "number", []string{"Hours Played", "Horas jugadas"}},
	{"LastPlayed", "date", []string{"Last Played", "Última vez jugado", "Ultima vez jugado"}},
	{"Recent", "checkbox", []string{"Recent", "Reciente"}},
	{"Status", "select", []string{"Status", "Estado"}},
}

type notionClient struct {
	token string
	dbID  string
	http  *http.Client
	props propNames
}

// resolveSchema reads the database schema and maps each logical field to
// the actual property name via the alias table (case-insensitive).
// Called at the start of every sync, so renames are picked up live.
func (n *notionClient) resolveSchema() error {
	raw, err := n.do("GET", "/databases/"+n.dbID, nil)
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	var out struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("decode schema: %w", err)
	}

	// normalized name → (actual name, type)
	norm := func(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
	byNorm := make(map[string]struct{ actual, ty string }, len(out.Properties))
	for name, p := range out.Properties {
		byNorm[norm(name)] = struct{ actual, ty string }{name, p.Type}
	}

	resolved := propNames{}
	var missing []string
	for _, a := range propAliases {
		found := ""
		for _, cand := range a.names {
			if hit, ok := byNorm[norm(cand)]; ok && hit.ty == a.notionTy {
				found = hit.actual
				break
			}
		}
		if found == "" {
			missing = append(missing, fmt.Sprintf("%s (%s: %s)", a.field, a.notionTy, strings.Join(a.names, " / ")))
			continue
		}
		switch a.field {
		case "Name":
			resolved.Name = found
		case "AppID":
			resolved.AppID = found
		case "Hours":
			resolved.Hours = found
		case "LastPlayed":
			resolved.LastPlayed = found
		case "Recent":
			resolved.Recent = found
		case "Status":
			resolved.Status = found
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("database is missing expected properties: %s — check names and types against the template", strings.Join(missing, "; "))
	}
	n.props = resolved
	return nil
}

func (n *notionClient) do(method, path string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, notionAPI+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+n.token)
	req.Header.Set("Notion-Version", notionVersion)
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	// Notion returns 429 with Retry-After when we hit rate limit.
	if resp.StatusCode == 429 {
		wait := 2 * time.Second
		if s := resp.Header.Get("Retry-After"); s != "" {
			if secs, err := strconv.Atoi(s); err == nil {
				wait = time.Duration(secs) * time.Second
			}
		}
		log.Printf("notion 429, retrying in %s", wait)
		time.Sleep(wait)
		return n.do(method, path, payload)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("notion %s %s → %d: %s", method, path, resp.StatusCode, raw)
	}
	return raw, nil
}

// existingPage stores the minimal info needed to decide whether to update.
type existingPage struct {
	PageID string
	Hours  float64
	Recent bool
	Cover  string
}

type notionQueryResp struct {
	Results []struct {
		ID         string `json:"id"`
		Properties map[string]struct {
			Number   *float64 `json:"number"`
			Checkbox *bool    `json:"checkbox"`
		} `json:"properties"`
		Cover *struct {
			External *struct {
				URL string `json:"url"`
			} `json:"external"`
		} `json:"cover"`
	} `json:"results"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor"`
}

// loadExisting paginates the entire database once and builds an
// AppID → page index. Much cheaper than querying per game.
func (n *notionClient) loadExisting() (map[int]existingPage, error) {
	index := make(map[int]existingPage)
	payload := map[string]any{"page_size": 100}

	for {
		raw, err := n.do("POST", "/databases/"+n.dbID+"/query", payload)
		if err != nil {
			return nil, err
		}
		var out notionQueryResp
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, err
		}
		for _, r := range out.Results {
			appProp, ok := r.Properties[n.props.AppID]
			if !ok || appProp.Number == nil {
				continue // manual page without AppID: skip it, user-owned
			}
			ep := existingPage{PageID: r.ID}
			if h, ok := r.Properties[n.props.Hours]; ok && h.Number != nil {
				ep.Hours = *h.Number
			}
			if c, ok := r.Properties[n.props.Recent]; ok && c.Checkbox != nil {
				ep.Recent = *c.Checkbox
			}
			if r.Cover != nil && r.Cover.External != nil {
				ep.Cover = r.Cover.External.URL
			}
			index[int(*appProp.Number)] = ep
		}
		if !out.HasMore {
			break
		}
		payload["start_cursor"] = out.NextCursor
	}
	return index, nil
}

// syncedProps builds ONLY Steam-derived properties.
// Status, Rating, and Notes are never touched here: they're user-owned.
func (n *notionClient) syncedProps(g steamGame, recent bool) map[string]any {
	props := map[string]any{
		n.props.Hours:  map[string]any{"number": round1(float64(g.PlaytimeForever) / 60.0)},
		n.props.Recent: map[string]any{"checkbox": recent},
	}
	if g.RtimeLastPlayed > 0 {
		props[n.props.LastPlayed] = map[string]any{
			"date": map[string]any{"start": time.Unix(g.RtimeLastPlayed, 0).UTC().Format("2006-01-02")},
		}
	}
	return props
}

func (n *notionClient) createPage(g steamGame, recent bool, cov *coverResolver) error {
	props := n.syncedProps(g, recent)
	props[n.props.Name] = map[string]any{
		"title": []map[string]any{{"text": map[string]any{"content": g.Name}}},
	}
	props[n.props.AppID] = map[string]any{"number": g.AppID}
	// Initial Status only on CREATE; afterwards 100% user-owned.
	props[n.props.Status] = map[string]any{"select": map[string]any{"name": "Backlog"}}

	payload := map[string]any{
		"parent":     map[string]any{"database_id": n.dbID},
		"properties": props,
		"cover":      map[string]any{"external": map[string]any{"url": cov.resolveCover(g.AppID)}},
	}
	_, err := n.do("POST", "/pages", payload)
	return err
}

// updatePage patches the Steam-derived properties. It only touches the cover
// when a non-empty URL is passed (i.e. the existing one was missing or legacy);
// otherwise the page's current cover is left as-is.
func (n *notionClient) updatePage(pageID string, g steamGame, recent bool, cover string) error {
	payload := map[string]any{"properties": n.syncedProps(g, recent)}
	if cover != "" {
		payload["cover"] = map[string]any{"external": map[string]any{"url": cover}}
	}
	_, err := n.do("PATCH", "/pages/"+pageID, payload)
	return err
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

// ---------- Progress ----------

// progress reports how far the per-game loop has gotten. On a real terminal
// (e.g. `make run`) it draws an in-place bar; without a TTY (e.g. Railway logs)
// it emits a throttled log line instead, so captured output stays readable.
type progress struct {
	total   int
	tty     bool
	lastLog time.Time
}

func newProgress(total int) *progress {
	fi, _ := os.Stderr.Stat()
	tty := fi != nil && fi.Mode()&os.ModeCharDevice != 0
	return &progress{total: total, tty: tty, lastLog: time.Now()}
}

// update redraws the indicator after `done` of `total` games are processed.
func (p *progress) update(done int) {
	if p.total == 0 {
		return
	}
	if p.tty {
		const width = 30
		filled := done * width / p.total
		bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
		fmt.Fprintf(os.Stderr, "\rsync: [%s] %d/%d games", bar, done, p.total)
		return
	}
	// Non-TTY: one line at most every 2s, plus the final one.
	if done < p.total && time.Since(p.lastLog) < 2*time.Second {
		return
	}
	p.lastLog = time.Now()
	log.Printf("sync: %d/%d games processed", done, p.total)
}

// finish ends the bar's line so later logs start fresh (TTY only).
func (p *progress) finish() {
	if p.tty && p.total > 0 {
		fmt.Fprintln(os.Stderr)
	}
}

// ---------- Sync ----------

func runSync(steamKey, steamID string, notion *notionClient) {
	start := time.Now()
	log.Println("sync: starting")

	games, err := fetchOwnedGames(steamKey, steamID)
	if err != nil {
		log.Printf("sync aborted: %v", err)
		return
	}
	if err := notion.resolveSchema(); err != nil {
		log.Printf("sync aborted: %v", err)
		return
	}
	log.Printf("schema resolved: hours=%q recent=%q lastPlayed=%q status=%q",
		notion.props.Hours, notion.props.Recent, notion.props.LastPlayed, notion.props.Status)
	recent, err := fetchRecentAppIDs(steamKey, steamID)
	if err != nil {
		log.Printf("warning: unable to read recent games: %v", err)
		recent = map[int]bool{}
	}
	existing, err := notion.loadExisting()
	if err != nil {
		log.Printf("sync aborted: %v", err)
		return
	}
	log.Printf("steam: %d games | notion: %d pages indexed", len(games), len(existing))

	covers := newCoverResolver()
	created, updated, skipped, fixedCovers, failed := 0, 0, 0, 0, 0
	bar := newProgress(len(games))
	for i, g := range games {
		bar.update(i)
		hours := round1(float64(g.PlaytimeForever) / 60.0)
		isRecent := recent[g.AppID]

		if ep, ok := existing[g.AppID]; ok {
			needCover := coverNeedsResolve(ep.Cover)
			// No changes → no request. This keeps subsequent syncs cheap.
			if ep.Hours == hours && ep.Recent == isRecent && !needCover {
				skipped++
				continue
			}
			cover := ""
			if needCover {
				// "" if the store API can't answer: leave the current cover
				// untouched and retry on a later sync.
				if cover = covers.resolve(g.AppID); cover != "" {
					fixedCovers++
				}
			}
			if err := notion.updatePage(ep.PageID, g, isRecent, cover); err != nil {
				log.Printf("update %q: %v", g.Name, err)
				failed++
			} else {
				updated++
			}
		} else {
			if err := notion.createPage(g, isRecent, covers); err != nil {
				log.Printf("create %q: %v", g.Name, err)
				failed++
			} else {
				created++
			}
		}
		time.Sleep(notionThrottle)
	}
	bar.update(len(games))
	bar.finish()
	log.Printf("sync: completed in %s — created %d, updated %d, unchanged %d, covers fixed %d, errors %d",
		time.Since(start).Round(time.Second), created, updated, skipped, fixedCovers, failed)
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("missing environment variable %s", key)
	}
	return v
}

func main() {
	// -once: sync a single time and exit, instead of looping on the ticker.
	// Handy for local runs, cron, or CI schedulers (e.g. GitHub Actions).
	// Also enabled via SYNC_ONCE=true for env-only environments.
	once := flag.Bool("once", false, "run a single sync and exit (no ticker loop)")
	flag.Parse()
	if s := os.Getenv("SYNC_ONCE"); !*once && (s == "true" || s == "1") {
		*once = true
	}

	steamKey := mustEnv("STEAM_API_KEY")
	steamID := mustEnv("STEAM_ID64")
	notion := &notionClient{
		token: mustEnv("NOTION_TOKEN"),
		dbID:  mustEnv("NOTION_DB_ID"),
		http:  &http.Client{Timeout: 30 * time.Second},
	}

	runSync(steamKey, steamID, notion) // immediate sync on startup
	if *once {
		return
	}

	intervalHours := 6
	if s := os.Getenv("SYNC_INTERVAL_HOURS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			intervalHours = n
		}
	}
	interval := time.Duration(intervalHours) * time.Hour

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	log.Printf("next sync in %s", interval)
	for range ticker.C {
		runSync(steamKey, steamID, notion)
		log.Printf("next sync in %s", interval)
	}
}
