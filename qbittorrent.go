package main

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/superturkey650/go-qbittorrent/qbt"
)

// basicAuthTransport wraps an http.RoundTripper and injects HTTP Basic Auth
// for a specific host. All other requests are forwarded unchanged.
// This is needed when qBittorrent sits behind a reverse proxy (nginx, openresty,
// etc.) that requires Basic Auth before forwarding to the qBittorrent WebUI.
//
// The qbt library's internal http.Client sets Transport = nil, which causes
// Go's net/http to use http.DefaultTransport at call time. Replacing
// http.DefaultTransport here is therefore sufficient to intercept all qbt
// requests without needing to modify the library.
type basicAuthTransport struct {
	host     string
	username string
	password string
	next     http.RoundTripper
}

func (t *basicAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == t.host {
		req = req.Clone(req.Context())
		req.SetBasicAuth(t.username, t.password)
	}
	return t.next.RoundTrip(req)
}

// Monitor polls qBittorrent and notifies the Matrix bot about torrent events.
type Monitor struct {
	client   *qbt.Client
	cfg      QBittorrentConfig
	bot      *MatrixBot
	interval time.Duration
	// known maps info hash → last known state.
	known map[string]qbt.TorrentInfo
}

func NewMonitor(cfg QBittorrentConfig, bot *MatrixBot) (*Monitor, error) {
	if cfg.HTTPUsername != "" {
		u, err := url.Parse(cfg.URL)
		if err != nil {
			return nil, fmt.Errorf("parsing qbittorrent URL: %w", err)
		}
		http.DefaultTransport = &basicAuthTransport{
			host:     u.Host,
			username: cfg.HTTPUsername,
			password: cfg.HTTPPassword,
			next:     http.DefaultTransport,
		}
		log.Printf("HTTP Basic Auth enabled for host %s", u.Host)
	}

	client := qbt.NewClient(cfg.URL)

	if err := login(client, cfg); err != nil {
		return nil, err
	}

	return &Monitor{
		client:   client,
		cfg:      cfg,
		bot:      bot,
		interval: time.Duration(cfg.PollInterval) * time.Second,
		known:    make(map[string]qbt.TorrentInfo),
	}, nil
}

// login authenticates and verifies the connection by fetching the app version.
// The qbt library's Login() only checks for HTTP 403; it does not inspect the
// response body ("Ok." vs "Fails."), so a bad-credentials login silently
// succeeds. Calling ApplicationVersion() afterwards catches that, and also
// detects HTML responses caused by missing/wrong HTTP Basic Auth.
func login(client *qbt.Client, cfg QBittorrentConfig) error {
	if err := client.Login(cfg.Username, cfg.Password); err != nil {
		return fmt.Errorf("qbittorrent login request: %w", err)
	}

	version, err := client.ApplicationVersion()
	if err != nil {
		return fmt.Errorf("qbittorrent connection check (URL: %s): %w", cfg.URL, err)
	}

	version = strings.TrimSpace(version)
	if strings.HasPrefix(version, "<") {
		hint := ""
		if strings.Contains(version, "401") {
			hint = " — HTTP 401: add http_username/http_password to config for the reverse proxy"
		} else if strings.Contains(version, "403") {
			hint = " — HTTP 403: IP may be banned after too many failed logins"
		}
		return fmt.Errorf("qbittorrent returned HTML instead of a version string%s\nURL: %s\nResponse: %.200s",
			hint, cfg.URL, version)
	}

	log.Printf("Connected to qBittorrent %s at %s", version, cfg.URL)
	return nil
}

// Start takes an initial snapshot of existing torrents (so we don't spam the
// room on startup) then polls on every interval tick.
func (m *Monitor) Start(ctx context.Context) {
	if err := m.snapshot(); err != nil {
		log.Printf("initial qbittorrent snapshot: %v", err)
	} else {
		log.Printf("qbittorrent monitor started, tracking %d existing torrent(s)", len(m.known))
	}

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.poll(ctx); err != nil {
				log.Printf("poll error: %v", err)
			}
		}
	}
}

func (m *Monitor) snapshot() error {
	torrents, err := m.fetchTorrents()
	if err != nil {
		return err
	}
	for _, t := range torrents {
		m.known[t.Hash] = t
	}
	return nil
}

func (m *Monitor) poll(ctx context.Context) error {
	torrents, err := m.fetchTorrents()
	if err != nil {
		return err
	}

	current := make(map[string]qbt.TorrentInfo, len(torrents))
	for _, t := range torrents {
		current[t.Hash] = t
	}

	// Detect new torrents.
	for hash, t := range current {
		if _, exists := m.known[hash]; !exists {
			msg := fmt.Sprintf("⬇️ New download added: **%s** (%s)", t.Name, formatSize(t.Size))
			if err := m.bot.SendNotice(ctx, msg); err != nil {
				log.Printf("send notice: %v", err)
			}
		}
	}

	// Detect completed, seeding, and errored torrents.
	for hash, t := range current {
		prev, exists := m.known[hash]
		if !exists {
			continue
		}
		if prev.Progress < 1.0 && t.Progress >= 1.0 {
			msg := fmt.Sprintf("✅ Download completed: **%s** (%s)", t.Name, formatSize(t.Size))
			if err := m.bot.SendNotice(ctx, msg); err != nil {
				log.Printf("send notice: %v", err)
			}
		}
		// Started seeding: entered a seeding state from a non-seeding state.
		// Guard with prev.Progress >= 1.0 to avoid firing on the same tick as
		// "completed" (downloading→uploading).
		if prev.Progress >= 1.0 && !isSeedingState(prev.State) && isSeedingState(t.State) {
			msg := fmt.Sprintf("🌱 Started seeding: **%s**", t.Name)
			if err := m.bot.SendNotice(ctx, msg); err != nil {
				log.Printf("send notice: %v", err)
			}
		}
		// Stopped seeding: left a seeding state while the torrent still exists.
		if isSeedingState(prev.State) && !isSeedingState(t.State) {
			msg := fmt.Sprintf("⏸️ Stopped seeding: **%s**", t.Name)
			if err := m.bot.SendNotice(ctx, msg); err != nil {
				log.Printf("send notice: %v", err)
			}
		}
		if prev.State != "error" && t.State == "error" {
			msg := fmt.Sprintf("❌ Download error: **%s**", t.Name)
			if err := m.bot.SendNotice(ctx, msg); err != nil {
				log.Printf("send notice: %v", err)
			}
		}
	}

	// Detect removed torrents.
	for hash, t := range m.known {
		if _, exists := current[hash]; !exists {
			msg := fmt.Sprintf("🗑️ Download removed: **%s**", t.Name)
			if err := m.bot.SendNotice(ctx, msg); err != nil {
				log.Printf("send notice: %v", err)
			}
		}
	}

	m.known = current
	return nil
}

// fetchTorrents calls the API and re-logins once if the session appears expired
// (the server returns an HTML page instead of JSON).
func (m *Monitor) fetchTorrents() ([]qbt.TorrentInfo, error) {
	torrents, err := m.client.Torrents(qbt.TorrentsOptions{})
	if err == nil {
		return torrents, nil
	}

	// An HTML response from qBittorrent (e.g. the login page) causes a JSON
	// decode error starting with "invalid character '<'". Treat this as a
	// session expiry and attempt one re-login.
	if strings.Contains(err.Error(), "invalid character '<'") {
		log.Printf("qBittorrent session expired, re-logging in")
		if loginErr := login(m.client, m.cfg); loginErr != nil {
			return nil, fmt.Errorf("re-login failed: %w", loginErr)
		}
		torrents, err = m.client.Torrents(qbt.TorrentsOptions{})
		if err != nil {
			return nil, fmt.Errorf("fetching torrents after re-login: %w", err)
		}
		return torrents, nil
	}

	return nil, fmt.Errorf("fetching torrents: %w", err)
}

func formatSize(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
		TB = GB * 1024
	)
	switch {
	case bytes >= TB:
		return fmt.Sprintf("%.2f TB", float64(bytes)/TB)
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/GB)
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", float64(bytes)/MB)
	case bytes >= KB:
		return fmt.Sprintf("%.2f KB", float64(bytes)/KB)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func formatUnixTime(ts int64) string {
	if ts <= 0 {
		return "—"
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04")
}

func formatRelativeTime(ts int64) string {
	if ts <= 0 {
		return "—"
	}
	total := int64(time.Since(time.Unix(ts, 0)).Seconds())
	if total < 60 {
		return "just now"
	}
	years := total / (365 * 24 * 3600)
	total %= 365 * 24 * 3600
	months := total / (30 * 24 * 3600)
	total %= 30 * 24 * 3600
	days := total / (24 * 3600)
	total %= 24 * 3600
	hours := total / 3600

	pluralUnit := func(n int64, unit string) string {
		if n == 1 {
			return fmt.Sprintf("1 %s", unit)
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}

	var parts []string
	if years > 0 {
		parts = append(parts, pluralUnit(years, "year"))
	}
	if months > 0 {
		parts = append(parts, pluralUnit(months, "month"))
	}
	if days > 0 {
		parts = append(parts, pluralUnit(days, "day"))
	}
	if hours > 0 {
		parts = append(parts, pluralUnit(hours, "hour"))
	}
	if len(parts) == 0 {
		return "just now"
	}
	return strings.Join(parts, ", ") + " ago"
}

func formatSpeed(bps int64) string {
	if bps == 0 {
		return "0 B/s"
	}
	return formatSize(bps) + "/s"
}

// isSeedingState reports whether the torrent is in any seeding-related state
// (actively uploading, stalled waiting for peers, queued, or being rechecked
// after completion). Transitions into/out of this set trigger seeding notices.
func isSeedingState(state string) bool {
	switch state {
	case "uploading", "forcedUP", "stalledUP", "queuedUP", "checkingUP":
		return true
	}
	return false
}

func formatState(state string) string {
	states := map[string]string{
		"downloading":        "Downloading",
		"uploading":          "Seeding",
		"stalledDL":          "Stalled ↓",
		"stalledUP":          "Stalled ↑",
		"pausedDL":           "Paused ↓",
		"pausedUP":           "Paused ↑",
		"queuedDL":           "Queued ↓",
		"queuedUP":           "Queued ↑",
		"checkingDL":         "Checking ↓",
		"checkingUP":         "Checking ↑",
		"checkingResumeData": "Resuming",
		"forcedDL":           "Forced ↓",
		"forcedUP":           "Forced ↑",
		"metaDL":             "Getting metadata",
		"error":              "Error",
		"missingFiles":       "Missing files",
		"moving":             "Moving",
		"unknown":            "Unknown",
	}
	if s, ok := states[state]; ok {
		return s
	}
	return state
}

// RegisterCommands wires up all monitor commands on the bot.
// Must be called before bot.Start().
func (m *Monitor) RegisterCommands(bot *MatrixBot) {
	bot.RegisterCommand("list", m.cmdList)
}

func (m *Monitor) cmdList(ctx context.Context, _ string) (plain, htmlBody string, err error) {
	torrents, err := m.fetchTorrents()
	if err != nil {
		return "", "", err
	}

	sort.Slice(torrents, func(i, j int) bool {
		return strings.ToLower(torrents[i].Name) < strings.ToLower(torrents[j].Name)
	})

	var buf bytes.Buffer
	buf.WriteString("<table><thead><tr>")
	for _, h := range []string{"Name", "Status", "Progress", "Size", "Seeds", "Peers", "↓ Speed", "↑ Speed", "Ratio", "Added On", "Last Activity"} {
		fmt.Fprintf(&buf, "<th>%s</th>", h)
	}
	buf.WriteString("</tr></thead><tbody>")

	var plainRows []string
	for _, t := range torrents {
		progress := fmt.Sprintf("%.1f%%", t.Progress*100)
		size := formatSize(t.Size)
		dl := formatSpeed(t.Dlspeed)
		up := formatSpeed(t.Upspeed)
		ratio := fmt.Sprintf("%.2f", t.Ratio)
		state := formatState(t.State)
		addedOn := formatUnixTime(t.AddedOn)
		lastActivity := formatRelativeTime(t.LastActivity)

		buf.WriteString("<tr>")
		for _, cell := range []string{
			html.EscapeString(t.Name),
			html.EscapeString(state),
			progress,
			size,
			fmt.Sprintf("%d", t.NumSeeds),
			fmt.Sprintf("%d", t.NumLeechs),
			dl,
			up,
			ratio,
			addedOn,
			lastActivity,
		} {
			fmt.Fprintf(&buf, "<td>%s</td>", cell)
		}
		buf.WriteString("</tr>")

		plainRows = append(plainRows, fmt.Sprintf(
			"%-60s | %-18s | %6s | %9s | %4d seeds | %4d peers | ↓ %-10s ↑ %-10s | ratio %s | added %s | active %s",
			t.Name, state, progress, size, t.NumSeeds, t.NumLeechs, dl, up, ratio, addedOn, lastActivity,
		))
	}

	buf.WriteString("</tbody></table>")

	header := fmt.Sprintf("%d torrent(s)\n", len(torrents))
	return header + strings.Join(plainRows, "\n"), buf.String(), nil
}
