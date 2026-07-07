package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"time"
)

// ---- Configuration via environment variables (loaded from .env) ----
var (
	sites []string

	checkInterval time.Duration
	httpTimeout   time.Duration

	smtpHost string
	smtpPort string
	smtpUser string
	smtpPass string
	mailFrom string
	fromName string
	mailTo   string
	mailCc   string

	realertEvery time.Duration

	// Body substrings that mean the site is broken even with HTTP 200
	errorPhrases = []string{
		"There has been a critical error on this website",
		"Error establishing a database connection",
		// Cloudflare origin/edge error pages (520–527, 530)
		"Web server is returning an unknown error",  // 520
		"Web server is down",                        // 521
		"Web server is returning a connection error", // 522 variant
		"Origin is unreachable",                     // 523
		"A timeout occurred",                        // 524
		"SSL handshake failed",                      // 525
		"Invalid SSL certificate",                   // 526
		"Railgun Listener to origin error",          // 527
		"Origin DNS error",                          // 530
		"The web server is not returning a connection",
		// WordPress fatal / PHP error pages
		"There has been a critical error on your website",           // fatal handler variant
		"This site is experiencing technical difficulties",          // WP recovery mode
		"Briefly unavailable for scheduled maintenance",             // .maintenance file
		"Fatal error:",                                              // PHP fatal
		"Parse error:",                                              // PHP parse
		"Allowed memory size of",                                    // PHP memory exhausted
		"Maximum execution time of",                                 // PHP timeout
		"Cannot modify header information",                          // headers already sent
		"Uncaught Error:",                                           // PHP 7+ uncaught
	}
)

// loadConfig reads all settings from the environment (populated from .env
// by loadDotEnv, or from the real environment). Secrets have no defaults.
func loadConfig() {
	sites = envList("MONITOR_URLS", "https://bigtree-group.com,https://shop.bigtree-group.com")

	checkInterval = 1 * time.Minute
	httpTimeout = 20 * time.Second

	smtpHost = env("SMTP_HOST", "")
	smtpPort = env("SMTP_PORT", "")
	smtpUser = env("SMTP_USER", "") // SMTP auth user
	smtpPass = env("SMTP_PASS", "") // app password
	mailFrom = env("MAIL_FROM", "") // envelope sender
	fromName = env("MAIL_FROM_NAME", "") 
	mailTo = env("MAIL_TO", "") // primary recipient(s), comma-separated
	mailCc = env("MAIL_CC", "") // carbon-copy recipient(s), comma-separated

	// Re-alert at most once per this window while a site stays down,
	// and send a recovery email when it comes back.
	realertEvery = 60 * time.Minute
}

type siteState struct {
	down      bool
	lastAlert time.Time
}

var states = map[string]*siteState{}

func main() {
	loadDotEnv(".env")
	loadConfig()
	if mailTo == "" || smtpUser == "" || smtpPass == "" {
		log.Fatal("SMTP_USER, SMTP_PASS and MAIL_TO must be set)")
	}
	for _, s := range sites {
		states[s] = &siteState{}
	}
	log.Printf("monitoring %v every %s", sites, checkInterval)

	checkAll() // immediate first check
	for range time.Tick(checkInterval) {
		checkAll()
	}
}

func checkAll() {
	for _, url := range sites {
		ok, reason := checkSite(url)
		st := states[url]
		switch {
		case !ok && (!st.down || time.Since(st.lastAlert) >= realertEvery):
			st.down = true
			st.lastAlert = time.Now()
			log.Printf("DOWN %s: %s", url, reason)
			sendMail(
				fmt.Sprintf("🔴 DOWN: %s", url),
				fmt.Sprintf("Site: %s\nTime: %s\nReason: %s\n", url, time.Now().Format(time.RFC1123), reason),
			)
		case !ok:
			log.Printf("still down %s: %s (alert suppressed)", url, reason)
		case ok && st.down:
			st.down = false
			log.Printf("RECOVERED %s", url)
			sendMail(
				fmt.Sprintf("🟢 RECOVERED: %s", url),
				fmt.Sprintf("Site: %s is back up at %s\n", url, time.Now().Format(time.RFC1123)),
			)
		default:
			log.Printf("OK %s", url)
		}
	}
}

// checkSite returns ok=false with a reason on: network error, HTTP >= 500,
// or known WordPress error phrases in the body.
func checkSite(url string) (bool, string) {
	client := &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "BigtreeMonitor/1.0")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := client.Do(req)
	if err != nil {
		return false, "request failed: " + err.Error()
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return false, fmt.Sprintf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20)) // read up to 2 MB
	if err != nil {
		return false, "failed reading body: " + err.Error()
	}
	text := string(body)
	for _, phrase := range errorPhrases {
		if strings.Contains(text, phrase) {
			return false, fmt.Sprintf("HTTP %d but body contains: %q", resp.StatusCode, phrase)
		}
	}
	return true, ""
}

func sendMail(subject, body string) {
	headers := []string{
		"From: " + fromName + " <" + mailFrom + ">",
		"To: " + mailTo,
	}
	if mailCc != "" {
		headers = append(headers, "Cc: "+mailCc)
	}
	headers = append(headers,
		"Subject: "+subject,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"",
		body,
	)
	msg := strings.Join(headers, "\r\n")

	// Envelope recipients must include Cc — the Cc header alone does not deliver.
	rcpts := splitAddrs(mailTo)
	rcpts = append(rcpts, splitAddrs(mailCc)...)

	auth := smtp.PlainAuth("", smtpUser, smtpPass, smtpHost)
	err := smtp.SendMail(smtpHost+":"+smtpPort, auth, mailFrom, rcpts, []byte(msg))
	if err != nil {
		log.Printf("email send failed: %v", err)
	}
}

// splitAddrs splits a comma-separated address list, trimming blanks.
func splitAddrs(list string) []string {
	var out []string
	for _, a := range strings.Split(list, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// ---- env helpers ----

// loadDotEnv loads KEY=VALUE lines from path into the process environment.
// Existing environment variables are never overwritten, so the real env wins
// over the file. A missing file is not an error (env-only deploys still work).
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, val)
		}
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envList(key, def string) []string {
	raw := env(key, def)
	var out []string
	for _, s := range strings.Split(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
