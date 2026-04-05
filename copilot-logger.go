// copilot-logger — standalone HTTPS MITM proxy that intercepts
// api.githubcopilot.com traffic and logs token usage.
//
// Usage:
//
//	go run copilot-logger.go [-addr :8080] [-task my-feature] [-log copilot_usage.log] [-summary copilot_summary.log]
//
// Then configure your HTTP_PROXY / HTTPS_PROXY (or VS Code / GitHub Copilot
// extension proxy settings) to point at http://localhost:8080.
//
// On first run the proxy generates a self-signed CA certificate
// (ca.crt / ca.key).  Install ca.crt as a trusted root CA in your OS / browser
// so that TLS connections to api.githubcopilot.com succeed.
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ── CONFIG flags ─────────────────────────────────────────

var (
	addr         = flag.String("addr", ":8080", "TCP address the MITM proxy listens on (e.g. :8080 or 127.0.0.1:9090)")
	taskName     = flag.String("task", "default", "label used to group token-usage stats in the summary log (e.g. feature-branch or sprint-42)")
	logFile      = flag.String("log", "copilot_usage.log", "path to the append-only NDJSON file that records every intercepted request and response")
	summaryFile  = flag.String("summary-file", "copilot_summary.log", "path to the summary file that is rewritten on each request with aggregated per-model token counts")
	dataFile     = flag.String("data", "copilot_data.json", "path to the persistent JSON store that accumulates stats across all runs")
	caCertFile   = flag.String("cacert", "ca.crt", "path to the self-signed CA certificate used to intercept TLS traffic (created automatically on first run)")
	caKeyFile    = flag.String("cakey", "ca.key", "path to the CA private key that signs per-host certificates (created automatically on first run, keep secret)")
	doSummary    = flag.Bool("summary", false, "print current-month usage summary from persistent data store and exit")
	doPrevMonth  = flag.Bool("prevmonth", false, "print previous-month usage summary from persistent data store and exit")
)

const targetHost = "api.githubcopilot.com"

// ── Model multipliers (official GitHub Copilot paid-plan billing weights) ────
// Source: https://docs.github.com/en/copilot/managing-copilot/monitoring-usage-and-entitlements/about-premium-requests
// Models with multiplier 0 are included in the base plan (no premium charge).
// Models marked "Not applicable" for free plan are not available on free tier.

var modelMultipliers = map[string]float64{
	// Claude
	"claude-haiku-4-5":     0.33,
	"claude-opus-4-5":      3,
	"claude-opus-4-6":      3,
	"claude-opus-4-6-fast": 30, // preview, fast mode
	"claude-sonnet-4":      1,
	"claude-sonnet-4-5":    1,
	"claude-sonnet-4-6":    1,
	// Gemini
	"gemini-2.5-pro": 1,
	"gemini-3-flash": 0.33,
	"gemini-3-pro":   1,
	"gemini-3.1-pro": 1,
	// GPT
	"gpt-4.1":            0,
	"gpt-4o":             0,
	"gpt-5-mini":         0,
	"gpt-5.1":            1,
	"gpt-5.1-codex":      1,
	"gpt-5.1-codex-mini": 0.33,
	"gpt-5.1-codex-max":  1,
	"gpt-5.2":            1,
	"gpt-5.2-codex":      1,
	"gpt-5.3-codex":      1,
	"gpt-5.4":            1,
	"gpt-5.4-mini":       0.33,
	// Grok / xAI
	"grok-code-fast-1": 0.25,
	// Other
	"raptor-mini": 0,
	"goldeneye":   0, // free-plan only
}

// premiumMultiplier returns the billing multiplier for a given model name.
// Model identifiers from the API may contain version suffixes or differ in
// casing; we normalise to lowercase and do a prefix/substring match so that
// variants like "gpt-4o-2024-05-13" still resolve correctly.
func premiumMultiplier(model string) float64 {
	lower := strings.ToLower(model)

	// Exact match first.
	if v, ok := modelMultipliers[lower]; ok {
		return v
	}

	// Prefix match: pick the longest matching key so that e.g.
	// "claude-opus-4-6-fast-…" beats "claude-opus-4-6".
	best := ""
	bestVal := -1.0
	for key, val := range modelMultipliers {
		if strings.HasPrefix(lower, key) && len(key) > len(best) {
			best = key
			bestVal = val
		}
	}
	if bestVal >= 0 {
		return bestVal
	}

	// Unknown model: default to 1 (standard premium request).
	return 1
}

// ── Persistent JSON store ─────────────────────────────────
//
// copilot_data.json is the single source of truth.  It is loaded at startup,
// updated after every intercepted response, and used to regenerate the
// human-readable summary log.  In-memory Stats are kept in sync so that the
// hot path (processResponseBody) never needs to re-read the file.

// TaskRecord holds all accumulated stats for one named task.
type TaskRecord struct {
	TotalCalls      int            `json:"total_calls"`
	TotalTokens     int            `json:"total_tokens"`
	CachedTokens    int            `json:"cached_tokens"`
	ReasoningTokens int            `json:"reasoning_tokens"`
	PremiumRequests float64        `json:"premium_requests"`
	Models          map[string]int `json:"models"`
	FirstSeen       string         `json:"first_seen"`
	LastSeen        string         `json:"last_seen"`
}

// Store is the top-level JSON document.
type Store struct {
	Global  *TaskRecord            `json:"global"`
	Tasks   map[string]*TaskRecord `json:"tasks"`
	Monthly map[string]*TaskRecord `json:"monthly"`
}

func newTaskRecord() *TaskRecord {
	return &TaskRecord{Models: make(map[string]int), FirstSeen: timestamp()}
}

func newStore() *Store {
	return &Store{
		Global:  newTaskRecord(),
		Tasks:   make(map[string]*TaskRecord),
		Monthly: make(map[string]*TaskRecord),
	}
}

func currentMonthKey() string {
	return time.Now().Format("2006-01")
}

var (
	store   *Store
	storeMu sync.Mutex
)

func loadStore() error {
	storeMu.Lock()
	defer storeMu.Unlock()

	data, err := os.ReadFile(*dataFile)
	if os.IsNotExist(err) {
		store = newStore()
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", *dataFile, err)
	}
	s := newStore()
	if err := json.Unmarshal(data, s); err != nil {
		return fmt.Errorf("parsing %s: %w", *dataFile, err)
	}
	// Ensure nested maps are initialised (defensive, in case file was hand-edited).
	if s.Global == nil {
		s.Global = newTaskRecord()
	}
	if s.Global.Models == nil {
		s.Global.Models = make(map[string]int)
	}
	if s.Tasks == nil {
		s.Tasks = make(map[string]*TaskRecord)
	}
	if s.Monthly == nil {
		s.Monthly = make(map[string]*TaskRecord)
	}
	for _, tr := range s.Tasks {
		if tr.Models == nil {
			tr.Models = make(map[string]int)
		}
	}
	for _, tr := range s.Monthly {
		if tr.Models == nil {
			tr.Models = make(map[string]int)
		}
	}
	store = s
	return nil
}

func saveStore() {
	storeMu.Lock()
	defer storeMu.Unlock()

	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		log.Printf("saveStore marshal error: %v", err)
		return
	}
	if err := os.WriteFile(*dataFile, data, 0644); err != nil {
		log.Printf("saveStore write error: %v", err)
	}
}

// getOrCreateTaskRecord returns the TaskRecord for name, creating it if needed.
// Must be called with storeMu held.
func getOrCreateTaskRecord(name string) *TaskRecord {
	if tr, ok := store.Tasks[name]; ok {
		return tr
	}
	tr := newTaskRecord()
	store.Tasks[name] = tr
	return tr
}

// promptExistingTask asks the user what to do when the chosen task name already
// has data in the store.  Returns true if startup should continue.
func promptExistingTask(name string, tr *TaskRecord) bool {
	fmt.Fprintf(os.Stderr,
		"\nTask %q already exists in %s:\n  calls=%d  tokens=%d  premium=%.2f  last seen=%s\n\n",
		name, *dataFile, tr.TotalCalls, tr.TotalTokens, tr.PremiumRequests, tr.LastSeen,
	)
	fmt.Fprintf(os.Stderr, "[A]ggregate into existing task / [R]eset and start fresh / [C]ancel: ")

	reader := bufio.NewReader(os.Stdin)
	for {
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(strings.ToLower(line))
		switch line {
		case "a", "aggregate":
			fmt.Fprintln(os.Stderr, "→ Aggregating into existing task.")
			return true
		case "r", "reset":
			fmt.Fprintln(os.Stderr, "→ Resetting task data.")
			storeMu.Lock()
			fresh := newTaskRecord()
			store.Tasks[name] = fresh
			storeMu.Unlock()
			return true
		case "c", "cancel":
			fmt.Fprintln(os.Stderr, "→ Cancelled.")
			return false
		default:
			fmt.Fprintf(os.Stderr, "Please enter A, R, or C: ")
		}
	}
}

// ── Stats (in-memory mirror of the store) ────────────────

type Stats struct {
	mu              sync.Mutex
	TotalCalls      int
	TotalTokens     int
	CachedTokens    int
	ReasoningTokens int
	PremiumRequests float64
	Models          map[string]int
}

func newStats() *Stats {
	return &Stats{Models: make(map[string]int)}
}

// statsFromRecord initialises an in-memory Stats from a persisted TaskRecord.
func statsFromRecord(tr *TaskRecord) *Stats {
	s := &Stats{
		TotalCalls:      tr.TotalCalls,
		TotalTokens:     tr.TotalTokens,
		CachedTokens:    tr.CachedTokens,
		ReasoningTokens: tr.ReasoningTokens,
		PremiumRequests: tr.PremiumRequests,
		Models:          make(map[string]int, len(tr.Models)),
	}
	for k, v := range tr.Models {
		s.Models[k] = v
	}
	return s
}

var (
	globalStats  *Stats
	taskStats    = map[string]*Stats{}
	taskMu       sync.Mutex
	monthlyStats *Stats
)

func getOrCreateTask(name string) *Stats {
	taskMu.Lock()
	defer taskMu.Unlock()
	if s, ok := taskStats[name]; ok {
		return s
	}
	// Bootstrap from the persistent store if a record already exists.
	storeMu.Lock()
	tr := getOrCreateTaskRecord(name)
	s := statsFromRecord(tr)
	storeMu.Unlock()
	taskStats[name] = s
	return s
}

// ── Logging helpers ──────────────────────────────────────

func timestamp() string {
	return time.Now().Format("2006-01-02 15:04:05")
}

func appendLog(path, text string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("appendLog error: %v", err)
		return
	}
	defer f.Close()
	fmt.Fprintln(f, text)
}

func saveSummary() {
	// Flush in-memory stats back to the store so the JSON file is always current.
	flushStatsToStore()
	saveStore()

	// Regenerate the human-readable summary from the store (single source of truth).
	storeMu.Lock()
	g := store.Global
	mk := currentMonthKey()
	mr := store.Monthly[mk]
	var sb strings.Builder
	sb.WriteString("\n" + strings.Repeat("=", 60) + "\n")
	sb.WriteString(fmt.Sprintf("COPILOT USAGE SUMMARY  (updated %s)\n", timestamp()))
	sb.WriteString(strings.Repeat("=", 60) + "\n")
	if mr != nil {
		sb.WriteString(fmt.Sprintf("  MTD (%s):\n", mk))
		sb.WriteString(fmt.Sprintf("  Total API calls     : %d\n", mr.TotalCalls))
		sb.WriteString(fmt.Sprintf("  Total tokens        : %d\n", mr.TotalTokens))
		sb.WriteString(fmt.Sprintf("  Cached tokens       : %d\n", mr.CachedTokens))
		sb.WriteString(fmt.Sprintf("  Reasoning tokens    : %d\n", mr.ReasoningTokens))
		sb.WriteString(fmt.Sprintf("  Premium requests    : %.2f (weighted total across all models)\n", mr.PremiumRequests))
		sb.WriteString("  Models used:\n")
		for model, count := range mr.Models {
			sb.WriteString(fmt.Sprintf("    - %s: %d calls\n", model, count))
		}
		sb.WriteString("\n  All-time:\n")
	}
	sb.WriteString(fmt.Sprintf("  Total API calls     : %d\n", g.TotalCalls))
	sb.WriteString(fmt.Sprintf("  Total tokens        : %d\n", g.TotalTokens))
	sb.WriteString(fmt.Sprintf("  Cached tokens       : %d\n", g.CachedTokens))
	sb.WriteString(fmt.Sprintf("  Reasoning tokens    : %d\n", g.ReasoningTokens))
	sb.WriteString(fmt.Sprintf("  Premium requests    : %.2f (weighted total across all models)\n", g.PremiumRequests))
	sb.WriteString("  Models used:\n")
	for model, count := range g.Models {
		sb.WriteString(fmt.Sprintf("    - %s: %d calls\n", model, count))
	}

	names := make([]string, 0, len(store.Tasks))
	for n := range store.Tasks {
		names = append(names, n)
	}
	sort.Strings(names)

	if len(names) > 0 {
		sb.WriteString("\n  Per-task breakdown:\n")
		sb.WriteString("  " + strings.Repeat("-", 56) + "\n")
		for _, name := range names {
			tr := store.Tasks[name]
			sb.WriteString(fmt.Sprintf("  Task: %s\n", name))
			sb.WriteString(fmt.Sprintf("    First seen      : %s\n", tr.FirstSeen))
			sb.WriteString(fmt.Sprintf("    Last seen       : %s\n", tr.LastSeen))
			sb.WriteString(fmt.Sprintf("    Calls           : %d\n", tr.TotalCalls))
			sb.WriteString(fmt.Sprintf("    Total tokens    : %d\n", tr.TotalTokens))
			sb.WriteString(fmt.Sprintf("    Cached tokens   : %d\n", tr.CachedTokens))
			sb.WriteString(fmt.Sprintf("    Reasoning tokens: %d\n", tr.ReasoningTokens))
			sb.WriteString(fmt.Sprintf("    Premium requests: %.2f (weighted total)\n", tr.PremiumRequests))
			sb.WriteString("    Models:\n")
			for model, count := range tr.Models {
				sb.WriteString(fmt.Sprintf("      - %s: %d calls\n", model, count))
			}
			sb.WriteString("\n")
		}
	}
	storeMu.Unlock()

	sb.WriteString(strings.Repeat("=", 60) + "\n")
	summary := sb.String()

	f, err := os.OpenFile(*summaryFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		log.Printf("saveSummary error: %v", err)
		return
	}
	defer f.Close()
	fmt.Fprint(f, summary)

	log.Print(summary)
}

// flushStatsToStore copies in-memory Stats into the store's TaskRecords and
// global record so they can be serialised to JSON.
func flushStatsToStore() {
	globalStats.mu.Lock()
	g := store.Global
	g.TotalCalls = globalStats.TotalCalls
	g.TotalTokens = globalStats.TotalTokens
	g.CachedTokens = globalStats.CachedTokens
	g.ReasoningTokens = globalStats.ReasoningTokens
	g.PremiumRequests = globalStats.PremiumRequests
	g.LastSeen = timestamp()
	for k, v := range globalStats.Models {
		g.Models[k] = v
	}
	globalStats.mu.Unlock()

	monthlyStats.mu.Lock()
	mk := currentMonthKey()
	if store.Monthly == nil {
		store.Monthly = make(map[string]*TaskRecord)
	}
	// Keep only the current month and the previous month.
	prevMonth := time.Now().AddDate(0, -1, 0).Format("2006-01")
	for k := range store.Monthly {
		if k != mk && k != prevMonth {
			delete(store.Monthly, k)
		}
	}
	mr, ok := store.Monthly[mk]
	if !ok {
		mr = newTaskRecord()
		store.Monthly[mk] = mr
	}
	mr.TotalCalls = monthlyStats.TotalCalls
	mr.TotalTokens = monthlyStats.TotalTokens
	mr.CachedTokens = monthlyStats.CachedTokens
	mr.ReasoningTokens = monthlyStats.ReasoningTokens
	mr.PremiumRequests = monthlyStats.PremiumRequests
	mr.LastSeen = timestamp()
	for k, v := range monthlyStats.Models {
		mr.Models[k] = v
	}
	monthlyStats.mu.Unlock()

	taskMu.Lock()
	defer taskMu.Unlock()
	storeMu.Lock()
	defer storeMu.Unlock()
	for name, s := range taskStats {
		s.mu.Lock()
		tr := getOrCreateTaskRecord(name)
		tr.TotalCalls = s.TotalCalls
		tr.TotalTokens = s.TotalTokens
		tr.CachedTokens = s.CachedTokens
		tr.ReasoningTokens = s.ReasoningTokens
		tr.PremiumRequests = s.PremiumRequests
		tr.LastSeen = timestamp()
		for k, v := range s.Models {
			tr.Models[k] = v
		}
		s.mu.Unlock()
	}
}

// ── SSE / usage parsing ──────────────────────────────────

type usageChunk struct {
	Model string `json:"model"`
	Usage *struct {
		TotalTokens         int `json:"total_tokens"`
		ReasoningTokens     int `json:"reasoning_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

func processResponseBody(task string, body []byte) {
	ts := getOrCreateTask(task)

	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		jsonStr := strings.TrimSpace(line[len("data:"):])
		if jsonStr == "[DONE]" {
			continue
		}

		var chunk usageChunk
		if err := json.Unmarshal([]byte(jsonStr), &chunk); err != nil {
			continue
		}
		if chunk.Usage == nil {
			continue
		}

		total := chunk.Usage.TotalTokens
		reasoning := chunk.Usage.ReasoningTokens
		cached := 0
		if chunk.Usage.PromptTokensDetails != nil {
			cached = chunk.Usage.PromptTokensDetails.CachedTokens
		}
		model := chunk.Model
		if model == "" {
			model = "unknown"
		}
		premiumWeight := premiumMultiplier(model)

		// global
		globalStats.mu.Lock()
		globalStats.TotalTokens += total
		globalStats.CachedTokens += cached
		globalStats.ReasoningTokens += reasoning
		globalStats.PremiumRequests += premiumWeight
		globalStats.Models[model]++
		globalStats.mu.Unlock()

		// monthly
		monthlyStats.mu.Lock()
		monthlyStats.TotalTokens += total
		monthlyStats.CachedTokens += cached
		monthlyStats.ReasoningTokens += reasoning
		monthlyStats.PremiumRequests += premiumWeight
		monthlyStats.Models[model]++
		monthlyStats.mu.Unlock()

		// per-task
		ts.mu.Lock()
		ts.TotalTokens += total
		ts.CachedTokens += cached
		ts.ReasoningTokens += reasoning
		ts.PremiumRequests += premiumWeight
		ts.Models[model]++
		ts.mu.Unlock()

		callLog := fmt.Sprintf(
			"[%s] [%s] ◄ RESPONSE\n  Model           : %s\n  Total tokens    : %d\n"+
				"  Cached tokens   : %d\n  Reasoning tokens: %d\n  Premium weight  : %.2gx\n",
			timestamp(), task, model, total, cached, reasoning, premiumWeight,
		)
		appendLog(*logFile, callLog)
		log.Printf("Copilot call — task=%s model=%s tokens=%d cached=%d reasoning=%d premium=%.2gx",
			task, model, total, cached, reasoning, premiumWeight)
	}

	saveSummary()
}

// ── CA / TLS helpers ─────────────────────────────────────

func loadOrCreateCA() (tls.Certificate, *x509.Certificate, error) {
	if _, err := os.Stat(*caCertFile); err == nil {
		cert, err := tls.LoadX509KeyPair(*caCertFile, *caKeyFile)
		if err != nil {
			return tls.Certificate{}, nil, err
		}
		x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return tls.Certificate{}, nil, err
		}
		return cert, x509Cert, nil
	}

	log.Printf("Generating new CA certificate → %s / %s", *caCertFile, *caKeyFile)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "copilot-logger CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	cf, _ := os.Create(*caCertFile)
	pem.Encode(cf, &pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	cf.Close()

	kf, _ := os.Create(*caKeyFile)
	pem.Encode(kf, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	kf.Close()

	x509Cert, _ := x509.ParseCertificate(certDER)
	tlsCert := tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: key}
	return tlsCert, x509Cert, nil
}

func signCert(caCert *x509.Certificate, caKey *rsa.PrivateKey, host string) (*tls.Config, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		DNSNames:     []string{host},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	tlsCert := tls.Certificate{Certificate: [][]byte{certDER}, PrivateKey: key}
	return &tls.Config{Certificates: []tls.Certificate{tlsCert}}, nil
}

// ── Proxy handler ────────────────────────────────────────

type proxy struct {
	caCert    *x509.Certificate
	caKey     *rsa.PrivateKey
	transport *http.Transport
}

func newProxy(caTLS tls.Certificate, caCert *x509.Certificate) *proxy {
	caKey := caTLS.PrivateKey.(*rsa.PrivateKey)
	return &proxy{
		caCert: caCert,
		caKey:  caKey,
		transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: false},
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
	}
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handlePlain(w, r)
}

// handleConnect — intercept CONNECT tunnel and MITM the TLS connection.
func (p *proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.Host)

	tlsCfg, err := signCert(p.caCert, p.caKey, host)
	if err != nil {
		http.Error(w, "failed to sign certificate", http.StatusInternalServerError)
		return
	}

	// Tell the client the tunnel is established.
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")

	// Wrap in TLS.
	tlsConn := tls.Server(conn, tlsCfg)
	if err := tlsConn.Handshake(); err != nil {
		tlsConn.Close()
		return
	}
	defer tlsConn.Close()

	// Serve the decrypted connection with a fresh HTTP server.
	innerProxy := &innerHandler{target: r.Host, parent: p}
	srv := &http.Server{Handler: innerProxy}
	srv.Serve(&singleConnListener{conn: tlsConn})
}

// handlePlain — plain HTTP (non-CONNECT) requests.
func (p *proxy) handlePlain(w http.ResponseWriter, r *http.Request) {
	p.doRequest(w, r, r.URL.Scheme, r.Host)
}

func (p *proxy) doRequest(w http.ResponseWriter, r *http.Request, scheme, host string) {
	task := *taskName
	isTarget := strings.Contains(host, targetHost)

	if isTarget && r.Method == http.MethodPost {
		globalStats.mu.Lock()
		globalStats.TotalCalls++
		globalStats.mu.Unlock()

		monthlyStats.mu.Lock()
		monthlyStats.TotalCalls++
		monthlyStats.mu.Unlock()

		ts := getOrCreateTask(task)
		ts.mu.Lock()
		ts.TotalCalls++
		ts.mu.Unlock()

		appendLog(*logFile, fmt.Sprintf("\n[%s] [%s] ► POST %s://%s%s",
			timestamp(), task, scheme, host, r.URL.RequestURI()))
	}

	// Build upstream URL.
	outURL := *r.URL
	if outURL.Host == "" {
		outURL.Host = host
	}
	if outURL.Scheme == "" {
		outURL.Scheme = scheme
	}

	outReq, err := http.NewRequest(r.Method, outURL.String(), r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	outReq.Header = r.Header.Clone()

	resp, err := p.transport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	if isTarget && r.Method == http.MethodPost {
		go processResponseBody(task, body)
	}

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

// innerHandler is used inside the MITM TLS server for CONNECT tunnels.
type innerHandler struct {
	target string
	parent *proxy
}

func (h *innerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.parent.doRequest(w, r, "https", h.target)
}

// singleConnListener wraps a single net.Conn as a net.Listener.
type singleConnListener struct {
	conn net.Conn
	once sync.Once
	ch   chan net.Conn
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	if l.ch == nil {
		l.ch = make(chan net.Conn, 1)
		l.ch <- l.conn
	}
	c, ok := <-l.ch
	if !ok {
		return nil, fmt.Errorf("listener closed")
	}
	return c, nil
}
func (l *singleConnListener) Close() error {
	if l.ch != nil {
		close(l.ch)
	}
	return nil
}
func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

// ── main ─────────────────────────────────────────────────

// printMonthRecord prints the stats for a single TaskRecord under a given heading.
func printMonthRecord(label string, tr *TaskRecord) {
	sep := strings.Repeat("─", 60)
	fmt.Printf("  %s\n", label)
	fmt.Println(sep)
	fmt.Printf("  %-26s %d\n", "Total API calls:", tr.TotalCalls)
	fmt.Printf("  %-26s %d\n", "Total tokens:", tr.TotalTokens)
	fmt.Printf("  %-26s %d\n", "Cached tokens:", tr.CachedTokens)
	fmt.Printf("  %-26s %d\n", "Reasoning tokens:", tr.ReasoningTokens)
	fmt.Printf("  %-26s %.2f\n", "Premium requests (weighted):", tr.PremiumRequests)
	if len(tr.Models) > 0 {
		fmt.Println()
		fmt.Println("  Models:")
		models := make([]string, 0, len(tr.Models))
		for m := range tr.Models {
			models = append(models, m)
		}
		sort.Strings(models)
		for _, m := range models {
			mult := premiumMultiplier(m)
			fmt.Printf("    %-36s %4d calls  (%.2gx)\n", m+":", tr.Models[m], mult)
		}
	}
}

// printSummary loads the data file and prints a human-readable report to stdout.
func printSummary() {
	if err := loadStore(); err != nil {
		fmt.Fprintf(os.Stderr, "Error loading data store: %v\n", err)
		os.Exit(1)
	}

	storeMu.Lock()
	defer storeMu.Unlock()

	g := store.Global
	mk := currentMonthKey()
	mr := store.Monthly[mk]
	sep := strings.Repeat("─", 60)
	thick := strings.Repeat("═", 60)

	fmt.Println()
	fmt.Println(thick)
	fmt.Printf("  COPILOT USAGE SUMMARY  —  current month: %s\n", mk)
	fmt.Println(thick)

	if mr != nil {
		printMonthRecord("Current month ("+mk+")", mr)
		fmt.Println()
		fmt.Printf("  All-time\n")
		fmt.Println(sep)
	}

	fmt.Printf("  %-26s %d\n", "Total API calls:", g.TotalCalls)
	fmt.Printf("  %-26s %d\n", "Total tokens:", g.TotalTokens)
	fmt.Printf("  %-26s %d\n", "Cached tokens:", g.CachedTokens)
	fmt.Printf("  %-26s %d\n", "Reasoning tokens:", g.ReasoningTokens)
	fmt.Printf("  %-26s %.2f\n", "Premium requests (weighted):", g.PremiumRequests)
	if len(g.Models) > 0 {
		fmt.Println()
		fmt.Println("  Models (all-time):")
		models := make([]string, 0, len(g.Models))
		for m := range g.Models {
			models = append(models, m)
		}
		sort.Strings(models)
		for _, m := range models {
			mult := premiumMultiplier(m)
			fmt.Printf("    %-36s %4d calls  (%.2gx)\n", m+":", g.Models[m], mult)
		}
	}

	names := make([]string, 0, len(store.Tasks))
	for n := range store.Tasks {
		names = append(names, n)
	}
	sort.Strings(names)

	if len(names) > 0 {
		fmt.Println()
		fmt.Println(sep)
		fmt.Println("  TASKS")
		fmt.Println(sep)
		for _, name := range names {
			tr := store.Tasks[name]
			fmt.Printf("  %-20s  calls=%-6d tokens=%-10d cached=%-8d reasoning=%-8d premium=%.2f\n",
				name, tr.TotalCalls, tr.TotalTokens, tr.CachedTokens, tr.ReasoningTokens, tr.PremiumRequests)
		}
	}

	fmt.Println(thick)
	fmt.Println()
}

// printPrevMonth loads the data file and prints stats for the previous calendar month.
func printPrevMonth() {
	if err := loadStore(); err != nil {
		fmt.Fprintf(os.Stderr, "Error loading data store: %v\n", err)
		os.Exit(1)
	}

	storeMu.Lock()
	defer storeMu.Unlock()

	pk := time.Now().AddDate(0, -1, 0).Format("2006-01")
	thick := strings.Repeat("═", 60)

	fmt.Println()
	fmt.Println(thick)
	fmt.Printf("  COPILOT USAGE  —  previous month: %s\n", pk)
	fmt.Println(thick)

	pr, ok := store.Monthly[pk]
	if !ok || pr.TotalCalls == 0 {
		fmt.Printf("  No data recorded for %s.\n", pk)
	} else {
		printMonthRecord("Previous month ("+pk+")", pr)
	}

	fmt.Println(thick)
	fmt.Println()
}

func main() {
	flag.Usage = func() {
		out := flag.CommandLine.Output()
		fmt.Fprintf(out, "usage: copilot-logger [-addr ADDR] [-task TASK] [-log FILE] [-data FILE]\n")
		fmt.Fprintf(out, "                      [-cacert FILE] [-cakey FILE] [-h] [--summary] [--prevmonth]\n")
		fmt.Fprintf(out, "\n")
		fmt.Fprintf(out, "HTTPS MITM proxy that intercepts api.githubcopilot.com traffic and logs token usage.\n")
		fmt.Fprintf(out, "\n")
		fmt.Fprintf(out, "options:\n")
		fmt.Fprintf(out, "  -addr ADDR      TCP address the proxy listens on (default: :8080)\n")
		fmt.Fprintf(out, "  -task TASK      label used to group token-usage stats (default: \"default\")\n")
		fmt.Fprintf(out, "  -log FILE       path to the append-only NDJSON log file (default: copilot_usage.log)\n")
		fmt.Fprintf(out, "  -summary-file FILE  path to the summary file rewritten on each request (default: copilot_summary.log)\n")
		fmt.Fprintf(out, "  -data FILE      path to the persistent JSON store accumulating stats across runs (default: copilot_data.json)\n")
		fmt.Fprintf(out, "  -cacert FILE    path to the self-signed CA certificate (default: ca.crt)\n")
		fmt.Fprintf(out, "  -cakey FILE     path to the CA private key (default: ca.key)\n")
		fmt.Fprintf(out, "  -h, --help      show this help message and exit\n")
		fmt.Fprintf(out, "\n")
		fmt.Fprintf(out, "commands:\n")
		fmt.Fprintf(out, "  --summary       print current-month usage summary from persistent data store and exit\n")
		fmt.Fprintf(out, "  --prevmonth     print previous-month usage summary from persistent data store and exit\n")
		fmt.Fprintf(out, "\n")
		fmt.Fprintf(out, "workflow:\n")
		fmt.Fprintf(out, "  1. Run the proxy (creates ca.crt/ca.key on first run).\n")
		fmt.Fprintf(out, "  2. Install ca.crt as a trusted root CA in your OS/browser/editor.\n")
		fmt.Fprintf(out, "  3. Point HTTP_PROXY / HTTPS_PROXY to http://localhost:8080.\n")
		fmt.Fprintf(out, "  4. Use GitHub Copilot normally — every request is logged and summarised.\n")
	}
	flag.Parse()

	// Command flags: --summary / --prevmonth — read the data file and exit.
	// Also support legacy positional subcommands for backwards compatibility.
	if *doSummary || flag.Arg(0) == "summary" {
		printSummary()
		return
	}
	if *doPrevMonth || flag.Arg(0) == "prevmonth" {
		printPrevMonth()
		return
	}

	// Load (or initialise) the persistent JSON store.
	if err := loadStore(); err != nil {
		log.Fatalf("Failed to load data store: %v", err)
	}

	// If the chosen task already has data, ask the user what to do.
	storeMu.Lock()
	existingTask, taskExists := store.Tasks[*taskName]
	storeMu.Unlock()
	if taskExists && existingTask.TotalCalls > 0 {
		if !promptExistingTask(*taskName, existingTask) {
			os.Exit(0)
		}
	}

	// Bootstrap in-memory stats from the (possibly reset) store so accumulated
	// data from previous runs is available immediately.
	storeMu.Lock()
	globalStats = statsFromRecord(store.Global)
	mk := currentMonthKey()
	if mr, ok := store.Monthly[mk]; ok {
		monthlyStats = statsFromRecord(mr)
	} else {
		monthlyStats = newStats()
	}
	// Also pre-populate any tasks already in the store.
	for name, tr := range store.Tasks {
		taskMu.Lock()
		taskStats[name] = statsFromRecord(tr)
		taskMu.Unlock()
	}
	storeMu.Unlock()

	caTLS, caCert, err := loadOrCreateCA()
	if err != nil {
		log.Fatalf("CA init failed: %v", err)
	}

	p := newProxy(caTLS, caCert)
	log.Printf("copilot-logger proxy listening on %s  (task=%s)", *addr, *taskName)
	log.Printf("Install %s as a trusted root CA, then point your proxy settings to http://localhost%s", *caCertFile, *addr)
	log.Printf("Persistent data store: %s", *dataFile)

	srv := &http.Server{
		Addr:    *addr,
		Handler: p,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
