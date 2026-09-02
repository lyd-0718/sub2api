package service

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func setupTraceDirs(t *testing.T) (*TraceAdminService, string) {
	t.Helper()
	base := t.TempDir()
	svc := &TraceAdminService{
		traceDir:   filepath.Join(base, "traces"),
		archiveDir: filepath.Join(base, "traces-archive"),
		settPath:   filepath.Join(base, "trace-settings.json"),
		sett:       DefaultTraceArchiveSettings(),
	}
	return svc, base
}

func writeTestTurn(t *testing.T, dir, name string, rec map[string]any) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(rec)
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	if _, err := gz.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveDayPastDeletesOriginals(t *testing.T) {
	svc, _ := setupTraceDirs(t)
	sessDir := filepath.Join(svc.traceDir, "20260901", "sess-a")
	writeTestTurn(t, sessDir, "100000.000-req1.json.gz", map[string]any{
		"session_id": "sess-a", "model": "m1", "api_key_id": 7, "started_at": "2026-09-01T10:00:00+08:00",
		"request": map[string]any{}, "response": map[string]any{"complete": true},
	})
	writeTestTurn(t, sessDir, "100100.000-req2.json.gz", map[string]any{
		"session_id": "sess-a", "model": "m1", "api_key_id": 7, "started_at": "2026-09-01T10:01:00+08:00",
		"request": map[string]any{}, "response": map[string]any{"complete": true},
	})

	res, err := svc.ArchiveDay(context.Background(), "20260901")
	if err != nil {
		t.Fatal(err)
	}
	if res.Turns != 2 || res.Sessions != 1 || res.Kept {
		t.Fatalf("res = %+v", res)
	}
	// 原始目录已删
	if _, err := os.Stat(sessDir); !os.IsNotExist(err) {
		t.Fatal("past day originals should be deleted")
	}
	// 压缩包可读且含 2 个 json
	n, err := countTarEntries(filepath.Join(svc.archiveDir, "20260901", "sess-a.tar.zst"))
	if err != nil || n != 2 {
		t.Fatalf("tar entries = %d err = %v", n, err)
	}
}

func TestArchiveTodayKeepsOriginals(t *testing.T) {
	svc, _ := setupTraceDirs(t)
	today := time.Now().Format("20060102")
	sessDir := filepath.Join(svc.traceDir, today, "sess-live")
	writeTestTurn(t, sessDir, "100000.000-req1.json.gz", map[string]any{
		"session_id": "sess-live", "request": map[string]any{}, "response": map[string]any{},
	})
	res, err := svc.ArchiveDay(context.Background(), today)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Kept {
		t.Fatal("today archive must keep originals")
	}
	if _, err := os.Stat(sessDir); err != nil {
		t.Fatal("today originals must be kept")
	}
}

func TestListSessionsAndDownload(t *testing.T) {
	svc, _ := setupTraceDirs(t)
	today := time.Now().Format("20060102")
	sessDir := filepath.Join(svc.traceDir, today, "s1")
	writeTestTurn(t, sessDir, "100000.000-r1.json.gz", map[string]any{
		"session_id": "s1", "model": "kimi-k3", "api_key_id": 3, "started_at": "2026-09-02T10:00:00+08:00",
		"request": map[string]any{}, "response": map[string]any{"complete": true},
	})

	rows, err := svc.ListSessions(context.Background(), "", "", 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Turns != 1 || rows[0].Model != "kimi-k3" || rows[0].APIKeyID != 3 {
		t.Fatalf("rows = %+v", rows)
	}

	path, name, cleanup, err := svc.ResolveDownload(today, "s1")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !strings.HasSuffix(name, ".tar.zst") {
		t.Fatalf("name = %s", name)
	}
	// 热数据现场压缩：原目录保留
	if _, err := os.Stat(sessDir); err != nil {
		t.Fatal("hot download must keep originals")
	}
	if n, _ := countTarEntries(path); n != 1 {
		t.Fatalf("download entries = %d", n)
	}
}

func TestResolveDownloadPathTraversal(t *testing.T) {
	svc, _ := setupTraceDirs(t)
	if _, _, _, err := svc.ResolveDownload("20260901", "../etc"); err == nil {
		t.Fatal("path traversal must be rejected")
	}
}

func TestSettingsRoundtrip(t *testing.T) {
	svc, _ := setupTraceDirs(t)
	st := TraceArchiveSettings{Enabled: true, TimeOfDay: "04:30", KeepHotDays: 2}
	if err := svc.SaveSettings(st); err != nil {
		t.Fatal(err)
	}
	svc2 := &TraceAdminService{settPath: svc.settPath, sett: DefaultTraceArchiveSettings()}
	if err := svc2.loadSettings(); err != nil {
		t.Fatal(err)
	}
	got := svc2.GetSettings()
	if got.TimeOfDay != "04:30" || got.KeepHotDays != 2 || !got.Enabled {
		t.Fatalf("got = %+v", got)
	}
	if err := svc2.SaveSettings(TraceArchiveSettings{TimeOfDay: "25:00"}); err == nil {
		t.Fatal("invalid time must be rejected")
	}
}

// ---------- 账号用量导出 ----------

func TestExportPricingCostAndCSV(t *testing.T) {
	svc := &AccountUsageExportService{
		pricing: ExportPricing{Currency: "CNY", Models: map[string]ExportModelPricing{
			"m1": {Input: 4, Output: 16, CacheRead: 1},
		}},
	}
	svc.pricing.Models["m1"] = ExportModelPricing{Input: 4, Output: 16, CacheRead: 1}

	rows := []AccountUsageRow{
		{AccountName: "A", Period: "2026-09", Model: "m1", Requests: 10,
			InputTokens: 1_000_000, OutputTokens: 100_000, CacheReadTokens: 2_000_000, CacheCreationTokens: 0},
		{AccountName: "A", Period: "2026-09", Model: "unpriced", Requests: 1, InputTokens: 100},
	}
	// 手算费用（模拟 Query 的计价逻辑）
	pricing := svc.GetPricing()
	for i := range rows {
		r := &rows[i]
		if p, ok := pricing.Models[r.Model]; ok && p.set() {
			r.CostKnown = true
			r.Cost = float64(r.InputTokens+r.CacheCreationTokens)/1e6*p.Input +
				float64(r.OutputTokens)/1e6*p.Output +
				float64(r.CacheReadTokens)/1e6*p.CacheRead
		}
	}
	// 1M*4 + 0.1M*16 + 2M*1 = 4 + 1.6 + 2 = 7.6
	if rows[0].Cost != 7.6 {
		t.Fatalf("cost = %v", rows[0].Cost)
	}
	if rows[1].CostKnown {
		t.Fatal("unpriced model must be CostKnown=false")
	}

	var buf strings.Builder
	if err := svc.WriteCSV(rows, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "\xEF\xBB\xBF") {
		t.Fatal("CSV must start with UTF-8 BOM")
	}
	if !strings.Contains(out, "7.60") || !strings.Contains(out, "合计") {
		t.Fatalf("csv = %s", out)
	}
	// 有未定价模型时合计费用应为 "-"
	lines := strings.Split(strings.TrimSpace(out), "\n")
	last := lines[len(lines)-1]
	if !strings.HasSuffix(last, "-") {
		t.Fatalf("total row = %s", last)
	}
}

func TestParseDateRange(t *testing.T) {
	start, end, err := ParseDateRange("2026-09-01", "2026-09-30")
	if err != nil {
		t.Fatal(err)
	}
	if start.Format("2006-01-02") != "2026-09-01" || end.Format("2006-01-02") != "2026-10-01" {
		t.Fatalf("range = %v ~ %v", start, end)
	}
	if _, _, err := ParseDateRange("2026-09-30", "2026-09-01"); err == nil {
		t.Fatal("reversed range must be rejected")
	}
}

// 确认 zstd 归档包内文件内容逐字节还原
func TestArchiveContentIntegrity(t *testing.T) {
	svc, _ := setupTraceDirs(t)
	sessDir := filepath.Join(svc.traceDir, "20260901", "sess-x")
	want := map[string]any{"session_id": "sess-x", "payload": strings.Repeat("abc", 5000)}
	writeTestTurn(t, sessDir, "100000.000-r1.json.gz", want)

	if _, err := svc.ArchiveDay(context.Background(), "20260901"); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(svc.archiveDir, "20260901", "sess-x.tar.zst"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	if _, err := io.Copy(&buf, tr); err != nil {
		t.Fatal(err)
	}
	if hdr.Name != "100000.000-r1.json" {
		t.Fatalf("entry name = %s", hdr.Name)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(buf.String()), &got); err != nil {
		t.Fatal(err)
	}
	if got["payload"] != want["payload"] {
		t.Fatal("content mismatch after archive roundtrip")
	}
}
