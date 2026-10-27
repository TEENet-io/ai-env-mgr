package adminweb

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

type staleSizeBillStore struct{ *fakeStore }

func (s staleSizeBillStore) ListInfo(prefix string) ([]ossclient.ObjectInfo, error) {
	infos, err := s.fakeStore.ListInfo(prefix)
	for i := range infos {
		if _, ok := cloudArchivePath(infos[i].Key); ok {
			infos[i].Size-- // Simulate a complete overwrite after the listing.
		}
	}
	return infos, err
}

func cloudBillZip(t *testing.T, manifest, csvBytes []byte) []byte {
	t.Helper()
	var body bytes.Buffer
	archive := zip.NewWriter(&body)
	for _, item := range []struct {
		name string
		data []byte
	}{{"billing-lines.csv", csvBytes}, {"manifest.json", manifest}} {
		writer, err := archive.Create(item.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(item.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func cloudBillZipContents(t *testing.T, body []byte) ([]byte, []byte) {
	t.Helper()
	archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, file := range archive.File {
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(reader); err != nil {
			t.Fatal(err)
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		files[file.Name] = buf.Bytes()
	}
	return files["manifest.json"], files["billing-lines.csv"]
}

func cloudBillFixture(t *testing.T, store *fakeStore) string {
	t.Helper()
	month := "2026-10"
	objectKey := cloudBillingPrefix + month + "/bill.zip"
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if err := w.Write(cloudBillingColumns); err != nil {
		t.Fatal(err)
	}
	providers := []string{"aws", "azure", "gcp"}
	bases := []string{"unblended_usage", "azure_actual_cost", "gcp_shared_view_list_price"}
	amounts := []string{"4.690269101", "1.06349822", "0.080287"}
	fingerprints := []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)}
	for i, provider := range providers {
		row := make([]string, len(cloudBillingColumns))
		values := map[string]string{
			"source_id":       provider + "-source",
			"provider":        provider,
			"billing_period":  month,
			"locator":         provider + ":1",
			"scope":           provider + "-scope",
			"day":             "2026-10-12",
			"usage_start":     "2026-10-12T00:00:00+00:00",
			"usage_end":       "2026-10-12T01:00:00+00:00",
			"currency":        "USD",
			"service":         "service",
			"sku":             "sku",
			"quantity":        "1",
			"comparable_cost": amounts[i],
			"cost_basis":      bases[i],
			"disposition":     "comparable",
			"components_json": `{}`,
		}
		if provider == "gcp" {
			values["sku"] = "0127-F0B7-365E"
			values["disposition"] = "unmapped"
			values["components_json"] = `{"sku_id":"0127-F0B7-365E","sku_description":"Gemini 3 Flash Text Output - Predictions","candidate_model":"gemini-3-flash","candidate_status":"description_only"}`
		}
		for index, name := range cloudBillingColumns {
			row[index] = values[name]
		}
		if err := w.Write(row); err != nil {
			t.Fatal(err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		t.Fatal(err)
	}
	csvBytes := buf.Bytes()
	csvHash := sha256.Sum256(csvBytes)
	batchInput, _ := json.Marshal(map[string]any{
		"period": month, "schema_version": 2, "snapshots": fingerprints,
	})
	batchHash := sha256.Sum256(batchInput)
	sources := make([]map[string]any, 3)
	for i, provider := range providers {
		sources[i] = map[string]any{
			"provider": provider, "source_id": provider + "-source", "source_version": "v1",
			"fingerprint": fingerprints[i], "source_updated_at": "2026-10-13T00:00:00+00:00",
			"fetched_at": "2026-10-14T00:00:00+00:00", "rows": 1,
			"cost_bases": []string{bases[i]},
		}
	}
	manifest := map[string]any{
		"schema_version": 2, "period": month,
		"generated_at":      "2026-10-14T00:00:00+00:00",
		"batch_fingerprint": hex.EncodeToString(batchHash[:]),
		"csv": map[string]any{
			"name": "billing-lines.csv", "sha256": hex.EncodeToString(csvHash[:]),
			"bytes": len(csvBytes), "rows": 3,
		},
		"providers": sources,
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	store.objects[objectKey] = cloudBillZip(t, manifestBytes, csvBytes)
	return objectKey
}

func TestCloudBillingReadsCompleteBatchAndGroupsSources(t *testing.T) {
	store := newFakeStore()
	cloudBillFixture(t, store)
	page, csvBytes, err := loadCloudBilling(store, "2026-10", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if page.Empty || page.Rows != 3 || len(page.Sources) != 3 || len(page.Groups) != 3 || len(page.Scopes) != 3 {
		t.Fatalf("wrong parsed batch: %+v", page)
	}
	if len(csvBytes) == 0 || page.Groups[0].Amount != "4.690269101" || page.Groups[2].CostBasis != "gcp_shared_view_list_price" {
		t.Fatalf("wrong provider totals: %+v", page.Groups)
	}
	if page.Sources[0].DisplayName != "AWS Bedrock" || page.Sources[0].Breakdowns[0].Comparable != "4.690269101" ||
		page.Sources[2].DisplayName != "Google Vertex AI" {
		t.Fatalf("wrong supplier summaries: %+v", page.Sources)
	}
	if len(page.Candidates) != 1 || page.Candidates[0].Model != "gemini-3-flash" ||
		page.Candidates[0].Status != "仅凭描述推断" || page.Candidates[0].Amount != "0.080287" ||
		page.Sources[2].Breakdowns[0].Unmapped != "0.080287" {
		t.Fatalf("candidate must remain unconfirmed and preserve totals: %+v %+v", page.Candidates, page.Sources[2])
	}
}

func TestCloudBillingAcceptsCompleteOverwriteAfterListing(t *testing.T) {
	store := newFakeStore()
	cloudBillFixture(t, store)
	page, _, err := loadCloudBilling(staleSizeBillStore{store}, "2026-10", time.Now())
	if err != nil || page.Empty || page.Rows != 3 {
		t.Fatalf("complete overwrite rejected: page=%+v err=%v", page, err)
	}
}

func TestCloudBillingSourceBreakdownsKeepCategoriesAndCurrenciesSeparate(t *testing.T) {
	sources := []cloudBillSource{{Provider: "aws"}, {Provider: "gcp"}}
	groups := []cloudBillGroup{
		{Provider: "aws", Currency: "USD", CostBasis: "unblended_usage", Disposition: "comparable", Amount: "4.69"},
		{Provider: "aws", Currency: "USD", CostBasis: "unblended_usage", Disposition: "excluded", Amount: "114.29"},
		{Provider: "aws", Currency: "EUR", CostBasis: "unblended_usage", Disposition: "comparable", Amount: "1"},
		{Provider: "gcp", Currency: "USD", CostBasis: "gcp_shared_view_list_price", Disposition: "unmapped", Amount: "0.080287"},
	}
	cloudBillingSourceBreakdowns(sources, groups)
	if len(sources[0].Breakdowns) != 2 || sources[0].Breakdowns[0].Currency != "EUR" ||
		sources[0].Breakdowns[1].Comparable != "4.69" || sources[0].Breakdowns[1].Excluded != "114.29" {
		t.Fatalf("AWS categories or currencies merged: %+v", sources[0].Breakdowns)
	}
	if len(sources[1].Breakdowns) != 1 || sources[1].Breakdowns[0].Comparable != "0" ||
		sources[1].Breakdowns[0].Unmapped != "0.080287" {
		t.Fatalf("GCP unmapped amount lost: %+v", sources[1].Breakdowns)
	}
}

func TestCloudBillingIgnoresStandaloneCSV(t *testing.T) {
	store := newFakeStore()
	csvKey := cloudBillingPrefix + "2026-10/billing-lines.csv"
	store.objects[csvKey] = []byte("not an archive")
	page, body, err := loadCloudBilling(store, "2026-10", time.Now())
	if err != nil || !page.Empty || body != nil {
		t.Fatalf("orphan CSV was accepted: page=%+v err=%v", page, err)
	}
	if _, ok := store.objects[csvKey]; !ok {
		t.Fatal("test setup lost CSV")
	}
}

func TestCloudBillingRejectsTamperedCSV(t *testing.T) {
	store := newFakeStore()
	key := cloudBillFixture(t, store)
	manifest, csvBytes := cloudBillZipContents(t, store.objects[key])
	content := bytes.Replace(csvBytes, []byte("4.690269101"), []byte("9.690269101"), 1)
	store.objects[key] = cloudBillZip(t, manifest, content)
	if _, _, err := loadCloudBilling(store, "2026-10", time.Now()); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("tampered CSV accepted: %v", err)
	}
}

func TestCloudBillingRejectsIncompleteManifest(t *testing.T) {
	store := newFakeStore()
	key := cloudBillFixture(t, store)
	manifestBytes, csvBytes := cloudBillZipContents(t, store.objects[key])
	var manifest map[string]any
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest["providers"] = manifest["providers"].([]any)[:2]
	manifestBytes, _ = json.Marshal(manifest)
	store.objects[key] = cloudBillZip(t, manifestBytes, csvBytes)
	if _, _, err := loadCloudBilling(store, "2026-10", time.Now()); err == nil || !strings.Contains(err.Error(), "三家") {
		t.Fatalf("incomplete manifest accepted: %v", err)
	}
}

func TestCloudBillingRejectsBrokenMonthlyReplacement(t *testing.T) {
	store := newFakeStore()
	key := cloudBillFixture(t, store)
	store.objects[key] = []byte("broken replacement")
	if _, _, err := loadCloudBilling(store, "", time.Now()); err == nil {
		t.Fatal("broken monthly replacement was accepted")
	}
}

func TestCloudBillingRejectsInvalidMonth(t *testing.T) {
	if _, _, err := loadCloudBilling(newFakeStore(), "../2026-10", time.Now()); err == nil {
		t.Fatal("unsafe month accepted")
	}
}

// Opt-in integration check for a manually published monthly archive. It only reads OSS.
func TestCloudBillingLiveOSS(t *testing.T) {
	month := os.Getenv("RECONCILE_TEST_OSS_MONTH")
	if month == "" {
		t.Skip("set RECONCILE_TEST_OSS_MONTH for a read-only OSS integration check")
	}
	keyID, secret := os.Getenv("AIENVMGR_OSS_ACCESS_KEY_ID"), os.Getenv("AIENVMGR_OSS_ACCESS_KEY_SECRET")
	if keyID == "" || secret == "" {
		t.Fatal("Admin OSS credentials are required for the opt-in integration check")
	}
	objects, err := ossclient.New("oss-ap-southeast-1.aliyuncs.com", "ai-collect-sg", keyID, secret)
	if err != nil {
		t.Fatal(err)
	}
	page, body, err := loadCloudBilling(objects, month, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if page.Empty || len(page.Sources) != 3 || page.Rows == 0 || len(body) == 0 {
		t.Fatalf("incomplete live batch: month=%s rows=%d sources=%d", month, page.Rows, len(page.Sources))
	}
	t.Logf("validated cloud bill month=%s rows=%d sources=%d", month, page.Rows, len(page.Sources))
}
