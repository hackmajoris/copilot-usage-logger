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
	addr        = flag.String("addr", ":8080", "proxy listen address")
	taskName    = flag.String("task", "default", "task name to group usage under")
	logFile     = flag.String("log", "copilot_usage.log", "append-only request/response log")
	summaryFile = flag.String("summary", "copilot_summary.log", "overwritten summary file")
	caCertFile  = flag.String("cacert", "ca.crt", "CA certificate file (created if missing)")
	caKeyFile   = flag.String("cakey", "ca.key", "CA private-key file (created if missing)")
)

const targetHost = "api.githubcopilot.com"

// ── Stats ────────────────────────────────────────────────

type Stats struct {
	mu              sync.Mutex
	TotalCalls      int
	TotalTokens     int
	CachedTokens    int
	ReasoningTokens int
	PremiumRequests int
	Models          map[string]int
}

func newStats() *Stats {
	return &Stats{Models: make(map[string]int)}
}

var (
	globalStats = newStats()
	taskStats   = map[string]*Stats{}
	taskMu      sync.Mutex
)

func getOrCreateTask(name string) *Stats {
	taskMu.Lock()
	defer taskMu.Unlock()
	if s, ok := taskStats[name]; ok {
		return s
	}
	s := newStats()
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
	var sb strings.Builder

	globalStats.mu.Lock()
	sb.WriteString("\n" + strings.Repeat("=", 60) + "\n")
	sb.WriteString(fmt.Sprintf("COPILOT USAGE SUMMARY  (updated %s)\n", timestamp()))
	sb.WriteString(strings.Repeat("=", 60) + "\n")
	sb.WriteString(fmt.Sprintf("  Total API calls     : %d\n", globalStats.TotalCalls))
	sb.WriteString(fmt.Sprintf("  Total tokens        : %d\n", globalStats.TotalTokens))
	sb.WriteString(fmt.Sprintf("  Cached tokens       : %d\n", globalStats.CachedTokens))
	sb.WriteString(fmt.Sprintf("  Reasoning tokens    : %d\n", globalStats.ReasoningTokens))
	sb.WriteString(fmt.Sprintf("  Premium requests    : %d (3x weight; %d raw requests)\n",
		globalStats.PremiumRequests, globalStats.PremiumRequests/3))
	sb.WriteString("  Models used:\n")

	models := globalStats.Models
	globalStats.mu.Unlock()

	for model, count := range models {
		sb.WriteString(fmt.Sprintf("    - %s: %d calls\n", model, count))
	}

	taskMu.Lock()
	names := make([]string, 0, len(taskStats))
	for n := range taskStats {
		names = append(names, n)
	}
	sort.Strings(names)

	if len(names) > 0 {
		sb.WriteString("\n  Per-task breakdown:\n")
		sb.WriteString("  " + strings.Repeat("-", 56) + "\n")
		for _, name := range names {
			ts := taskStats[name]
			ts.mu.Lock()
			sb.WriteString(fmt.Sprintf("  Task: %s\n", name))
			sb.WriteString(fmt.Sprintf("    Calls           : %d\n", ts.TotalCalls))
			sb.WriteString(fmt.Sprintf("    Total tokens    : %d\n", ts.TotalTokens))
			sb.WriteString(fmt.Sprintf("    Cached tokens   : %d\n", ts.CachedTokens))
			sb.WriteString(fmt.Sprintf("    Reasoning tokens: %d\n", ts.ReasoningTokens))
			sb.WriteString(fmt.Sprintf("    Premium requests: %d (3x weight; %d raw requests)\n",
				ts.PremiumRequests, ts.PremiumRequests/3))
			sb.WriteString("    Models:\n")
			for model, count := range ts.Models {
				sb.WriteString(fmt.Sprintf("      - %s: %d calls\n", model, count))
			}
			sb.WriteString("\n")
			ts.mu.Unlock()
		}
	}
	taskMu.Unlock()

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
		premiumWeight := 0
		if reasoning > 0 {
			premiumWeight = 3
		}

		// global
		globalStats.mu.Lock()
		globalStats.TotalTokens += total
		globalStats.CachedTokens += cached
		globalStats.ReasoningTokens += reasoning
		globalStats.PremiumRequests += premiumWeight
		globalStats.Models[model]++
		globalStats.mu.Unlock()

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
				"  Cached tokens   : %d\n  Reasoning tokens: %d\n  Premium weight  : %dx\n",
			timestamp(), task, model, total, cached, reasoning, premiumWeight,
		)
		appendLog(*logFile, callLog)
		log.Printf("Copilot call — task=%s model=%s tokens=%d cached=%d reasoning=%d premium=%dx",
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

func main() {
	flag.Parse()

	caTLS, caCert, err := loadOrCreateCA()
	if err != nil {
		log.Fatalf("CA init failed: %v", err)
	}

	p := newProxy(caTLS, caCert)
	log.Printf("copilot-logger proxy listening on %s  (task=%s)", *addr, *taskName)
	log.Printf("Install %s as a trusted root CA, then point your proxy settings to http://localhost%s", *caCertFile, *addr)

	srv := &http.Server{
		Addr:    *addr,
		Handler: p,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
