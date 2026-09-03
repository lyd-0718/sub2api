package service

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// AccountUsageExportService 上游账号用量导出（报表用）。
// 数据：usage_logs 聚合（account_id 维度），费用按"导出专用独立定价"计算，
// 与系统计费的 PricingService 完全隔离（独立 JSON 文件存储）。
// ============================================================================

// ExportPricing 导出定价：币种 + 每模型三项单价（每百万 token）+ 导出控制。
type ExportPricing struct {
	Currency string                        `json:"currency"` // CNY / USD / ...
	Models   map[string]ExportModelPricing `json:"models"`
	// Aliases 模型名归并：{"k3": "kimi-k3"}，聚合与计价统一用归并后的名字。
	Aliases map[string]string `json:"aliases,omitempty"`
}

// NormalizeModel 应用别名归并。
func (p ExportPricing) NormalizeModel(model string) string {
	if p.Aliases == nil {
		return model
	}
	if target, ok := p.Aliases[model]; ok && target != "" {
		return target
	}
	return model
}

type ExportModelPricing struct {
	Input     float64 `json:"input"`      // 输入 / 百万 tok
	Output    float64 `json:"output"`     // 输出 / 百万 tok
	CacheRead float64 `json:"cache_read"` // 缓存读取 / 百万 tok
	// Excluded 为 true 时该模型不出现在 CSV 导出与合计中（预览中淡显标记）。
	Excluded bool `json:"excluded,omitempty"`
}

func (p ExportModelPricing) set() bool {
	return p.Input > 0 || p.Output > 0 || p.CacheRead > 0
}

// AccountUsageRow 导出/预览行：账号 × 周期 × 模型。
type AccountUsageRow struct {
	AccountID           int64   `json:"account_id"`
	AccountName         string  `json:"account_name"`
	Period              string  `json:"period"`
	Model               string  `json:"model"`
	Requests            int64   `json:"requests"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	Cost                float64 `json:"cost"`       // 独立定价核算
	CostKnown           bool    `json:"cost_known"` // 该模型未定价时为 false
	Currency            string  `json:"currency"`
	Excluded            bool    `json:"excluded"` // 定价表中标记不参与导出（预览淡显，CSV 跳过）
}

type AccountUsageExportService struct {
	sqlDB       *sql.DB
	pricingPath string

	pricingMu sync.RWMutex
	pricing   ExportPricing
}

func NewAccountUsageExportService(sqlDB *sql.DB) *AccountUsageExportService {
	dir := os.Getenv("TRACE_DIR")
	if dir == "" {
		dir = "data/traces"
	}
	s := &AccountUsageExportService{
		sqlDB:       sqlDB,
		pricingPath: filepath.Join(filepath.Dir(dir), "export-pricing.json"),
		pricing: ExportPricing{
			Currency: "CNY",
			Models:   map[string]ExportModelPricing{},
			// 默认别名：k3 系列是 kimi-k3 的短名/变体
			Aliases: map[string]string{"k3": "kimi-k3"},
		},
	}
	_ = s.loadPricing()
	return s
}

// ---------- 定价 ----------

func (s *AccountUsageExportService) GetPricing() ExportPricing {
	s.pricingMu.RLock()
	defer s.pricingMu.RUnlock()
	return s.pricing
}

func (s *AccountUsageExportService) SavePricing(p ExportPricing) error {
	if strings.TrimSpace(p.Currency) == "" {
		return errors.New("currency 不能为空")
	}
	if p.Models == nil {
		p.Models = map[string]ExportModelPricing{}
	}
	s.pricingMu.Lock()
	s.pricing = p
	s.pricingMu.Unlock()
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.pricingPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.pricingPath, data, 0o644)
}

func (s *AccountUsageExportService) loadPricing() error {
	data, err := os.ReadFile(s.pricingPath)
	if err != nil {
		return err
	}
	var p ExportPricing
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	if p.Models == nil {
		p.Models = map[string]ExportModelPricing{}
	}
	// 旧版定价文件没有 aliases 字段：补默认别名，避免升级后归并失效
	if p.Aliases == nil {
		p.Aliases = map[string]string{"k3": "kimi-k3"}
	}
	s.pricingMu.Lock()
	s.pricing = p
	s.pricingMu.Unlock()
	return nil
}

// PricedModelsSeen 返回近 90 天实际出现过的模型及各模型调用次数（定价页自动列模型用）。
func (s *AccountUsageExportService) PricedModelsSeen(ctx context.Context) (map[string]int64, error) {
	rows, err := s.sqlDB.QueryContext(ctx,
		`SELECT model, COUNT(*) FROM usage_logs
		 WHERE created_at >= NOW() - INTERVAL '90 days'
		 GROUP BY model ORDER BY 2 DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pricing := s.GetPricing()
	out := map[string]int64{}
	for rows.Next() {
		var m string
		var c int64
		if rows.Scan(&m, &c) == nil {
			out[pricing.NormalizeModel(m)] += c // 别名归并后计数合并
		}
	}
	return out, rows.Err()
}

// ---------- 聚合查询 ----------

var exportGranularityFormat = map[string]string{
	"day":   "YYYY-MM-DD",
	"week":  "IYYY-IW",
	"month": "YYYY-MM",
}

// Query 聚合 usage_logs。granularity: day|week|month|total；accountIDs 空 = 全部账号。
// groupBy: "account"（默认，账号×周期×模型）| "model"（账号维度抹掉，周期×模型聚合）。
// 成功口径沿用仓库约定（actual_cost > 0 为成功请求代理）。
func (s *AccountUsageExportService) Query(ctx context.Context, start, end time.Time, granularity string, accountIDs []int64, groupBy string) ([]AccountUsageRow, error) {
	bucketExpr := "'全部'"
	if f, ok := exportGranularityFormat[granularity]; ok {
		bucketExpr = fmt.Sprintf("TO_CHAR(ul.created_at, '%s')", f)
	}
	aggregateAccounts := groupBy == "model"
	accountSelect := "ul.account_id,\n       COALESCE(NULLIF(a.name, ''), '账号#' || ul.account_id::text) AS account_name,"
	accountGroup := "ul.account_id, a.name,"
	if aggregateAccounts {
		accountSelect = "0::bigint AS account_id,\n       '全部' AS account_name,"
		accountGroup = ""
	}

	args := []any{start, end}
	where := []string{"ul.created_at >= $1 AND ul.created_at < $2", "ul.actual_cost > 0"}
	if len(accountIDs) > 0 {
		ph := make([]string, len(accountIDs))
		for i, id := range accountIDs {
			ph[i] = fmt.Sprintf("$%d", i+3)
			args = append(args, id)
		}
		where = append(where, fmt.Sprintf("ul.account_id IN (%s)", strings.Join(ph, ",")))
	}

	q := fmt.Sprintf(`
SELECT %s
       %s AS period,
       ul.model,
       COUNT(*) AS requests,
       COALESCE(SUM(ul.input_tokens), 0),
       COALESCE(SUM(ul.output_tokens), 0),
       COALESCE(SUM(ul.cache_read_tokens), 0),
       COALESCE(SUM(ul.cache_creation_tokens), 0)
FROM usage_logs ul
LEFT JOIN accounts a ON a.id = ul.account_id
WHERE %s
GROUP BY %s period, ul.model
ORDER BY account_name, period, requests DESC`, accountSelect, bucketExpr, strings.Join(where, " AND "), accountGroup)

	rows, err := s.sqlDB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	pricing := s.GetPricing()
	// 按 账号×周期×归并后模型 合并（别名如 k3 → kimi-k3 的行会被加起来）
	merged := map[string]*AccountUsageRow{}
	order := []string{}
	for rows.Next() {
		var r AccountUsageRow
		if err := rows.Scan(&r.AccountID, &r.AccountName, &r.Period, &r.Model, &r.Requests,
			&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheCreationTokens); err != nil {
			return nil, err
		}
		r.Model = pricing.NormalizeModel(r.Model)
		key := fmt.Sprintf("%d|%s|%s", r.AccountID, r.Period, r.Model)
		if m, ok := merged[key]; ok {
			m.Requests += r.Requests
			m.InputTokens += r.InputTokens
			m.OutputTokens += r.OutputTokens
			m.CacheReadTokens += r.CacheReadTokens
			m.CacheCreationTokens += r.CacheCreationTokens
			continue
		}
		cp := r
		merged[key] = &cp
		order = append(order, key)
	}
	out := []AccountUsageRow{}
	for _, key := range order {
		r := merged[key]
		r.Currency = pricing.Currency
		if p, ok := pricing.Models[r.Model]; ok {
			r.Excluded = p.Excluded
			if p.set() {
				r.CostKnown = true
				// 缓存创建按输入价计（它本质就是输入），缓存读取用缓存价
				r.Cost = float64(r.InputTokens+r.CacheCreationTokens)/1e6*p.Input +
					float64(r.OutputTokens)/1e6*p.Output +
					float64(r.CacheReadTokens)/1e6*p.CacheRead
			}
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ---------- CSV ----------

// WriteCSV 导出 CSV（UTF-8 BOM，Excel 直开）。含合计行。
// aggregated=true（聚合模式）时不输出"账号"列。
func (s *AccountUsageExportService) WriteCSV(rows []AccountUsageRow, w io.Writer, aggregated bool) error {
	if _, err := w.Write([]byte("\xEF\xBB\xBF")); err != nil { // BOM
		return err
	}
	cw := csv.NewWriter(w)
	defer cw.Flush()

	pricing := s.GetPricing()
	cur := pricing.Currency
	header := []string{"账号", "周期", "模型", "请求数",
		"输入token", "输出token", "缓存读取token", "缓存写入token",
		fmt.Sprintf("token费用(%s)", cur)}
	if aggregated {
		header = header[1:]
	}
	if err := cw.Write(header); err != nil {
		return err
	}

	var totReq, totIn, totOut, totCR, totCC int64
	var totCost float64
	allKnown := true
	for _, r := range rows {
		if r.Excluded {
			continue // 不参与导出与合计
		}
		cost := "-"
		if r.CostKnown {
			cost = strconv.FormatFloat(r.Cost, 'f', 2, 64)
		} else {
			allKnown = false
		}
		rec := []string{r.Period, r.Model,
			strconv.FormatInt(r.Requests, 10),
			strconv.FormatInt(r.InputTokens, 10),
			strconv.FormatInt(r.OutputTokens, 10),
			strconv.FormatInt(r.CacheReadTokens, 10),
			strconv.FormatInt(r.CacheCreationTokens, 10),
			cost}
		if !aggregated {
			rec = append([]string{r.AccountName}, rec...)
		}
		if err := cw.Write(rec); err != nil {
			return err
		}
		totReq += r.Requests
		totIn += r.InputTokens
		totOut += r.OutputTokens
		totCR += r.CacheReadTokens
		totCC += r.CacheCreationTokens
		totCost += r.Cost
	}
	totalCost := "-"
	if allKnown {
		totalCost = strconv.FormatFloat(totCost, 'f', 2, 64)
	}
	nums := []string{
		strconv.FormatInt(totReq, 10),
		strconv.FormatInt(totIn, 10),
		strconv.FormatInt(totOut, 10),
		strconv.FormatInt(totCR, 10),
		strconv.FormatInt(totCC, 10),
		totalCost}
	// 合计行对齐列数：聚合模式 8 列（周期,模型,…），非聚合 9 列（账号,周期,模型,…）
	label := []string{"合计", ""}
	if !aggregated {
		label = []string{"合计", "", ""}
	}
	return cw.Write(append(label, nums...))
}

// ParseDateRange 解析 start/end（YYYY-MM-DD），end 为闭区间当日（内部转开区间）。
func ParseDateRange(startStr, endStr string) (time.Time, time.Time, error) {
	start, err := time.ParseInLocation("2006-01-02", startStr, time.Local)
	if err != nil {
		return time.Time{}, time.Time{}, errors.New("start 格式应为 YYYY-MM-DD")
	}
	end, err := time.ParseInLocation("2006-01-02", endStr, time.Local)
	if err != nil {
		return time.Time{}, time.Time{}, errors.New("end 格式应为 YYYY-MM-DD")
	}
	if end.Before(start) {
		return time.Time{}, time.Time{}, errors.New("end 不能早于 start")
	}
	return start, end.AddDate(0, 0, 1), nil
}

// SortAccountUsageRows 预览排序：账号名 → 周期 → 请求数降序。
func SortAccountUsageRows(rows []AccountUsageRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].AccountName != rows[j].AccountName {
			return rows[i].AccountName < rows[j].AccountName
		}
		if rows[i].Period != rows[j].Period {
			return rows[i].Period < rows[j].Period
		}
		return rows[i].Requests > rows[j].Requests
	})
}
