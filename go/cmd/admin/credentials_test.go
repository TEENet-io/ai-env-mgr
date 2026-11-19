package main

import "testing"

func TestADeploymentsOwnBucketIsUsedWhenNamed(t *testing.T) {
	t.Setenv("AIENVMGR_OSS_BUCKET", "")
	t.Setenv("AIENVMGR_OSS_ENDPOINT", "")
	if b, e := ossTarget(); b != ossBucket || e != ossEndpoint {
		t.Fatalf("unset: got %s %s, want the built-in %s %s", b, e, ossBucket, ossEndpoint)
	}
	t.Setenv("AIENVMGR_OSS_BUCKET", "bippo-workspace-sg")
	t.Setenv("AIENVMGR_OSS_ENDPOINT", "oss-ap-southeast-1.aliyuncs.com")
	if b, e := ossTarget(); b != "bippo-workspace-sg" || e != "oss-ap-southeast-1.aliyuncs.com" {
		t.Fatalf("set: got %s %s", b, e)
	}
}
