package service

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"
)

// ============================================================================
// TraceAdminService 会话 trace 的后台管理：列表 / 统计 / 归档 / 下载 / 定时归档。
// 数据全部来自 trace 包落盘的文件系统（traces/ 与 traces-archive/），
// 不进数据库；API Key 名称仅做只读 join 展示。
// ============================================================================

type TraceAdminService struct {
	sqlDB      *sql.DB // 仅用于 api_keys 名称 join，可为 nil（退化为显示 ID）
	traceDir   string  // data/traces
	archiveDir string  // data/traces-archive
	settPath   string  // data/trace-settings.json

	settMu sync.RWMutex
	sett   TraceArchiveSettings

	schedOnce sync.Once
}

// TraceArchiveSettings 定时归档设置（独立 JSON 文件，与官方 settings 表隔离）。
type TraceArchiveSettings struct {
	Enabled     bool   `json:"enabled"`       // 自动归档开关
	TimeOfDay   string `json:"time_of_day"`   // "03:00"
	KeepHotDays int    `json:"keep_hot_days"` // 热数据保留天数（含今天），默认 1
}

func DefaultTraceArchiveSettings() TraceArchiveSettings {
	return TraceArchiveSettings{Enabled: true, TimeOfDay: "03:00", KeepHotDays: 1}
}

// TraceSessionRow 会话列表行。
type TraceSessionRow struct {
	Date      string `json:"date"` // 20260902
	SessionID string `json:"session_id"`
	Turns     int    `json:"turns"`
	SizeBytes int64  `json:"size_bytes"`
	Archived  bool   `json:"archived"`
	FirstAt   string `json:"first_at"`
	LastAt    string `json:"last_at"`
	Model     string `json:"model"`
	APIKeyID  int64  `json:"api_key_id"`
	APIKey    string `json:"api_key_name"` // join 自 api_keys.name，删除的 key 显示 "已删除(id)"
	UserID    int64  `json:"user_id"`
}

// TraceStats 顶部统计卡。
type TraceStats struct {
	TodayTurns    int   `json:"today_turns"`
	HotBytes      int64 `json:"hot_bytes"`
	ArchivedBytes int64 `json:"archived_bytes"`
	TotalSessions int   `json:"total_sessions"`
}

func NewTraceAdminService(sqlDB *sql.DB) *TraceAdminService {
	dir := os.Getenv("TRACE_DIR")
	if dir == "" {
		dir = "data/traces"
	}
	s := &TraceAdminService{
		sqlDB:      sqlDB,
		traceDir:   dir,
		archiveDir: filepath.Join(filepath.Dir(dir), "traces-archive"),
		settPath:   filepath.Join(filepath.Dir(dir), "trace-settings.json"),
		sett:       DefaultTraceArchiveSettings(),
	}
	_ = s.loadSettings()
	return s
}

// StartScheduler 进程内定时归档调度器（每 30 秒检查一次是否到点）。
func (s *TraceAdminService) StartScheduler(ctx context.Context) {
	s.schedOnce.Do(func() {
		go func() {
			lastRun := ""
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-ticker.C:
					st := s.GetSettings()
					if !st.Enabled {
						continue
					}
					hh, mm, ok := parseHHMM(st.TimeOfDay)
					if !ok {
						continue
					}
					today := now.Format("2006-01-02")
					if now.Hour() == hh && now.Minute() == mm && lastRun != today {
						lastRun = today
						_, _ = s.ArchiveOlderThan(ctx, st.KeepHotDays)
					}
				}
			}
		}()
	})
}

func parseHHMM(v string) (int, int, bool) {
	parts := strings.Split(strings.TrimSpace(v), ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// ---------- 设置 ----------

func (s *TraceAdminService) GetSettings() TraceArchiveSettings {
	s.settMu.RLock()
	defer s.settMu.RUnlock()
	return s.sett
}

func (s *TraceAdminService) SaveSettings(in TraceArchiveSettings) error {
	if _, _, ok := parseHHMM(in.TimeOfDay); !ok {
		return errors.New("time_of_day 格式应为 HH:MM")
	}
	if in.KeepHotDays < 1 {
		in.KeepHotDays = 1
	}
	s.settMu.Lock()
	s.sett = in
	s.settMu.Unlock()
	data, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.settPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.settPath, data, 0o644)
}

func (s *TraceAdminService) loadSettings() error {
	data, err := os.ReadFile(s.settPath)
	if err != nil {
		return err
	}
	var st TraceArchiveSettings
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}
	s.settMu.Lock()
	if st.KeepHotDays < 1 {
		st.KeepHotDays = 1
	}
	s.sett = st
	s.settMu.Unlock()
	return nil
}

// ---------- 列表与统计 ----------

// ListSessions 聚合热数据目录与归档目录。filters 零值即不过滤。
func (s *TraceAdminService) ListSessions(ctx context.Context, date, sessionID string, apiKeyID int64, archived *bool) ([]TraceSessionRow, error) {
	rows := make([]TraceSessionRow, 0, 64)
	if archived == nil || !*archived {
		hot, err := s.scanHot(ctx)
		if err != nil {
			return nil, err
		}
		rows = append(rows, hot...)
	}
	if archived == nil || *archived {
		cold, err := s.scanArchived(ctx)
		if err != nil {
			return nil, err
		}
		rows = append(rows, cold...)
	}

	filtered := rows[:0]
	for _, r := range rows {
		if date != "" && r.Date != date {
			continue
		}
		if sessionID != "" && !strings.Contains(r.SessionID, sessionID) {
			continue
		}
		if apiKeyID > 0 && r.APIKeyID != apiKeyID {
			continue
		}
		filtered = append(filtered, r)
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].Date != filtered[j].Date {
			return filtered[i].Date > filtered[j].Date
		}
		return filtered[i].LastAt > filtered[j].LastAt
	})
	s.fillAPIKeyNames(ctx, filtered)
	return filtered, nil
}

func (s *TraceAdminService) Stats(ctx context.Context) (*TraceStats, error) {
	stats := &TraceStats{}
	today := time.Now().Format("20060102")
	seen := map[string]bool{}

	hot, err := s.scanHot(ctx)
	if err == nil {
		for _, r := range hot {
			stats.HotBytes += r.SizeBytes
			seen[r.SessionID] = true
			if r.Date == today {
				stats.TodayTurns += r.Turns
			}
		}
	}
	cold, err := s.scanArchived(ctx)
	if err == nil {
		for _, r := range cold {
			stats.ArchivedBytes += r.SizeBytes
			seen[r.SessionID] = true
		}
	}
	stats.TotalSessions = len(seen)
	return stats, nil
}

// scanHot 扫 traces/<keyDir>/<日期>/<会话>/ 三层目录。
func (s *TraceAdminService) scanHot(ctx context.Context) ([]TraceSessionRow, error) {
	rows := []TraceSessionRow{}
	keyDirs, err := os.ReadDir(s.traceDir)
	if err != nil {
		if os.IsNotExist(err) {
			return rows, nil
		}
		return nil, err
	}
	for _, kd := range keyDirs {
		if !kd.IsDir() {
			continue
		}
		dayDirs, err := os.ReadDir(filepath.Join(s.traceDir, kd.Name()))
		if err != nil {
			continue
		}
		for _, dd := range dayDirs {
			if !dd.IsDir() {
				continue
			}
			date := dd.Name()
			sessDirs, err := os.ReadDir(filepath.Join(s.traceDir, kd.Name(), date))
			if err != nil {
				continue
			}
			for _, sd := range sessDirs {
				if !sd.IsDir() {
					continue
				}
				select {
				case <-ctx.Done():
					return rows, ctx.Err()
				default:
				}
				rows = append(rows, s.scanSessionDir(date, sd.Name(), filepath.Join(s.traceDir, kd.Name(), date, sd.Name())))
			}
		}
	}
	return rows, nil
}

func (s *TraceAdminService) scanSessionDir(date, sid, dir string) TraceSessionRow {
	row := TraceSessionRow{Date: date, SessionID: sid}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return row
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json.gz") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	row.Turns = len(names)
	if len(names) == 0 {
		return row
	}
	var total int64
	for _, n := range names {
		if info, err := os.Stat(filepath.Join(dir, n)); err == nil {
			total += info.Size()
		}
	}
	row.SizeBytes = total
	// 首轮文件读 meta（api key、user、model、首末时间）
	if meta := s.readRecordMeta(filepath.Join(dir, names[0])); meta != nil {
		row.APIKeyID = meta.APIKeyID
		row.UserID = meta.UserID
		row.Model = meta.Model
		row.FirstAt = meta.StartedAt
	}
	row.LastAt = row.FirstAt
	if meta := s.readRecordMeta(filepath.Join(dir, names[len(names)-1])); meta != nil {
		row.LastAt = meta.StartedAt
	}
	return row
}

type traceRecordMeta struct {
	APIKeyID  int64  `json:"api_key_id"`
	UserID    int64  `json:"user_id"`
	Model     string `json:"model"`
	StartedAt string `json:"started_at"`
}

// readRecordMeta 只解码 meta 字段，不加载 request/response 大字段。
// 注意：meta 字段在 JSON 头部但整个文件可达数 MB，截断读取后
// json.Unmarshal 必然失败——用 gjson 直接取字段，容忍截断。
func (s *TraceAdminService) readRecordMeta(path string) *traceRecordMeta {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil
	}
	defer gz.Close()
	head := make([]byte, 8192)
	n, _ := io.ReadAtLeast(gz, head, 1)
	return metaFromHead(head[:n])
}

func metaFromHead(head []byte) *traceRecordMeta {
	return &traceRecordMeta{
		APIKeyID:  gjson.GetBytes(head, "api_key_id").Int(),
		UserID:    gjson.GetBytes(head, "user_id").Int(),
		Model:     gjson.GetBytes(head, "model").String(),
		StartedAt: gjson.GetBytes(head, "started_at").String(),
	}
}

// scanArchived 扫 traces-archive/<keyDir>/<日期>/<会话>.tar.zst。
func (s *TraceAdminService) scanArchived(ctx context.Context) ([]TraceSessionRow, error) {
	rows := []TraceSessionRow{}
	keyDirs, err := os.ReadDir(s.archiveDir)
	if err != nil {
		if os.IsNotExist(err) {
			return rows, nil
		}
		return nil, err
	}
	for _, kd := range keyDirs {
		if !kd.IsDir() {
			continue
		}
		dayDirs, err := os.ReadDir(filepath.Join(s.archiveDir, kd.Name()))
		if err != nil {
			continue
		}
		for _, dd := range dayDirs {
			if !dd.IsDir() {
				continue
			}
			date := dd.Name()
			files, err := os.ReadDir(filepath.Join(s.archiveDir, kd.Name(), date))
			if err != nil {
				continue
			}
			for _, f := range files {
				name := f.Name()
				if !strings.HasSuffix(name, ".tar.zst") {
					continue
				}
				select {
				case <-ctx.Done():
					return rows, ctx.Err()
				default:
				}
				row := TraceSessionRow{
					Date:      date,
					SessionID: strings.TrimSuffix(name, ".tar.zst"),
					Archived:  true,
				}
				if info, err := f.Info(); err == nil {
					row.SizeBytes = info.Size()
					row.LastAt = info.ModTime().Format(time.RFC3339)
				}
				s.fillArchiveMeta(&row, filepath.Join(s.archiveDir, kd.Name(), date, name))
				rows = append(rows, row)
			}
		}
	}
	return rows, nil
}

// archiveMetaSidecar 是归档时写入的元信息旁车文件，避免列表/统计时解压整个压缩包
// （压缩包解压后可达数百 MB，逐包流式扫描会把接口拖到超时）。
type archiveMetaSidecar struct {
	Turns    int    `json:"turns"`
	APIKeyID int64  `json:"api_key_id"`
	UserID   int64  `json:"user_id"`
	Model    string `json:"model"`
	FirstAt  string `json:"first_at"`
}

// fillArchiveMeta 优先读旁车 meta；缺失时做一次完整扫描并补写旁车。
func (s *TraceAdminService) fillArchiveMeta(row *TraceSessionRow, path string) {
	if meta, err := s.readArchiveSidecar(path); err == nil && meta != nil {
		row.Turns = meta.Turns
		row.APIKeyID = meta.APIKeyID
		row.UserID = meta.UserID
		row.Model = meta.Model
		row.FirstAt = meta.FirstAt
		return
	}
	meta := s.scanArchiveMeta(path)
	if meta == nil {
		return
	}
	row.Turns = meta.Turns
	row.APIKeyID = meta.APIKeyID
	row.UserID = meta.UserID
	row.Model = meta.Model
	row.FirstAt = meta.FirstAt
	_ = s.writeArchiveSidecar(path, meta)
}

func (s *TraceAdminService) readArchiveSidecar(archivePath string) (*archiveMetaSidecar, error) {
	data, err := os.ReadFile(archivePath + ".meta.json")
	if err != nil {
		return nil, err
	}
	var m archiveMetaSidecar
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *TraceAdminService) writeArchiveSidecar(archivePath string, m *archiveMetaSidecar) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(archivePath+".meta.json", data, 0o644)
}

// scanArchiveMeta 完整流式扫描压缩包：数轮次 + 读首个文件 meta。慢，仅用于补建旁车。
func (s *TraceAdminService) scanArchiveMeta(path string) *archiveMetaSidecar {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		return nil
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	meta := &archiveMetaSidecar{}
	first := true
	for {
		hdr, err := tr.Next()
		if err != nil {
			if meta.Turns == 0 {
				return nil
			}
			return meta
		}
		if hdr.Typeflag != tar.TypeReg || !strings.HasSuffix(hdr.Name, ".json") {
			continue
		}
		meta.Turns++
		if first {
			first = false
			head := make([]byte, 8192)
			n, _ := io.ReadAtLeast(tr, head, 1)
			rec := metaFromHead(head[:n])
			meta.APIKeyID = rec.APIKeyID
			meta.UserID = rec.UserID
			meta.Model = rec.Model
			meta.FirstAt = rec.StartedAt
		}
	}
}

// fillAPIKeyNames 批量 join api_keys 名称。
func (s *TraceAdminService) fillAPIKeyNames(ctx context.Context, rows []TraceSessionRow) {
	if s.sqlDB == nil {
		return
	}
	idSet := map[int64]bool{}
	for _, r := range rows {
		if r.APIKeyID > 0 {
			idSet[r.APIKeyID] = true
		}
	}
	if len(idSet) == 0 {
		return
	}
	ids := make([]int64, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	q := fmt.Sprintf("SELECT id, name FROM api_keys WHERE id IN (%s)", strings.Join(placeholders, ","))
	rs, err := s.sqlDB.QueryContext(ctx, q, args...)
	if err != nil {
		return
	}
	defer rs.Close()
	names := map[int64]string{}
	for rs.Next() {
		var id int64
		var name string
		if rs.Scan(&id, &name) == nil {
			names[id] = name
		}
	}
	for i := range rows {
		if rows[i].APIKeyID == 0 {
			continue
		}
		if n, ok := names[rows[i].APIKeyID]; ok {
			rows[i].APIKey = n
		} else {
			rows[i].APIKey = fmt.Sprintf("已删除(id:%d)", rows[i].APIKeyID)
		}
	}
}

// ---------- 归档 ----------

// ArchiveResult 单日归档结果。
type ArchiveResult struct {
	Date     string   `json:"date"`
	Sessions int      `json:"sessions"`
	Turns    int      `json:"turns"`
	Kept     bool     `json:"kept_originals"` // 今天的热数据归档后保留原目录
	Archived []string `json:"archived"`
	Skipped  []string `json:"skipped"`
}

// ArchiveDay 归档所有 key 下指定日期的会话。
// isToday=true 时压缩后保留原始目录（仍在被写入）。
// 逐会话：解压 → tar → zstd 极致压缩 → 校验文件数一致 → （非今天）删除原目录。
func (s *TraceAdminService) ArchiveDay(_ context.Context, date string) (*ArchiveResult, error) {
	isToday := date == time.Now().Format("20060102")
	res := &ArchiveResult{Date: date, Kept: isToday}

	keyDirs, err := os.ReadDir(s.traceDir)
	if err != nil {
		return nil, fmt.Errorf("日期目录不存在: %s", date)
	}
	found := false
	for _, kd := range keyDirs {
		if !kd.IsDir() {
			continue
		}
		src := filepath.Join(s.traceDir, kd.Name(), date)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		found = true
		dst := filepath.Join(s.archiveDir, kd.Name(), date)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return res, err
		}
		sessDirs, err := os.ReadDir(src)
		if err != nil {
			return res, err
		}
		for _, sd := range sessDirs {
			if !sd.IsDir() {
				continue
			}
			sid := sd.Name()
			outPath := filepath.Join(dst, sid+".tar.zst")
			if _, err := os.Stat(outPath); err == nil {
				res.Skipped = append(res.Skipped, sid)
				continue
			}
			turns, err := s.archiveSessionDir(filepath.Join(src, sid), outPath)
			if err != nil {
				return res, fmt.Errorf("归档会话 %s/%s 失败: %w", kd.Name(), sid, err)
			}
			res.Turns += turns
			res.Sessions++
			res.Archived = append(res.Archived, kd.Name()+"/"+sid)
			if !isToday {
				_ = os.RemoveAll(filepath.Join(src, sid))
			}
		}
		// 全部会话归档完且非今天：删掉空的日期目录
		if !isToday {
			if entries, _ := os.ReadDir(src); len(entries) == 0 {
				_ = os.Remove(src)
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("日期目录不存在: %s", date)
	}
	return res, nil
}

// ArchiveOlderThan 归档所有 key 下早于保留期的日期目录（定时任务用）。
func (s *TraceAdminService) ArchiveOlderThan(ctx context.Context, keepHotDays int) ([]string, error) {
	cutoff := time.Now().AddDate(0, 0, -(keepHotDays - 1)).Format("20060102")
	keyDirs, err := os.ReadDir(s.traceDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	dateSet := map[string]bool{}
	for _, kd := range keyDirs {
		if !kd.IsDir() {
			continue
		}
		dayDirs, err := os.ReadDir(filepath.Join(s.traceDir, kd.Name()))
		if err != nil {
			continue
		}
		for _, dd := range dayDirs {
			if dd.IsDir() && dd.Name() < cutoff {
				dateSet[dd.Name()] = true
			}
		}
	}
	done := []string{}
	for date := range dateSet {
		if _, err := s.ArchiveDay(ctx, date); err != nil {
			return done, err
		}
		done = append(done, date)
	}
	sort.Strings(done)
	return done, nil
}

// archiveSessionDir 核心压缩：gz 解压 → tar → zstd（极致压缩 + 长窗口吃跨轮重复）。
// 返回轮次数；写入失败会清理半成品。
func (s *TraceAdminService) archiveSessionDir(srcDir, outPath string) (int, error) {
	tmpPath := outPath + ".tmp"
	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	// 失败清理
	defer func() {
		out.Close()
		if _, err := os.Stat(outPath); os.IsNotExist(err) {
			os.Remove(tmpPath)
		}
	}()

	enc, err := zstd.NewWriter(out,
		zstd.WithEncoderLevel(zstd.SpeedBestCompression),
		zstd.WithWindowSize(1<<27), // 128MB 长窗口：跨轮历史去重
	)
	if err != nil {
		return 0, err
	}
	tw := tar.NewWriter(enc)

	count := 0
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		tw.Close()
		enc.Close()
		return 0, err
	}
	names := []string{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json.gz") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		plain, err := readGzipFile(filepath.Join(srcDir, n))
		if err != nil {
			tw.Close()
			enc.Close()
			return 0, fmt.Errorf("解压 %s 失败: %w", n, err)
		}
		base := strings.TrimSuffix(n, ".gz")
		if err := tw.WriteHeader(&tar.Header{Name: base, Mode: 0o644, Size: int64(len(plain)), ModTime: time.Now()}); err != nil {
			tw.Close()
			enc.Close()
			return 0, err
		}
		if _, err := tw.Write(plain); err != nil {
			tw.Close()
			enc.Close()
			return 0, err
		}
		count++
	}
	if err := tw.Close(); err != nil {
		enc.Close()
		return 0, err
	}
	if err := enc.Close(); err != nil {
		return 0, err
	}
	if err := out.Close(); err != nil {
		return 0, err
	}
	// 校验：重新打开数文件数
	verify, err := countTarEntries(tmpPath)
	if err != nil || verify != count {
		return 0, fmt.Errorf("归档校验失败: 文件数 %d != %d (%v)", verify, count, err)
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		return 0, err
	}
	// 写元信息旁车：列表/统计页不再解压整个包
	if firstMeta := s.readFirstRecordMeta(srcDir); firstMeta != nil {
		_ = s.writeArchiveSidecar(outPath, &archiveMetaSidecar{
			Turns:    count,
			APIKeyID: firstMeta.APIKeyID,
			UserID:   firstMeta.UserID,
			Model:    firstMeta.Model,
			FirstAt:  firstMeta.StartedAt,
		})
	}
	return count, nil
}

// readFirstRecordMeta 读会话目录里最早一轮的 meta（写旁车用）。
func (s *TraceAdminService) readFirstRecordMeta(dir string) *traceRecordMeta {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := []string{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json.gz") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	return s.readRecordMeta(filepath.Join(dir, names[0]))
}

func readGzipFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	return io.ReadAll(gz)
}

func countTarEntries(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		return 0, err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	n := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		if hdr.Typeflag == tar.TypeReg {
			n++
		}
	}
}

// ---------- 下载 ----------

// ResolveDownload 返回可下载的文件路径与下载文件名。
// keyDir 为 key-<apiKeyID> 形式（列表行的 api_key_id 推导，0/负数视为非法）。
// 已归档：直接给 .tar.zst；热数据：现场快速压缩到临时文件（保留原目录），调用方负责用后删除。
func (s *TraceAdminService) ResolveDownload(keyDir, date, sid string) (path, downloadName string, cleanup func(), err error) {
	if !fs.ValidPath(sid) || strings.Contains(sid, "/") || strings.Contains(sid, "..") {
		return "", "", nil, errors.New("非法会话 ID")
	}
	if !fs.ValidPath(date) || strings.Contains(date, "/") || strings.Contains(date, "..") {
		return "", "", nil, errors.New("非法日期")
	}
	if !fs.ValidPath(keyDir) || strings.Contains(keyDir, "/") || strings.Contains(keyDir, "..") || !strings.HasPrefix(keyDir, "key-") {
		return "", "", nil, errors.New("非法 key 目录")
	}
	downloadName = fmt.Sprintf("trace_%s_%s_%s.tar.zst", keyDir, date, sid)

	archived := filepath.Join(s.archiveDir, keyDir, date, sid+".tar.zst")
	if _, statErr := os.Stat(archived); statErr == nil {
		return archived, downloadName, func() {}, nil
	}

	srcDir := filepath.Join(s.traceDir, keyDir, date, sid)
	if _, statErr := os.Stat(srcDir); statErr != nil {
		return "", "", nil, errors.New("会话不存在")
	}
	// 热数据：现场压缩（默认速度档 + 长窗口），不删原目录
	tmp, err := os.CreateTemp("", "trace-dl-*.tar.zst")
	if err != nil {
		return "", "", nil, err
	}
	tmpPath := tmp.Name()
	enc, err := zstd.NewWriter(tmp, zstd.WithWindowSize(1<<25))
	if err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", "", nil, err
	}
	tw := tar.NewWriter(enc)
	names, rerr := filepath.Glob(filepath.Join(srcDir, "*.json.gz"))
	if rerr != nil || len(names) == 0 {
		tw.Close()
		enc.Close()
		tmp.Close()
		os.Remove(tmpPath)
		return "", "", nil, errors.New("会话目录为空")
	}
	sort.Strings(names)
	for _, n := range names {
		plain, derr := readGzipFile(n)
		if derr != nil {
			continue
		}
		base := strings.TrimSuffix(filepath.Base(n), ".gz")
		if tw.WriteHeader(&tar.Header{Name: base, Mode: 0o644, Size: int64(len(plain)), ModTime: time.Now()}) == nil {
			_, _ = tw.Write(plain)
		}
	}
	tw.Close()
	if cerr := enc.Close(); cerr != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return "", "", nil, cerr
	}
	if cerr := tmp.Close(); cerr != nil {
		os.Remove(tmpPath)
		return "", "", nil, cerr
	}
	return tmpPath, downloadName, func() { os.Remove(tmpPath) }, nil
}
