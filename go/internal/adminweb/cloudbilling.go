package adminweb

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

const (
	cloudBillingPrefix      = "admin/billing/"
	cloudBillingMaxManifest = 64 << 10
	cloudBillingMaxCSV      = 128 << 20
	cloudBillingMaxArchive  = 128 << 20
	cloudBillingMaxRows     = 500_000
)

var (
	cloudBillingMonthRE = regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])$`)
	cloudBillingHashRE  = regexp.MustCompile(`^[a-f0-9]{64}$`)
	cloudBillingMoneyRE = regexp.MustCompile(`^[+-]?[0-9]+(?:\.[0-9]+)?$`)
)

var cloudBillingColumns = []string{
	"source_id", "provider", "billing_period", "invoice_period", "locator", "scope",
	"project_id", "project_name", "subscription_id", "day", "usage_start", "usage_end",
	"region", "model", "tier", "currency", "service", "sku", "resource", "charge_type",
	"metric", "unit", "quantity", "comparable_cost", "net_cost", "unit_rate", "cost_basis",
	"disposition", "reason", "components_json",
}

type cloudBillingObjects interface {
	ListInfo(prefix string) ([]ossclient.ObjectInfo, error)
	Get(key string) ([]byte, string, error)
}

type cloudBillManifest struct {
	SchemaVersion    int    `json:"schema_version"`
	Period           string `json:"period"`
	GeneratedAt      string `json:"generated_at"`
	BatchFingerprint string `json:"batch_fingerprint"`
	CSV              struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
		Bytes  int64  `json:"bytes"`
		Rows   int    `json:"rows"`
	} `json:"csv"`
	Providers []struct {
		Provider        string   `json:"provider"`
		SourceID        string   `json:"source_id"`
		SourceVersion   string   `json:"source_version"`
		Fingerprint     string   `json:"fingerprint"`
		SourceUpdatedAt *string  `json:"source_updated_at"`
		FetchedAt       string   `json:"fetched_at"`
		Rows            int      `json:"rows"`
		CostBases       []string `json:"cost_bases"`
	} `json:"providers"`
}

type cloudBillSource struct {
	Provider, DisplayName, SourceID, CostBases, SourceUpdatedAt string
	Rows                                                        int
	Breakdowns                                                  []cloudBillCostBreakdown
}

type cloudBillCostBreakdown struct {
	Currency, CostBasis, Comparable, Unmapped, Adjustment, Excluded string
}

type cloudBillGroup struct {
	Provider, Scope, Currency, CostBasis, Disposition, Amount string
	Rows                                                      int
}

type cloudBillCandidate struct {
	SKU, Description, Model, Status, Currency, CostBasis, Amount string
	Rows                                                         int
}

type cloudBillPage struct {
	Month, ObjectKey, GeneratedAt, BatchFingerprint string
	Months                                          []string
	Rows                                            int
	Sources                                         []cloudBillSource
	Groups                                          []cloudBillGroup
	Scopes                                          []cloudBillGroup
	MoreScopes                                      int
	Candidates                                      []cloudBillCandidate
	MoreCandidates                                  int
	Empty                                           bool
}

type cloudBillAggregate struct {
	group  cloudBillGroup
	amount *big.Rat
}

type cloudBillCandidateAggregate struct {
	candidate cloudBillCandidate
	amount    *big.Rat
}

func cloudBillingMonth(value string) bool {
	if !cloudBillingMonthRE.MatchString(value) {
		return false
	}
	t, err := time.Parse("2006-01", value)
	return err == nil && t.Format("2006-01") == value
}

func cloudArchivePath(key string) (month string, ok bool) {
	if !strings.HasPrefix(key, cloudBillingPrefix) {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(key, cloudBillingPrefix), "/")
	if len(parts) != 2 || parts[1] != "bill.zip" || !cloudBillingMonth(parts[0]) {
		return "", false
	}
	return parts[0], true
}

func cloudBillingDecimal(value string, required bool) (*big.Rat, error) {
	if value == "" && !required {
		return nil, nil
	}
	if !cloudBillingMoneyRE.MatchString(value) {
		return nil, errors.New("账单包含无效金额或用量")
	}
	whole, fraction, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(value, "+"), "-"), ".")
	if len(whole) > 50 || len(fraction) > 100 {
		return nil, errors.New("账单金额或用量精度超限")
	}
	n, ok := new(big.Rat).SetString(value)
	if !ok {
		return nil, errors.New("账单包含无效金额或用量")
	}
	return n, nil
}

func cloudBillingAmount(value *big.Rat) string {
	if value == nil {
		return "0"
	}
	// Input decimals are limited to 100 places, so this is an exact decimal sum.
	return strings.TrimRight(strings.TrimRight(value.FloatString(100), "0"), ".")
}

func cloudBillingAdd(groups map[string]*cloudBillAggregate, key cloudBillGroup, amount *big.Rat) {
	id := key.Provider + "\x00" + key.Scope + "\x00" + key.Currency + "\x00" + key.CostBasis + "\x00" + key.Disposition
	entry := groups[id]
	if entry == nil {
		entry = &cloudBillAggregate{group: key, amount: new(big.Rat)}
		groups[id] = entry
	}
	entry.group.Rows++
	entry.amount.Add(entry.amount, amount)
}

func cloudBillingGroups(groups map[string]*cloudBillAggregate) []cloudBillGroup {
	out := make([]cloudBillGroup, 0, len(groups))
	for _, entry := range groups {
		entry.group.Amount = cloudBillingAmount(entry.amount)
		out = append(out, entry.group)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.Provider+"\x00"+a.Scope+"\x00"+a.Currency+"\x00"+a.CostBasis+"\x00"+a.Disposition <
			b.Provider+"\x00"+b.Scope+"\x00"+b.Currency+"\x00"+b.CostBasis+"\x00"+b.Disposition
	})
	return out
}

func cloudBillingSourceBreakdowns(sources []cloudBillSource, groups []cloudBillGroup) {
	for i := range sources {
		source := &sources[i]
		switch source.Provider {
		case "aws":
			source.DisplayName = "AWS Bedrock"
		case "azure":
			source.DisplayName = "Azure OpenAI"
		case "gcp":
			source.DisplayName = "Google Vertex AI"
		}
		byBasis := map[string]*cloudBillCostBreakdown{}
		for _, group := range groups {
			if group.Provider != source.Provider {
				continue
			}
			key := group.Currency + "\x00" + group.CostBasis
			item := byBasis[key]
			if item == nil {
				item = &cloudBillCostBreakdown{
					Currency: group.Currency, CostBasis: group.CostBasis,
					Comparable: "0", Unmapped: "0", Adjustment: "0", Excluded: "0",
				}
				byBasis[key] = item
			}
			switch group.Disposition {
			case "comparable":
				item.Comparable = group.Amount
			case "unmapped":
				item.Unmapped = group.Amount
			case "adjustment":
				item.Adjustment = group.Amount
			case "excluded":
				item.Excluded = group.Amount
			}
		}
		keys := make([]string, 0, len(byBasis))
		for key := range byBasis {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			source.Breakdowns = append(source.Breakdowns, *byBasis[key])
		}
	}
}

func cloudReadZipFile(file *zip.File, limit int64) ([]byte, error) {
	if file.UncompressedSize64 == 0 || file.UncompressedSize64 > uint64(limit) {
		return nil, errors.New("云账单压缩包内文件大小异常")
	}
	reader, err := file.Open()
	if err != nil {
		return nil, errors.New("云账单压缩包内文件无法读取")
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, limit+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || len(data) == 0 || int64(len(data)) > limit || uint64(len(data)) != file.UncompressedSize64 {
		return nil, errors.New("云账单压缩包内文件校验失败")
	}
	return data, nil
}

// loadCloudBilling reads one atomically replaced monthly archive. CSV and
// manifest are checked together before any amount is shown or downloaded.
func loadCloudBilling(objects cloudBillingObjects, requested string, now time.Time) (*cloudBillPage, []byte, error) {
	if requested != "" && !cloudBillingMonth(requested) {
		return nil, nil, errors.New("账期必须为 YYYY-MM")
	}
	infos, err := objects.ListInfo(cloudBillingPrefix)
	if err != nil {
		return nil, nil, fmt.Errorf("列出云账单失败: %w", err)
	}
	monthSet := map[string]bool{}
	byKey := make(map[string]ossclient.ObjectInfo, len(infos))
	for _, info := range infos {
		byKey[info.Key] = info
		if month, ok := cloudArchivePath(info.Key); ok {
			monthSet[month] = true
		}
	}
	months := make([]string, 0, len(monthSet))
	for month := range monthSet {
		months = append(months, month)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(months)))
	month := requested
	if month == "" {
		month = now.UTC().Format("2006-01")
		if len(months) > 0 {
			month = months[0]
		}
	}
	page := &cloudBillPage{Month: month, Months: months, Empty: true}
	if !monthSet[month] {
		return page, nil, nil
	}
	objectKey := cloudBillingPrefix + month + "/bill.zip"
	info := byKey[objectKey]
	if info.Size <= 0 || info.Size > cloudBillingMaxArchive {
		return page, nil, errors.New("云账单压缩包大小异常")
	}
	archiveBytes, _, err := objects.Get(objectKey)
	if err != nil {
		return page, nil, fmt.Errorf("读取云账单压缩包失败: %w", err)
	}
	// A monthly overwrite can happen between ListInfo and Get. The two calls
	// may legitimately describe different complete versions of the same ZIP;
	// validate the fetched object itself instead of comparing their sizes.
	if len(archiveBytes) == 0 || len(archiveBytes) > cloudBillingMaxArchive {
		return page, nil, errors.New("云账单压缩包大小异常")
	}
	archive, err := zip.NewReader(bytes.NewReader(archiveBytes), int64(len(archiveBytes)))
	if err != nil || len(archive.File) != 2 {
		return page, nil, errors.New("云账单压缩包格式错误")
	}
	files := map[string]*zip.File{}
	for _, file := range archive.File {
		if file.Name != "manifest.json" && file.Name != "billing-lines.csv" {
			return page, nil, errors.New("云账单压缩包含意外文件")
		}
		files[file.Name] = file
	}
	if len(files) != 2 {
		return page, nil, errors.New("云账单压缩包缺少文件")
	}
	manifestBytes, err := cloudReadZipFile(files["manifest.json"], cloudBillingMaxManifest)
	if err != nil {
		return page, nil, err
	}
	var manifest cloudBillManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return page, nil, errors.New("云账单 manifest 不是有效 JSON")
	}
	if manifest.SchemaVersion != 2 || manifest.Period != month ||
		manifest.CSV.Name != "billing-lines.csv" || !cloudBillingHashRE.MatchString(manifest.CSV.SHA256) ||
		!cloudBillingHashRE.MatchString(manifest.BatchFingerprint) || manifest.CSV.Rows < 0 || manifest.CSV.Rows > cloudBillingMaxRows {
		return page, nil, errors.New("云账单 manifest 版本或账期信息不匹配")
	}
	if _, err := time.Parse(time.RFC3339Nano, manifest.GeneratedAt); err != nil {
		return page, nil, errors.New("云账单生成时间无效")
	}
	if len(manifest.Providers) != 3 {
		return page, nil, errors.New("云账单缺少三家供应商来源")
	}
	sources := map[string]int{}
	bases := map[string]map[string]bool{}
	fingerprints := make([]string, 0, 3)
	for _, source := range manifest.Providers {
		if !slices.Contains([]string{"aws", "azure", "gcp"}, source.Provider) || sources[source.Provider] != 0 ||
			source.SourceID == "" || source.Rows < 0 || !cloudBillingHashRE.MatchString(source.Fingerprint) {
			return page, nil, errors.New("云账单供应商来源无效")
		}
		if _, err := time.Parse(time.RFC3339Nano, source.FetchedAt); err != nil {
			return page, nil, errors.New("云账单采集时间无效")
		}
		if source.SourceUpdatedAt != nil {
			if _, err := time.Parse(time.RFC3339Nano, *source.SourceUpdatedAt); err != nil {
				return page, nil, errors.New("云账单来源更新时间无效")
			}
		}
		// -1 means seen; zero rows is still a valid source and must not be
		// mistaken for an absent provider.
		sources[source.Provider] = -1
		bases[source.Provider] = map[string]bool{}
		for _, basis := range source.CostBases {
			bases[source.Provider][basis] = true
		}
		fingerprints = append(fingerprints, source.Fingerprint)
		updated := "—"
		if source.SourceUpdatedAt != nil {
			updated = *source.SourceUpdatedAt
		}
		page.Sources = append(page.Sources, cloudBillSource{
			Provider: source.Provider, SourceID: source.SourceID,
			CostBases: strings.Join(source.CostBases, ", "), SourceUpdatedAt: updated, Rows: source.Rows,
		})
	}
	if len(sources) != 3 {
		return page, nil, errors.New("云账单供应商来源重复")
	}
	batchInput, _ := json.Marshal(map[string]any{"period": month, "schema_version": 2, "snapshots": fingerprints})
	batchHash := sha256.Sum256(batchInput)
	if hex.EncodeToString(batchHash[:]) != manifest.BatchFingerprint {
		return page, nil, errors.New("云账单批次指纹不匹配")
	}
	csvBytes, err := cloudReadZipFile(files["billing-lines.csv"], cloudBillingMaxCSV)
	if err != nil {
		return page, nil, err
	}
	if int64(len(csvBytes)) != manifest.CSV.Bytes {
		return page, nil, errors.New("云账单 CSV 字节数不匹配")
	}
	hash := sha256.Sum256(csvBytes)
	if hex.EncodeToString(hash[:]) != manifest.CSV.SHA256 {
		return page, nil, errors.New("云账单 CSV SHA-256 不匹配")
	}
	reader := csv.NewReader(bytes.NewReader(csvBytes))
	reader.FieldsPerRecord = len(cloudBillingColumns)
	header, err := reader.Read()
	if err != nil || !slices.Equal(header, cloudBillingColumns) {
		return page, nil, errors.New("云账单 CSV 表头与版本不匹配")
	}
	counts := map[string]int{}
	allGroups := map[string]*cloudBillAggregate{}
	scopeGroups := map[string]*cloudBillAggregate{}
	candidateGroups := map[string]*cloudBillCandidateAggregate{}
	for {
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return page, nil, errors.New("云账单 CSV 行格式错误")
		}
		page.Rows++
		if page.Rows > cloudBillingMaxRows {
			return page, nil, errors.New("云账单 CSV 行数超限")
		}
		provider, basis, disposition := row[1], row[26], row[27]
		if sources[provider] != -1 || row[2] != month || row[0] == "" || row[4] == "" || row[5] == "" ||
			row[15] == "" || basis == "" || !bases[provider][basis] ||
			!slices.Contains([]string{"comparable", "unmapped", "adjustment"}, disposition) {
			return page, nil, errors.New("云账单 CSV 含不属于本批次的行")
		}
		var sourceID string
		for _, source := range manifest.Providers {
			if source.Provider == provider {
				sourceID = source.SourceID
				break
			}
		}
		if row[0] != sourceID {
			return page, nil, errors.New("云账单 CSV 来源 ID 不匹配")
		}
		if _, err := time.Parse(time.RFC3339Nano, row[10]); err != nil {
			return page, nil, errors.New("云账单 CSV 用量时间无效")
		}
		if _, err := time.Parse(time.RFC3339Nano, row[11]); err != nil {
			return page, nil, errors.New("云账单 CSV 用量时间无效")
		}
		if _, err := cloudBillingDecimal(row[22], true); err != nil {
			return page, nil, err
		}
		cost, err := cloudBillingDecimal(row[23], true)
		if err != nil {
			return page, nil, err
		}
		if _, err := cloudBillingDecimal(row[24], false); err != nil {
			return page, nil, err
		}
		if _, err := cloudBillingDecimal(row[25], false); err != nil {
			return page, nil, err
		}
		var components map[string]string
		if err := json.Unmarshal([]byte(row[29]), &components); err != nil || components == nil {
			return page, nil, errors.New("云账单 CSV 供应商字段不是有效 JSON")
		}
		counts[provider]++
		cloudBillingAdd(allGroups, cloudBillGroup{
			Provider: provider, Currency: row[15], CostBasis: basis, Disposition: disposition,
		}, cost)
		cloudBillingAdd(scopeGroups, cloudBillGroup{
			Provider: provider, Scope: row[5], Currency: row[15], CostBasis: basis, Disposition: disposition,
		}, cost)
		if provider == "gcp" && disposition == "unmapped" {
			status := "未识别"
			switch components["candidate_status"] {
			case "description_only":
				status = "仅凭描述推断"
			case "ambiguous":
				status = "多个模型共用 SKU"
			}
			candidate := cloudBillCandidate{
				SKU: components["sku_id"], Description: components["sku_description"],
				Model: components["candidate_model"], Status: status,
				Currency: row[15], CostBasis: basis,
			}
			if candidate.SKU == "" {
				candidate.SKU = row[17]
			}
			id := strings.Join([]string{candidate.SKU, candidate.Description, candidate.Model, candidate.Status, candidate.Currency, candidate.CostBasis}, "\x00")
			entry := candidateGroups[id]
			if entry == nil {
				entry = &cloudBillCandidateAggregate{candidate: candidate, amount: new(big.Rat)}
				candidateGroups[id] = entry
			}
			entry.candidate.Rows++
			entry.amount.Add(entry.amount, cost)
		}
	}
	if page.Rows != manifest.CSV.Rows {
		return page, nil, errors.New("云账单 CSV 行数不匹配")
	}
	for _, source := range manifest.Providers {
		if counts[source.Provider] != source.Rows {
			return page, nil, errors.New("云账单供应商行数不匹配")
		}
	}
	page.ObjectKey, page.GeneratedAt, page.BatchFingerprint = objectKey, manifest.GeneratedAt, manifest.BatchFingerprint
	page.Groups = cloudBillingGroups(allGroups)
	cloudBillingSourceBreakdowns(page.Sources, page.Groups)
	page.Scopes = cloudBillingGroups(scopeGroups)
	for _, entry := range candidateGroups {
		entry.candidate.Amount = cloudBillingAmount(entry.amount)
		page.Candidates = append(page.Candidates, entry.candidate)
	}
	sort.Slice(page.Candidates, func(i, j int) bool {
		a, b := page.Candidates[i], page.Candidates[j]
		return a.Model+"\x00"+a.SKU+"\x00"+a.Currency+"\x00"+a.CostBasis <
			b.Model+"\x00"+b.SKU+"\x00"+b.Currency+"\x00"+b.CostBasis
	})
	if len(page.Candidates) > 100 {
		page.MoreCandidates = len(page.Candidates) - 100
		page.Candidates = page.Candidates[:100]
	}
	if len(page.Scopes) > 100 {
		page.MoreScopes = len(page.Scopes) - 100
		page.Scopes = page.Scopes[:100]
	}
	page.Empty = false
	return page, csvBytes, nil
}

func (s *Server) handleCloudBilling(w http.ResponseWriter, r *http.Request, sess *session) {
	data := newPage(sess, r, "usage")
	data.Tab = "cloud"
	page, _, err := loadCloudBilling(s.dbm.objects, r.URL.Query().Get("month"), time.Now())
	if err != nil {
		data.Error = err.Error()
	}
	if page == nil {
		page = &cloudBillPage{Month: time.Now().UTC().Format("2006-01"), Empty: true}
	}
	data.CloudBillingPage = page
	s.render(w, "cloud_billing.html", http.StatusOK, data)
}

func (s *Server) handleCloudBillingCSV(w http.ResponseWriter, r *http.Request, _ *session) {
	page, content, err := loadCloudBilling(s.dbm.objects, r.URL.Query().Get("month"), time.Now())
	if err != nil {
		http.Error(w, "云账单校验失败："+err.Error(), http.StatusBadGateway)
		return
	}
	if page.Empty {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="cloud-billing-`+page.Month+`.csv"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(content)
}
