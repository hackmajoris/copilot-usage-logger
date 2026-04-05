package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ── test helpers ──────────────────────────────────────────

// withFreshStore swaps the global store for a new one and restores the original
// after the test completes.
func withFreshStore(t *testing.T) {
	t.Helper()
	orig := store
	store = newStore()
	t.Cleanup(func() { store = orig })
}

// withTempFiles redirects logFile, summaryFile, and dataFile into a temp
// directory so tests never touch the real files.  Returns the temp dir path.
func withTempFiles(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	origLog, origSummary, origData := logFile, summaryFile, dataFile
	lf := filepath.Join(dir, "usage.log")
	sf := filepath.Join(dir, "summary.log")
	df := filepath.Join(dir, "data.json")
	logFile, summaryFile, dataFile = &lf, &sf, &df
	t.Cleanup(func() { logFile, summaryFile, dataFile = origLog, origSummary, origData })
	return dir
}

// sseBody builds a mock SSE response from a list of raw JSON chunk strings.
func sseBody(chunks ...string) []byte {
	var sb strings.Builder
	for _, c := range chunks {
		fmt.Fprintf(&sb, "data: %s\n\n", c)
	}
	sb.WriteString("data: [DONE]\n")
	return []byte(sb.String())
}

// ── premiumMultiplier ─────────────────────────────────────

func TestPremiumMultiplier(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  float64
	}{
		// exact matches
		{"claude-sonnet-4-5",        "claude-sonnet-4-5",        1.0},
		{"claude-haiku-4-5",         "claude-haiku-4-5",         0.33},
		{"claude-opus-4-6",          "claude-opus-4-6",          3.0},
		{"claude-opus-4-6-fast",     "claude-opus-4-6-fast",     30.0},
		{"gpt-4o free",              "gpt-4o",                   0.0},
		{"gpt-4.1 free",             "gpt-4.1",                  0.0},
		{"gpt-5.1",                  "gpt-5.1",                  1.0},
		{"gemini-2.5-pro",           "gemini-2.5-pro",           1.0},
		{"grok-code-fast-1",         "grok-code-fast-1",         0.25},
		{"raptor-mini free",         "raptor-mini",              0.0},
		// prefix match — model ID has an extra version/date suffix
		{"gpt-4o with date suffix",          "gpt-4o-2024-05-13",           0.0},
		{"claude-sonnet-4-5 versioned",      "claude-sonnet-4-5-20251015",  1.0},
		// longest prefix wins: "claude-opus-4-6-fast" must beat "claude-opus-4-6"
		{"fast variant beats base prefix",   "claude-opus-4-6-fast-preview", 30.0},
		// case-insensitive
		{"all uppercase",  "GPT-4O",            0.0},
		{"mixed case",     "Claude-Sonnet-4-5", 1.0},
		// unknown model defaults to 1
		{"unknown model",  "some-future-model",  1.0},
		{"empty string",   "",                   1.0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := premiumMultiplier(tc.model)
			if got != tc.want {
				t.Errorf("premiumMultiplier(%q) = %v, want %v", tc.model, got, tc.want)
			}
		})
	}
}

// ── sortedKeys ────────────────────────────────────────────

func TestSortedKeys(t *testing.T) {
	tests := []struct {
		name  string
		input map[string]int
		want  []string
	}{
		{"empty map",       map[string]int{},                               []string{}},
		{"single entry",    map[string]int{"z": 1},                        []string{"z"}},
		{"already sorted",  map[string]int{"a": 1, "b": 2, "c": 3},       []string{"a", "b", "c"}},
		{"reverse order",   map[string]int{"c": 1, "b": 2, "a": 3},       []string{"a", "b", "c"}},
		{
			name:  "model names",
			input: map[string]int{"gpt-4o": 3, "claude-sonnet-4-5": 1, "gemini-2.5-pro": 2},
			want:  []string{"claude-sonnet-4-5", "gemini-2.5-pro", "gpt-4o"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sortedKeys(tc.input)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d; got %v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// ── copyRecord ────────────────────────────────────────────

func TestCopyRecord(t *testing.T) {
	tests := []struct {
		name string
		orig TaskRecord
	}{
		{
			name: "empty models map",
			orig: TaskRecord{TotalCalls: 5, TotalTokens: 1000, Models: map[string]int{}},
		},
		{
			name: "populated record",
			orig: TaskRecord{
				TotalCalls:      10,
				TotalTokens:     5000,
				CachedTokens:    200,
				ReasoningTokens: 50,
				PremiumRequests: 3.5,
				Models:          map[string]int{"gpt-4o": 5, "claude-sonnet-4-5": 5},
				FirstSeen:       "2026-01-01 00:00:00",
				LastSeen:        "2026-04-01 12:00:00",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cp := copyRecord(&tc.orig)

			// Mutating scalar fields on the copy must not affect the original.
			origCalls := tc.orig.TotalCalls
			cp.TotalCalls++
			if tc.orig.TotalCalls != origCalls {
				t.Error("mutating copy TotalCalls affected original")
			}

			// Mutating the Models map on the copy must not affect the original.
			cp.Models["injected"] = 99
			if _, ok := tc.orig.Models["injected"]; ok {
				t.Error("mutating copy Models affected original (shallow copy)")
			}
		})
	}
}

// ── recordCall ────────────────────────────────────────────

func TestRecordCall(t *testing.T) {
	tests := []struct {
		name        string
		task        string
		times       int
		wantGlobal  int
		wantTask    int
		wantMonthly int
	}{
		{"single call",    "feature-a", 1, 1, 1, 1},
		{"three calls",    "feature-b", 3, 3, 3, 3},
		{"ten calls",      "feature-c", 10, 10, 10, 10},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withFreshStore(t)

			for range tc.times {
				recordCall(tc.task)
			}

			if got := store.Global.TotalCalls; got != tc.wantGlobal {
				t.Errorf("Global.TotalCalls = %d, want %d", got, tc.wantGlobal)
			}

			tr, ok := store.Tasks[tc.task]
			if !ok {
				t.Fatalf("task %q not created in store", tc.task)
			}
			if got := tr.TotalCalls; got != tc.wantTask {
				t.Errorf("Task.TotalCalls = %d, want %d", got, tc.wantTask)
			}

			mk := currentMonthKey()
			mr, ok := store.Monthly[mk]
			if !ok {
				t.Fatalf("monthly record %q not created", mk)
			}
			if got := mr.TotalCalls; got != tc.wantMonthly {
				t.Errorf("Monthly.TotalCalls = %d, want %d", got, tc.wantMonthly)
			}
		})
	}
}

// ── recordUsage ───────────────────────────────────────────

func TestRecordUsage(t *testing.T) {
	tests := []struct {
		name            string
		task            string
		model           string
		total           int
		cached          int
		reasoning       int
		wantPremium     float64
		wantModelCounts map[string]int
	}{
		{
			name:            "free model — no premium charge",
			task:            "task-a",
			model:           "gpt-4o",
			total:           500,
			cached:          50,
			reasoning:       0,
			wantPremium:     0.0,
			wantModelCounts: map[string]int{"gpt-4o": 1},
		},
		{
			name:            "premium model — 3x weight",
			task:            "task-b",
			model:           "claude-opus-4-6",
			total:           1000,
			cached:          100,
			reasoning:       200,
			wantPremium:     3.0,
			wantModelCounts: map[string]int{"claude-opus-4-6": 1},
		},
		{
			name:            "unknown model defaults to weight 1",
			task:            "task-c",
			model:           "future-model-x",
			total:           300,
			cached:          0,
			reasoning:       0,
			wantPremium:     1.0,
			wantModelCounts: map[string]int{"future-model-x": 1},
		},
		{
			name:            "zero tokens still registers model",
			task:            "task-d",
			model:           "gpt-4o",
			total:           0,
			cached:          0,
			reasoning:       0,
			wantPremium:     0.0,
			wantModelCounts: map[string]int{"gpt-4o": 1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withFreshStore(t)

			recordUsage(tc.task, tc.model, tc.total, tc.cached, tc.reasoning)

			// Verify all three records (global, task, monthly) are updated identically.
			mk := currentMonthKey()
			records := map[string]*TaskRecord{
				"global":  store.Global,
				"task":    store.Tasks[tc.task],
				"monthly": store.Monthly[mk],
			}

			for label, rec := range records {
				if rec == nil {
					t.Errorf("%s record not created", label)
					continue
				}
				if rec.TotalTokens != tc.total {
					t.Errorf("%s TotalTokens = %d, want %d", label, rec.TotalTokens, tc.total)
				}
				if rec.CachedTokens != tc.cached {
					t.Errorf("%s CachedTokens = %d, want %d", label, rec.CachedTokens, tc.cached)
				}
				if rec.ReasoningTokens != tc.reasoning {
					t.Errorf("%s ReasoningTokens = %d, want %d", label, rec.ReasoningTokens, tc.reasoning)
				}
				if rec.PremiumRequests != tc.wantPremium {
					t.Errorf("%s PremiumRequests = %v, want %v", label, rec.PremiumRequests, tc.wantPremium)
				}
				for model, count := range tc.wantModelCounts {
					if got := rec.Models[model]; got != count {
						t.Errorf("%s Models[%q] = %d, want %d", label, model, got, count)
					}
				}
			}
		})
	}
}

// ── loadStore ─────────────────────────────────────────────

func TestLoadStore(t *testing.T) {
	tests := []struct {
		name          string
		fileContent   string // empty → file absent
		wantErr       bool
		wantCalls     int
		wantTaskCount int
		wantNilModels bool // expect Models to be non-nil after load
	}{
		{
			name:          "no file creates fresh store",
			wantCalls:     0,
			wantTaskCount: 0,
		},
		{
			name: "valid JSON is loaded correctly",
			fileContent: `{
				"global": {
					"total_calls": 42, "total_tokens": 1000,
					"models": {"gpt-4o": 5},
					"first_seen": "2026-01-01 00:00:00",
					"last_seen":  "2026-04-01 00:00:00"
				},
				"tasks": {
					"sprint-1": {
						"total_calls": 10, "total_tokens": 500,
						"models": {},
						"first_seen": "2026-01-01 00:00:00",
						"last_seen":  "2026-01-31 00:00:00"
					}
				},
				"monthly": {}
			}`,
			wantCalls:     42,
			wantTaskCount: 1,
		},
		{
			name:        "invalid JSON returns error",
			fileContent: `not valid json {{{`,
			wantErr:     true,
		},
		{
			name: "null models map is initialised defensively",
			fileContent: `{
				"global": {"total_calls": 1, "models": null, "first_seen": "", "last_seen": ""},
				"tasks":   {},
				"monthly": {}
			}`,
			wantCalls:     1,
			wantTaskCount: 0,
		},
		{
			name: "missing tasks and monthly maps are initialised",
			fileContent: `{
				"global": {"total_calls": 0, "models": {}, "first_seen": "", "last_seen": ""}
			}`,
			wantCalls:     0,
			wantTaskCount: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			df := filepath.Join(dir, "data.json")
			origData := dataFile
			dataFile = &df
			t.Cleanup(func() { dataFile = origData })

			if tc.fileContent != "" {
				if err := os.WriteFile(df, []byte(tc.fileContent), 0644); err != nil {
					t.Fatalf("setup WriteFile: %v", err)
				}
			}

			origStore := store
			t.Cleanup(func() { store = origStore })

			err := loadStore()
			if (err != nil) != tc.wantErr {
				t.Fatalf("loadStore() error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}

			if store.Global == nil {
				t.Fatal("store.Global is nil after loadStore")
			}
			if store.Global.Models == nil {
				t.Error("store.Global.Models is nil after loadStore")
			}
			if store.Tasks == nil {
				t.Error("store.Tasks is nil after loadStore")
			}
			if store.Monthly == nil {
				t.Error("store.Monthly is nil after loadStore")
			}
			if got := store.Global.TotalCalls; got != tc.wantCalls {
				t.Errorf("Global.TotalCalls = %d, want %d", got, tc.wantCalls)
			}
			if got := len(store.Tasks); got != tc.wantTaskCount {
				t.Errorf("len(Tasks) = %d, want %d", got, tc.wantTaskCount)
			}
		})
	}
}

// ── saveStore / loadStore round-trip ─────────────────────

func TestSaveLoadRoundtrip(t *testing.T) {
	tests := []struct {
		name  string
		setup func(s *Store)
		check func(t *testing.T, s *Store)
	}{
		{
			name: "global token counts survive round-trip",
			setup: func(s *Store) {
				s.Global.TotalCalls = 7
				s.Global.TotalTokens = 3500
				s.Global.CachedTokens = 100
				s.Global.ReasoningTokens = 50
				s.Global.PremiumRequests = 2.5
				s.Global.Models["gpt-4o"] = 3
				s.Global.Models["claude-sonnet-4-5"] = 4
			},
			check: func(t *testing.T, s *Store) {
				t.Helper()
				g := s.Global
				if g.TotalCalls != 7 {
					t.Errorf("TotalCalls = %d, want 7", g.TotalCalls)
				}
				if g.TotalTokens != 3500 {
					t.Errorf("TotalTokens = %d, want 3500", g.TotalTokens)
				}
				if g.PremiumRequests != 2.5 {
					t.Errorf("PremiumRequests = %v, want 2.5", g.PremiumRequests)
				}
				if g.Models["gpt-4o"] != 3 {
					t.Errorf("Models[gpt-4o] = %d, want 3", g.Models["gpt-4o"])
				}
				if g.Models["claude-sonnet-4-5"] != 4 {
					t.Errorf("Models[claude-sonnet-4-5] = %d, want 4", g.Models["claude-sonnet-4-5"])
				}
			},
		},
		{
			name: "task records survive round-trip",
			setup: func(s *Store) {
				tr := newTaskRecord()
				tr.TotalCalls = 5
				tr.TotalTokens = 1000
				tr.Models["gemini-2.5-pro"] = 5
				s.Tasks["my-task"] = tr
			},
			check: func(t *testing.T, s *Store) {
				t.Helper()
				tr, ok := s.Tasks["my-task"]
				if !ok {
					t.Fatal("task 'my-task' not found after reload")
				}
				if tr.TotalCalls != 5 {
					t.Errorf("TotalCalls = %d, want 5", tr.TotalCalls)
				}
				if tr.Models["gemini-2.5-pro"] != 5 {
					t.Errorf("Models[gemini-2.5-pro] = %d, want 5", tr.Models["gemini-2.5-pro"])
				}
			},
		},
		{
			name: "monthly records survive round-trip",
			setup: func(s *Store) {
				mk := currentMonthKey()
				mr := newTaskRecord()
				mr.TotalCalls = 12
				mr.PremiumRequests = 4.5
				s.Monthly[mk] = mr
			},
			check: func(t *testing.T, s *Store) {
				t.Helper()
				mk := currentMonthKey()
				mr, ok := s.Monthly[mk]
				if !ok {
					t.Fatalf("monthly record %q not found after reload", mk)
				}
				if mr.TotalCalls != 12 {
					t.Errorf("TotalCalls = %d, want 12", mr.TotalCalls)
				}
				if mr.PremiumRequests != 4.5 {
					t.Errorf("PremiumRequests = %v, want 4.5", mr.PremiumRequests)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withFreshStore(t)
			withTempFiles(t)

			tc.setup(store)
			saveStore()

			store = newStore() // wipe in-memory state
			if err := loadStore(); err != nil {
				t.Fatalf("loadStore: %v", err)
			}

			tc.check(t, store)
		})
	}
}

// ── processResponseBody ───────────────────────────────────

func TestProcessResponseBody(t *testing.T) {
	type wantStats struct {
		totalTokens     int
		cachedTokens    int
		reasoningTokens int
		premiumRequests float64
		models          map[string]int
	}

	tests := []struct {
		name string
		task string
		body []byte
		want wantStats
	}{
		{
			name: "single usage chunk",
			task: "task-1",
			body: sseBody(`{"model":"gpt-4o","usage":{"total_tokens":100,"reasoning_tokens":0,"prompt_tokens_details":{"cached_tokens":20}}}`),
			want: wantStats{
				totalTokens:     100,
				cachedTokens:    20,
				reasoningTokens: 0,
				premiumRequests: 0.0,
				models:          map[string]int{"gpt-4o": 1},
			},
		},
		{
			name: "premium model — correct weight applied",
			task: "task-2",
			body: sseBody(`{"model":"claude-opus-4-6","usage":{"total_tokens":500,"reasoning_tokens":50,"prompt_tokens_details":{"cached_tokens":10}}}`),
			want: wantStats{
				totalTokens:     500,
				cachedTokens:    10,
				reasoningTokens: 50,
				premiumRequests: 3.0,
				models:          map[string]int{"claude-opus-4-6": 1},
			},
		},
		{
			name: "chunks without usage field are skipped",
			task: "task-3",
			body: sseBody(
				`{"model":"gpt-4o","choices":[{"delta":{"content":"hello"}}]}`,
				`{"model":"gpt-4o","usage":{"total_tokens":200,"reasoning_tokens":0}}`,
			),
			want: wantStats{
				totalTokens:     200,
				premiumRequests: 0.0,
				models:          map[string]int{"gpt-4o": 1},
			},
		},
		{
			name: "invalid JSON lines are skipped",
			task: "task-4",
			body: []byte("data: not-json\n\ndata: {\"model\":\"gpt-4o\",\"usage\":{\"total_tokens\":50}}\n\ndata: [DONE]\n"),
			want: wantStats{
				totalTokens:     50,
				premiumRequests: 0.0,
				models:          map[string]int{"gpt-4o": 1},
			},
		},
		{
			name: "empty model name falls back to 'unknown' with weight 1",
			task: "task-5",
			body: sseBody(`{"model":"","usage":{"total_tokens":75}}`),
			want: wantStats{
				totalTokens:     75,
				premiumRequests: 1.0,
				models:          map[string]int{"unknown": 1},
			},
		},
		{
			name: "no data lines — no updates",
			task: "task-6",
			body: []byte("event: start\n\ndata: [DONE]\n"),
			want: wantStats{
				totalTokens:     0,
				premiumRequests: 0.0,
				models:          map[string]int{},
			},
		},
		{
			name: "multiple usage chunks accumulate",
			task: "task-7",
			body: sseBody(
				`{"model":"gpt-4o","usage":{"total_tokens":100}}`,
				`{"model":"gpt-4o","usage":{"total_tokens":200}}`,
			),
			want: wantStats{
				totalTokens:     300,
				premiumRequests: 0.0,
				models:          map[string]int{"gpt-4o": 2},
			},
		},
		{
			name: "mixed models accumulate independently",
			task: "task-8",
			body: sseBody(
				`{"model":"gpt-4o","usage":{"total_tokens":100}}`,
				`{"model":"claude-sonnet-4-5","usage":{"total_tokens":200}}`,
			),
			want: wantStats{
				totalTokens:     300,
				premiumRequests: 1.0, // 0 (gpt-4o) + 1 (claude-sonnet-4-5)
				models:          map[string]int{"gpt-4o": 1, "claude-sonnet-4-5": 1},
			},
		},
		{
			name: "no prompt_tokens_details — cached defaults to 0",
			task: "task-9",
			body: sseBody(`{"model":"gpt-4o","usage":{"total_tokens":80,"reasoning_tokens":5}}`),
			want: wantStats{
				totalTokens:     80,
				cachedTokens:    0,
				reasoningTokens: 5,
				premiumRequests: 0.0,
				models:          map[string]int{"gpt-4o": 1},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withFreshStore(t)
			withTempFiles(t)

			processResponseBody(tc.task, tc.body)

			g := store.Global
			if g.TotalTokens != tc.want.totalTokens {
				t.Errorf("Global.TotalTokens = %d, want %d", g.TotalTokens, tc.want.totalTokens)
			}
			if g.CachedTokens != tc.want.cachedTokens {
				t.Errorf("Global.CachedTokens = %d, want %d", g.CachedTokens, tc.want.cachedTokens)
			}
			if g.ReasoningTokens != tc.want.reasoningTokens {
				t.Errorf("Global.ReasoningTokens = %d, want %d", g.ReasoningTokens, tc.want.reasoningTokens)
			}
			if g.PremiumRequests != tc.want.premiumRequests {
				t.Errorf("Global.PremiumRequests = %v, want %v", g.PremiumRequests, tc.want.premiumRequests)
			}
			for model, count := range tc.want.models {
				if got := g.Models[model]; got != count {
					t.Errorf("Global.Models[%q] = %d, want %d", model, got, count)
				}
			}
			// No unexpected models should be present.
			if len(g.Models) != len(tc.want.models) {
				t.Errorf("Global.Models has %d entries, want %d: %v", len(g.Models), len(tc.want.models), g.Models)
			}
		})
	}
}

// ── getOrCreateMonthlyRecord ──────────────────────────────

func TestGetOrCreateMonthlyRecord(t *testing.T) {
	tests := []struct {
		name         string
		existing     map[string]int // monthKey → TotalCalls seed
		requestKey   string
		wantExisting bool   // true if the returned record should be pre-populated
		wantPruned   []string // month keys that should be gone after the call
		wantRetained []string // month keys that should still exist
	}{
		{
			name:         "creates new record when absent",
			existing:     map[string]int{},
			requestKey:   "2026-04",
			wantExisting: false,
		},
		{
			name:         "returns existing record without reset",
			existing:     map[string]int{"2026-04": 5},
			requestKey:   "2026-04",
			wantExisting: true,
		},
		{
			name:       "prunes stale months older than previous",
			existing:   map[string]int{"2025-01": 1, "2025-06": 2},
			requestKey: currentMonthKey(),
			wantPruned: []string{"2025-01", "2025-06"},
		},
		{
			name: "retains current and previous month",
			existing: map[string]int{
				currentMonthKey(): 10,
				previousMonthKey(): 5,
				"2024-01": 3, // stale
			},
			requestKey:   currentMonthKey(),
			wantRetained: []string{currentMonthKey(), previousMonthKey()},
			wantPruned:   []string{"2024-01"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withFreshStore(t)

			// Seed the store with pre-existing monthly records.
			storeMu.Lock()
			for k, calls := range tc.existing {
				r := newTaskRecord()
				r.TotalCalls = calls
				store.Monthly[k] = r
			}

			mr := getOrCreateMonthlyRecord(tc.requestKey)

			if tc.wantExisting {
				if mr.TotalCalls == 0 {
					t.Error("expected existing record with TotalCalls > 0, got fresh record")
				}
			}
			for _, k := range tc.wantPruned {
				if _, ok := store.Monthly[k]; ok {
					t.Errorf("stale month %q was not pruned", k)
				}
			}
			for _, k := range tc.wantRetained {
				if _, ok := store.Monthly[k]; !ok {
					t.Errorf("month %q was incorrectly pruned", k)
				}
			}
			storeMu.Unlock()
		})
	}
}

// previousMonthKey returns the YYYY-MM key for last month, used in pruning tests.
func previousMonthKey() string {
	return time.Now().AddDate(0, -1, 0).Format("2006-01")
}
