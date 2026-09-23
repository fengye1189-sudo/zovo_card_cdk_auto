package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tuzi/cdk-recharge-system/internal/db"
)

func TestMarketplaceFailureBridgeSignsPurposeBoundBody(t *testing.T) {
	secret := "0123456789abcdef0123456789abcdef"
	var received marketplaceFailureEvent
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		stamp := r.Header.Get("X-MaplePass-Time")
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte("cdk-failure:v1:" + stamp + ":" + string(body)))
		if r.Header.Get("X-MaplePass-Signature") != hex.EncodeToString(mac.Sum(nil)) {
			t.Fatal("invalid signature")
		}
		if json.Unmarshal(body, &received) != nil {
			t.Fatal("invalid body")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("CDK_SSO_SHARED_SECRET", secret)
	t.Setenv("MAPLE_STORE_FAILURE_WEBHOOK_URL", server.URL)
	event := marketplaceFailureEvent{Version: 1, EventID: "local:41:failed_precharge", LocalID: 41, CodePrefix: "MAPLE-ABCD", Product: "Plus", UpstreamOrder: 99, UpstreamStatus: "failed_precharge", Permanent: true}
	if err := postMarketplaceFailure(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if received != event {
		t.Fatalf("unexpected event: %#v", received)
	}
}

func TestMarketplaceFailureClaimIsSingleAndLeaseRecovers(t *testing.T) {
	newLocalFixture(t)
	if !claimMarketplaceFailure("order:41", 41, "local:41:failed") {
		t.Fatal("first event claim should succeed")
	}
	if claimMarketplaceFailure("order:41", 41, "local:41:failed") {
		t.Fatal("same event must not be claimed twice")
	}
	_, err := db.DB.Exec("UPDATE automation_alerts SET updated_at=? WHERE alert_key=?", time.Now().Unix()-301, "order:41")
	if err != nil {
		t.Fatal(err)
	}
	if !claimMarketplaceFailure("order:41", 41, "local:41:failed") {
		t.Fatal("expired claim should be recoverable")
	}
}
