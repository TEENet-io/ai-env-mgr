package dbstore

import (
	"testing"
	"time"
)

func TestAdminJobsPersistProgressAndRecover(t *testing.T) {
	s, ctx := newTestStore(t)
	jobs := s.AdminJobs()
	created, err := jobs.Create(ctx, "admin-test-1", "agent", "1.5.10")
	if err != nil {
		t.Fatal(err)
	}
	if created.State != "running" || created.Kind != "agent" {
		t.Fatalf("created job = %+v", created)
	}
	if err := jobs.Update(ctx, created.ID, "running", "上传到 OSS", 10, 20, "", nil); err != nil {
		t.Fatal(err)
	}
	latest, err := jobs.Latest(ctx)
	if err != nil || latest.Step != "上传到 OSS" || latest.DoneBytes != 10 || latest.TotalBytes != 20 {
		t.Fatalf("latest = %+v, err=%v", latest, err)
	}
	if n, err := jobs.RecoverRunning(ctx, "console restarted"); err != nil || n != 1 {
		t.Fatalf("recover = %d, err=%v", n, err)
	}
	latest, err = jobs.Latest(ctx)
	if err != nil || latest.State != "failed" || latest.LastError != "console restarted" || latest.EndedAt == nil {
		t.Fatalf("recovered = %+v, err=%v", latest, err)
	}
	ended := time.Now().UTC()
	if err := jobs.Update(ctx, created.ID, "done", "完成", 20, 20, "", &ended); err != nil {
		t.Fatal(err)
	}
	if list, err := jobs.ListRecent(ctx, 10); err != nil || len(list) != 1 || list[0].State != "done" {
		t.Fatalf("list = %+v, err=%v", list, err)
	}
}
