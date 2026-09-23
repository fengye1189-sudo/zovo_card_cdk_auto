package handler

import (
 "net/http/httptest"
 "testing"
 "time"
 "github.com/gin-gonic/gin"
 "github.com/tuzi/cdk-recharge-system/internal/auth"
)

func TestEmailEnforcementRejectsPasswordAndOldSessions(t *testing.T) {
 teamFixture(t)
 t.Setenv("MAPLE_ADMIN_EMAIL_REQUIRED", "1")
 router := gin.New()
 router.POST("/login", AdminLogin)
 response := httptest.NewRecorder()
 router.ServeHTTP(response, httptest.NewRequest("POST", "/login", nil))
 if response.Code != 403 { t.Fatal("password endpoint bypassed email gate", response.Code) }
 claims := &auth.CustomClaims{UserID:1,Username:"original_owner",IsAdmin:true}
 if _, err := auth.ResolveAdminSession(claims); err == nil { t.Fatal("old session survived email enforcement") }
 claims.EmailVerifiedUntil = time.Now().Add(-time.Second).Unix()
 if _, err := auth.ResolveAdminSession(claims); err == nil { t.Fatal("expired proof accepted") }
 claims.EmailVerifiedUntil = time.Now().Add(time.Hour).Unix()
 if _, err := auth.ResolveAdminSession(claims); err != nil { t.Fatal("verified existing owner rejected",err) }
}

func TestEmailSessionCannotOutliveVerification(t *testing.T) {
 teamFixture(t)
 t.Setenv("MAPLE_ADMIN_EMAIL_REQUIRED", "1")
 t.Setenv("JWT_SECRET", "test-email-signing-key-not-production-12345")
 c,_ := gin.CreateTestContext(httptest.NewRecorder())
 c.Request=httptest.NewRequest("GET","/",nil)
 if _,err := issueAdminSession(c,1,"original_owner","Owner"); err==nil {t.Fatal("unverified session issued")}
 until := time.Now().Add(time.Hour).Unix()
 c.Set("admin_email_verified_until",until)
 session,err := issueAdminSession(c,1,"original_owner","Owner")
 if err!=nil {t.Fatal(err)}
 expiration,err:=time.Parse(time.RFC3339,session["expires_at"].(string))
 if err!=nil || expiration.Unix()!=until {t.Fatal("session exceeds email verification",err)}
}
