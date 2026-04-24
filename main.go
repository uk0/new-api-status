package main

import (
	"database/sql"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/mattn/go-sqlite3"
)

//go:embed ui/dist
var uiFS embed.FS

// --- Config ---

type Config struct {
	DSN            string
	Driver         string // postgres, mysql, sqlite3
	Listen         string
	PollInterval   time.Duration
	TurnstileSite  string
	TurnstileSecret string
}

func loadConfig() *Config {
	c := &Config{}
	flag.StringVar(&c.DSN, "dsn", os.Getenv("SQL_DSN"), "database DSN")
	flag.StringVar(&c.Driver, "driver", os.Getenv("DB_DRIVER"), "database driver: postgres, mysql, sqlite3")
	flag.StringVar(&c.Listen, "listen", envOr("LISTEN", ":8787"), "listen address")
	flag.StringVar(&c.TurnstileSite, "turnstile-site", os.Getenv("TURNSTILE_SITE_KEY"), "Cloudflare Turnstile site key")
	flag.StringVar(&c.TurnstileSecret, "turnstile-secret", os.Getenv("TURNSTILE_SECRET_KEY"), "Cloudflare Turnstile secret key")
	poll := flag.Int("poll", envInt("POLL_INTERVAL", 30), "poll interval in seconds")
	flag.Parse()
	c.PollInterval = time.Duration(*poll) * time.Second
	if c.Driver == "" {
		c.Driver = detectDriver(c.DSN)
	}
	return c
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	var n int
	fmt.Sscanf(v, "%d", &n)
	if n <= 0 {
		return def
	}
	return n
}

func detectDriver(dsn string) string {
	if strings.HasPrefix(dsn, "postgres") {
		return "postgres"
	}
	if strings.Contains(dsn, "@tcp(") || strings.HasPrefix(dsn, "mysql://") {
		return "mysql"
	}
	return "sqlite3"
}

func normalizeDSN(driver, dsn string) string {
	if driver == "mysql" {
		dsn = strings.TrimPrefix(dsn, "mysql://")
	}
	return dsn
}

// --- Data types ---

type MinuteStat struct {
	MinuteTs    int64   `json:"minuteTs"`
	Total       int     `json:"total"`
	Success     int     `json:"success"`
	SuccessRate float64 `json:"successRate"`
}

type Availability struct {
	Minutes       []MinuteStat `json:"minutes"`
	OverallRate   float64      `json:"overallRate"`
	TotalRequests int          `json:"totalRequests"`
	TotalSuccess  int          `json:"totalSuccess"`
}

type ChannelInfo struct {
	Name        string                  `json:"name"`
	OverallRate float64                 `json:"overallRate"`
	Total       int                     `json:"totalRequests"`
	Success     int                     `json:"totalSuccess"`
	Models      map[string]*Availability `json:"models"`
}

type StatusData struct {
	Groups   map[string]*Availability `json:"groups"`
	Channels map[string]*ChannelInfo  `json:"channels"`
	UpdatedAt int64                   `json:"updatedAt"`
}

// --- Store (in-memory, refreshed by polling) ---

type Store struct {
	mu   sync.RWMutex
	data *StatusData
}

func (s *Store) Get() *StatusData {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data
}

func (s *Store) Set(d *StatusData) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = d
}

// --- DB poller ---

type logRow struct {
	createdAt   int64
	logType     int
	group       string
	channelId   int
	channelName string
	modelName   string
}

func pollDB(db *sql.DB, driver string) *StatusData {
	now := time.Now().Unix()
	since := now - 3600

	placeholder := "$1"
	channelJoin := `LEFT JOIN channels c ON l.channel_id = c.id`
	groupCol := `l."group"`
	if driver == "mysql" {
		placeholder = "?"
		groupCol = "l.`group`"
	} else if driver == "sqlite3" {
		placeholder = "?"
		groupCol = "l.`group`"
	}

	query := fmt.Sprintf(
		`SELECT l.created_at, l.type, %s, l.channel_id, COALESCE(c.name, ''), l.model_name
		 FROM logs l %s
		 WHERE l.created_at >= %s AND l.type IN (2, 5)
		 ORDER BY l.created_at`,
		groupCol, channelJoin, placeholder,
	)

	rows, err := db.Query(query, since)
	if err != nil {
		log.Printf("[poll] query error: %v", err)
		return nil
	}
	defer rows.Close()

	type minuteKey struct {
		prefix string
		ts     int64
	}
	type counter struct{ total, success int }
	buckets := make(map[minuteKey]*counter)

	inc := func(prefix string, ts int64, isSuccess bool) {
		mk := minuteKey{prefix, ts}
		c := buckets[mk]
		if c == nil {
			c = &counter{}
			buckets[mk] = c
		}
		c.total++
		if isSuccess {
			c.success++
		}
	}

	channelNames := make(map[int]string)

	for rows.Next() {
		var r logRow
		if err := rows.Scan(&r.createdAt, &r.logType, &r.group, &r.channelId, &r.channelName, &r.modelName); err != nil {
			continue
		}
		minuteTs := (r.createdAt / 60) * 60
		isSuccess := r.logType == 2

		if r.group != "" {
			inc("g:"+r.group, minuteTs, isSuccess)
		}
		if r.channelName != "" {
			channelNames[r.channelId] = r.channelName
			inc("c:"+r.channelName, minuteTs, isSuccess)
			if r.modelName != "" {
				inc("c:"+r.channelName+"|"+r.modelName, minuteTs, isSuccess)
			}
		}
	}

	data := &StatusData{
		Groups:    make(map[string]*Availability),
		Channels:  make(map[string]*ChannelInfo),
		UpdatedAt: now,
	}

	groupCutoff := ((now / 60) - 59) * 60
	channelCutoff := ((now / 60) - 59) * 60

	for mk, c := range buckets {
		rate := float64(0)
		if c.total > 0 {
			rate = float64(c.success) / float64(c.total) * 100
		}
		ms := MinuteStat{MinuteTs: mk.ts, Total: c.total, Success: c.success, SuccessRate: rate}

		p := mk.prefix
		if strings.HasPrefix(p, "g:") {
			if mk.ts < groupCutoff {
				continue
			}
			name := p[2:]
			a := data.Groups[name]
			if a == nil {
				a = &Availability{}
				data.Groups[name] = a
			}
			a.Minutes = append(a.Minutes, ms)
			a.TotalRequests += c.total
			a.TotalSuccess += c.success
		} else if strings.HasPrefix(p, "c:") {
			if mk.ts < channelCutoff {
				continue
			}
			rest := p[2:]
			parts := strings.SplitN(rest, "|", 2)
			chName := parts[0]
			ci := data.Channels[chName]
			if ci == nil {
				ci = &ChannelInfo{Name: chName, Models: make(map[string]*Availability)}
				data.Channels[chName] = ci
			}
			if len(parts) == 1 {
				ci.Total += c.total
				ci.Success += c.success
			} else {
				modelName := parts[1]
				ma := ci.Models[modelName]
				if ma == nil {
					ma = &Availability{}
					ci.Models[modelName] = ma
				}
				ma.Minutes = append(ma.Minutes, ms)
				ma.TotalRequests += c.total
				ma.TotalSuccess += c.success
			}
		}
	}

	for _, a := range data.Groups {
		if a.TotalRequests > 0 {
			a.OverallRate = float64(a.TotalSuccess) / float64(a.TotalRequests) * 100
		}
		sort.Slice(a.Minutes, func(i, j int) bool { return a.Minutes[i].MinuteTs < a.Minutes[j].MinuteTs })
	}
	for _, ci := range data.Channels {
		if ci.Total > 0 {
			ci.OverallRate = float64(ci.Success) / float64(ci.Total) * 100
		}
		for _, ma := range ci.Models {
			if ma.TotalRequests > 0 {
				ma.OverallRate = float64(ma.TotalSuccess) / float64(ma.TotalRequests) * 100
			}
			sort.Slice(ma.Minutes, func(i, j int) bool { return ma.Minutes[i].MinuteTs < ma.Minutes[j].MinuteTs })
		}
	}

	return data
}

// --- Turnstile verification ---

func verifyTurnstile(secret, token, ip string) bool {
	resp, err := http.PostForm("https://challenges.cloudflare.com/turnstile/v0/siteverify", map[string][]string{
		"secret":   {secret},
		"response": {token},
		"remoteip": {ip},
	})
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var result struct{ Success bool `json:"success"` }
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Success
}

// --- HTTP handlers ---

func jsonResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func handleConfig(cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonResp(w, map[string]any{
			"turnstileSiteKey": cfg.TurnstileSite,
		})
	}
}

func handleStatus(store *Store, cfg *Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.TurnstileSecret != "" {
			token := r.URL.Query().Get("token")
			if token == "" {
				http.Error(w, `{"error":"missing turnstile token"}`, 403)
				return
			}
			if !verifyTurnstile(cfg.TurnstileSecret, token, r.RemoteAddr) {
				http.Error(w, `{"error":"turnstile verification failed"}`, 403)
				return
			}
		}
		d := store.Get()
		if d == nil {
			jsonResp(w, map[string]any{"groups": map[string]any{}, "channels": map[string]any{}, "updatedAt": 0})
			return
		}
		jsonResp(w, d)
	}
}

// --- Main ---

func main() {
	cfg := loadConfig()

	if cfg.DSN == "" {
		log.Fatal("DSN is required. Set SQL_DSN env or use -dsn flag")
	}

	db, err := sql.Open(cfg.Driver, normalizeDSN(cfg.Driver, cfg.DSN))
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		log.Fatalf("failed to ping database: %v", err)
	}
	log.Printf("connected to database (%s)", cfg.Driver)

	store := &Store{}

	go func() {
		for {
			if d := pollDB(db, cfg.Driver); d != nil {
				store.Set(d)
				log.Printf("[poll] refreshed: %d groups, %d channels", len(d.Groups), len(d.Channels))
			}
			time.Sleep(cfg.PollInterval)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/status/config", handleConfig(cfg))
	mux.HandleFunc("/api/status", handleStatus(store, cfg))

	uiDist, _ := fs.Sub(uiFS, "ui/dist")
	fileServer := http.FileServer(http.FS(uiDist))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && !strings.HasPrefix(r.URL.Path, "/assets") {
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	})

	log.Printf("listening on %s", cfg.Listen)
	log.Fatal(http.ListenAndServe(cfg.Listen, mux))
}
