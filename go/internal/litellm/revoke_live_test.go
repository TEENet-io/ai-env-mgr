package litellm

import (
	"context"
	"os"
	"testing"
)

// TestRevokeByAliasAgainstRealGateway exercises the exact path that failed in
// production: find a listed token (hash only), revoke it by alias, confirm it
// is gone. Opt-in via GW_URL / GW_ADMIN_KEY; uses a throwaway alias.
func TestRevokeByAliasAgainstRealGateway(t *testing.T) {
	base, key := os.Getenv("GW_URL"), os.Getenv("GW_ADMIN_KEY")
	if base == "" || key == "" {
		t.Skip("set GW_URL and GW_ADMIN_KEY to run against a live gateway")
	}
	c, ctx := New(base, key), context.Background()
	const alias = "emp-zz-probe-revoke"

	_ = c.DeleteKeyByAlias(ctx, alias) // clean slate; 404 here is fine
	if _, err := c.GenerateKey(ctx, alias, []string{"grok-4.6"}, 0.01, nil); err != nil {
		t.Fatalf("generate: %v", err)
	}
	listed, found, err := c.FindKeyByAlias(ctx, alias)
	if err != nil || !found {
		t.Fatalf("listed token not found: found=%v err=%v", found, err)
	}
	if listed.Key != "" || listed.Token == "" {
		t.Fatalf("listing should carry hash only: %+v", listed)
	}
	if err := c.DeleteKeyByAlias(ctx, alias); err != nil {
		t.Fatalf("revoke by alias: %v", err)
	}
	if _, found, _ := c.FindKeyByAlias(ctx, alias); found {
		t.Fatal("token still listed after revocation")
	}
	t.Logf("listed as hash %s…, revoked by alias, gone", listed.Token[:8])
}
