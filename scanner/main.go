package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ─── Types ─────────────────────────────────────────────────────────────────

type Result struct {
	Domain string
	Count  int
	Ok     bool
}

type Config struct {
	DomainsFile string
	OutDir      string
	Webhook     string
	Repo        string
	RunID       string
}

// ─── Entry point ───────────────────────────────────────────────────────────

func main() {
	if len(os.Args) > 1 && os.Args[1] == "notify" {
		runNotify(os.Args[2:])
		return
	}
	runScan(os.Args[1:])
}

// ─────────────────────────────────────────────────────────────────────────
// SUBCOMMAND: scan  (default, no subcommand needed)
// ─────────────────────────────────────────────────────────────────────────

func runScan(args []string) {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	cfg := Config{}
	fs.StringVar(&cfg.DomainsFile, "domains", "domains.txt", "File with one domain per line")
	fs.StringVar(&cfg.OutDir, "out", "results", "Output directory")
	fs.StringVar(&cfg.Webhook, "webhook", "", "Discord webhook URL (chunk notification)")
	fs.StringVar(&cfg.Repo, "repo", env("GITHUB_REPOSITORY", ""), "GitHub repo")
	fs.StringVar(&cfg.RunID, "run-id", env("GITHUB_RUN_ID", ""), "GitHub run ID")
	_ = fs.Parse(args)

	domains := readLines(cfg.DomainsFile)
	if len(domains) == 0 {
		fatalf("❌ No domains found in %s\n", cfg.DomainsFile)
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		fatalf("❌ Cannot create output dir: %v\n", err)
	}

	total := len(domains)
	fmt.Printf("📋 Chunk has %d domain(s) to scan\n\n", total)

	results := make([]Result, 0, total)
	startAll := time.Now()

	for i, domain := range domains {
		fmt.Printf("[%d/%d] 🔍 Scanning: %s\n", i+1, total, domain)
		t0 := time.Now()

		urls, err := runGau(domain)
		elapsed := time.Since(t0).Round(time.Second)

		if err != nil {
			fmt.Printf("         ❌ Error: %v  (%s)\n", err, elapsed)
			results = append(results, Result{domain, 0, false})
			continue
		}

		if err := writeLines(filepath.Join(cfg.OutDir, safeName(domain)+".txt"), urls); err != nil {
			fmt.Printf("         ⚠️  Write error: %v\n", err)
		}

		results = append(results, Result{domain, len(urls), true})
	}

	totalURLs := 0
	for _, r := range results {
		totalURLs += r.Count
	}

	elapsed := time.Since(startAll).Round(time.Second)
	fmt.Printf("\n🎉 Chunk Done! %s total URLs found from current domains  (%s elapsed)\n", fmtNum(totalURLs), elapsed)

	// ── Discord chunk notification ──────────────────────────────────────────
	if cfg.Webhook != "" {
		sendChunkNotif(cfg, results, totalURLs)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// SUBCOMMAND: notify  (final merged summary)
// ─────────────────────────────────────────────────────────────────────────

func runNotify(args []string) {
	fs := flag.NewFlagSet("notify", flag.ExitOnError)
	total := fs.Int("total", 0, "Total unique URLs in merged file")
	webhook := fs.String("webhook", "", "Discord webhook URL")
	repo := fs.String("repo", env("GITHUB_REPOSITORY", ""), "GitHub repo")
	runID := fs.String("run-id", env("GITHUB_RUN_ID", ""), "GitHub run ID")
	_ = fs.Parse(args)

	if *webhook == "" {
		fmt.Println("⚠️  No webhook — skipping Discord notification")
		return
	}

	runURL := fmt.Sprintf("https://github.com/%s/actions/runs/%s", *repo, *runID)
	now := time.Now().UTC().Format("2006-01-02 15:04 UTC")

	embed := map[string]any{
		"title": "✅ GAU Scan — Complete!",
		"color": 0x5865F2,
		"description": fmt.Sprintf(
			"📊 **%s unique URLs** across all domains\n[📂 Download `all_urls.txt`](%s)",
			fmtNum(*total), runURL,
		),
		"footer": map[string]string{"text": fmt.Sprintf("Finished at %s", now)},
	}

	sendEmbed(*webhook, embed)
	fmt.Printf("✅ Final summary sent — %s total URLs\n", fmtNum(*total))
}

// ─── GAU runner ────────────────────────────────────────────────────────────

func runGau(domain string) ([]string, error) {
	cmd := exec.Command("gau", domain)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if se := strings.TrimSpace(stderr.String()); se != "" {
			return nil, fmt.Errorf("%v — %s", err, se)
		}
		return nil, err
	}
	return dedup(&stdout), nil
}

// ─── Discord helpers ───────────────────────────────────────────────────────

func sendChunkNotif(cfg Config, results []Result, totalURLs int) {
	runURL := fmt.Sprintf("https://github.com/%s/actions/runs/%s", cfg.Repo, cfg.RunID)
	now := time.Now().UTC().Format("2006-01-02 15:04 UTC")

	fields := make([]map[string]any, 0, len(results))
	for _, r := range results {
		val := fmt.Sprintf("✅ %s URLs", fmtNum(r.Count))
		if !r.Ok {
			val = "❌ Failed"
		}
		fields = append(fields, map[string]any{
			"name": r.Domain, "value": val, "inline": true,
		})
	}

	embed := map[string]any{
		"title":  "🔍 GAU Chunk Done",
		"color":  0x00ff99,
		"fields": fields,
		"footer": map[string]string{
			"text": fmt.Sprintf("Chunk: %s URLs • %s | %s", fmtNum(totalURLs), now, runURL),
		},
	}
	sendEmbed(cfg.Webhook, embed)
}

func sendEmbed(webhookURL string, embed map[string]any) {
	payload, _ := json.Marshal(map[string]any{"embeds": []any{embed}})
	resp, err := http.Post(webhookURL, "application/json", bytes.NewReader(payload)) //nolint:noctx
	if err != nil {
		fmt.Printf("⚠️  Discord: %v\n", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		fmt.Printf("⚠️  Discord HTTP %d\n", resp.StatusCode)
	}
}

// ─── File helpers ──────────────────────────────────────────────────────────

func readLines(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

func writeLines(path string, lines []string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, l := range lines {
		_, _ = fmt.Fprintln(w, l)
	}
	return w.Flush()
}

func dedup(buf *bytes.Buffer) []string {
	seen := make(map[string]struct{})
	sc := bufio.NewScanner(buf)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			seen[line] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for u := range seen {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// ─── Misc helpers ──────────────────────────────────────────────────────────

func safeName(s string) string {
	return strings.NewReplacer(".", "_", "/", "_", ":", "_").Replace(s)
}

func fmtNum(n int) string {
	s := fmt.Sprintf("%d", n)
	if n < 1000 {
		return s
	}
	b := []byte(s)
	out := make([]byte, 0, len(b)+len(b)/3)
	for i, c := range b {
		if i > 0 && (len(b)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
	os.Exit(1)
}
