package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExtractTokenFromExactKey(t *testing.T) {
	t.Parallel()

	payload := map[string]any{"openai.access": "token-from-exact-key"}
	token, err := extractToken(payload)
	if err != nil {
		t.Fatalf("extractToken returned error: %v", err)
	}

	if token != "token-from-exact-key" {
		t.Fatalf("unexpected token: %q", token)
	}
}

func TestExtractTokenFallbackNested(t *testing.T) {
	t.Parallel()

	payload := map[string]any{"openai": map[string]any{"access": "nested-token"}}
	token, err := extractToken(payload)
	if err != nil {
		t.Fatalf("extractToken returned error: %v", err)
	}

	if token != "nested-token" {
		t.Fatalf("unexpected token: %q", token)
	}
}

func TestExtractTokenPrefersExactKey(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"openai.access": "exact-token",
		"openai":        map[string]any{"access": "nested-token"},
	}
	token, err := extractToken(payload)
	if err != nil {
		t.Fatalf("extractToken returned error: %v", err)
	}

	if token != "exact-token" {
		t.Fatalf("expected exact token, got %q", token)
	}
}

func TestRunOutputsUsedPercentOnly(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("unexpected Authorization header: %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"user_id":"user-1","rate_limit":{"primary_window":{"used_percent":73.25}}}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"openai.access":"test-token","openai.accountId":"acct-1"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	accountsFile := filepath.Join(dir, "accounts.json")

	var out strings.Builder
	err := run(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		UsageURL:     server.URL,
		HTTPClient:   server.Client(),
	}, &out)
	if err != nil {
		t.Fatalf("run returned error: %v", err)
	}

	if out.String() != "73.25" {
		t.Fatalf("expected only used_percent value, got %q", out.String())
	}

	storedData, err := os.ReadFile(accountsFile)
	if err != nil {
		t.Fatalf("read accounts file: %v", err)
	}

	var store map[string]map[string]any
	if err := json.Unmarshal(storedData, &store); err != nil {
		t.Fatalf("parse accounts file JSON: %v", err)
	}

	acct, ok := store["user-1"]
	if !ok {
		t.Fatalf("expected user user-1 to be stored")
	}

	if got, _ := acct["user_id"].(string); got != "user-1" {
		t.Fatalf("unexpected stored user_id: %q", got)
	}

	if got, _ := acct["access"].(string); got != "test-token" {
		t.Fatalf("unexpected stored access: %q", got)
	}

	if got, _ := acct["usedPercent"].(string); got != "73.25" {
		t.Fatalf("expected usedPercent to be stored, got %v", acct["usedPercent"])
	}

	if _, exists := acct["cooldownUntil"]; exists {
		t.Fatalf("did not expect cooldownUntil under threshold")
	}
}

func TestRunRegistersCurrentAccountWhenMissingFromStore(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"user_id":"user-new","rate_limit":{"primary_window":{"used_percent":42.5}}}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"openai.access":"new-token","openai.accountId":"acct-new","openai.refresh":"new-refresh"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	accountsFile := filepath.Join(dir, "accounts.json")
	initialStore := map[string]map[string]any{
		"user-existing": {
			"user_id":   "user-existing",
			"accountId": "acct-existing",
			"access":    "existing-token",
		},
	}
	encoded, err := json.Marshal(initialStore)
	if err != nil {
		t.Fatalf("marshal initial store: %v", err)
	}
	if err := os.WriteFile(accountsFile, encoded, 0o600); err != nil {
		t.Fatalf("write accounts file: %v", err)
	}

	var out strings.Builder
	err = run(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		UsageURL:     server.URL,
		HTTPClient:   server.Client(),
	}, &out)
	if err != nil {
		t.Fatalf("run returned error: %v", err)
	}

	if out.String() != "42.5" {
		t.Fatalf("expected only used_percent output, got %q", out.String())
	}

	store := mustReadStore(t, accountsFile)
	if len(store) != 2 {
		t.Fatalf("expected 2 accounts after registration, got %d", len(store))
	}

	acct, ok := store["user-new"]
	if !ok {
		t.Fatalf("expected current user user-new to be registered")
	}

	if got, _ := acct["accountId"].(string); got != "acct-new" {
		t.Fatalf("expected accountId metadata acct-new, got %q", got)
	}

	if got, _ := acct["access"].(string); got != "new-token" {
		t.Fatalf("expected current access token to be stored, got %q", got)
	}

	if got, _ := acct["refresh"].(string); got != "new-refresh" {
		t.Fatalf("expected openai.refresh field to be preserved, got %q", got)
	}
}

func TestRunDoesNotDuplicateExistingAccountAcrossRepeatedChecks(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"user_id":"user-stable","rate_limit":{"primary_window":{"used_percent":44}}}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"openai.access":"stable-token","openai.accountId":"acct-stable"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	accountsFile := filepath.Join(dir, "accounts.json")

	for i := 0; i < 2; i++ {
		var out strings.Builder
		err := run(context.Background(), config{
			AuthFile:     authFile,
			AccountsFile: accountsFile,
			UsageURL:     server.URL,
			HTTPClient:   server.Client(),
		}, &out)
		if err != nil {
			t.Fatalf("run #%d returned error: %v", i+1, err)
		}

		if out.String() != "44" {
			t.Fatalf("run #%d expected only used_percent output, got %q", i+1, out.String())
		}
	}

	store := mustReadStore(t, accountsFile)
	if len(store) != 1 {
		t.Fatalf("expected exactly one stored account after repeated checks, got %d", len(store))
	}

	acct, ok := store["user-stable"]
	if !ok {
		t.Fatalf("expected user-stable in store")
	}

	if got, _ := acct["access"].(string); got != "stable-token" {
		t.Fatalf("expected stored access token stable-token, got %q", got)
	}

	if got, _ := acct["usedPercent"].(string); got != "44" {
		t.Fatalf("expected usedPercent to be refreshed, got %v", acct["usedPercent"])
	}
}

func TestRunReturnsErrorOnNon200(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("unauthorized"))
	}))
	defer server.Close()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"openai.access":"test-token","openai.accountId":"acct-2"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	accountsFile := filepath.Join(dir, "accounts.json")

	err := run(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		UsageURL:     server.URL,
		HTTPClient:   server.Client(),
	}, &strings.Builder{})
	if err == nil {
		t.Fatalf("expected error for non-200 response")
	}

	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected status code in error, got: %v", err)
	}
}

// TestFetchUsageWindowRedactsReflectedBearerToken: a reflected token in the
// response body must never appear in the returned error; non-secret text is kept.
func TestFetchUsageWindowRedactsReflectedBearerToken(t *testing.T) {
	t.Parallel()
	const token = "reflected-bearer-token-do-not-leak"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("unexpected Authorization header: %q", got)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, "auth failed for %s please retry", token)
	}))
	defer server.Close()
	_ = t.TempDir()
	_, err := fetchUsageWindow(context.Background(), server.Client(), server.URL, token)
	if err == nil {
		t.Fatalf("expected error from non-200 response")
	}
	msg := err.Error()
	if strings.Contains(msg, token) {
		t.Fatalf("error leaked bearer token %q: %v", token, err)
	}
	if !strings.Contains(msg, "[REDACTED]") || !strings.Contains(msg, "please retry") {
		t.Fatalf("expected [REDACTED] placeholder and surviving response text, got: %v", err)
	}
}

func TestExtractAccountIDFromExactKey(t *testing.T) {
	t.Parallel()

	payload := map[string]any{"openai.accountId": "account-from-exact-key"}
	accountID, ok := extractAccountID(payload)
	if !ok {
		t.Fatalf("extractAccountID did not extract accountId")
	}

	if accountID != "account-from-exact-key" {
		t.Fatalf("unexpected account ID: %q", accountID)
	}
}

func TestExtractAccountIDFallbackNested(t *testing.T) {
	t.Parallel()

	payload := map[string]any{"openai": map[string]any{"accountId": "nested-account"}}
	accountID, ok := extractAccountID(payload)
	if !ok {
		t.Fatalf("extractAccountID did not extract accountId")
	}

	if accountID != "nested-account" {
		t.Fatalf("unexpected account ID: %q", accountID)
	}
}

func TestExtractAccountIDPrefersExactKey(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"openai.accountId": "exact-account",
		"openai":           map[string]any{"accountId": "nested-account"},
	}
	accountID, ok := extractAccountID(payload)
	if !ok {
		t.Fatalf("extractAccountID did not extract accountId")
	}

	if accountID != "exact-account" {
		t.Fatalf("expected exact account ID, got %q", accountID)
	}
}

func TestPersistOpenAIAccountIsIdempotentByUserID(t *testing.T) {
	t.Parallel()

	accountsFile := filepath.Join(t.TempDir(), "openai-accounts.json")

	if err := persistOpenAIAccount(accountsFile, map[string]any{
		"user_id":   "user-1",
		"accountId": "acct-1",
		"access":    "token-v1",
		"refresh":   "refresh-v1",
	}); err != nil {
		t.Fatalf("first persistOpenAIAccount call failed: %v", err)
	}

	if err := persistOpenAIAccount(accountsFile, map[string]any{
		"user_id":   "user-1",
		"accountId": "acct-1",
		"access":    "token-v2",
		"refresh":   "refresh-v2",
	}); err != nil {
		t.Fatalf("second persistOpenAIAccount call failed: %v", err)
	}

	data, err := os.ReadFile(accountsFile)
	if err != nil {
		t.Fatalf("read accounts file: %v", err)
	}

	var store map[string]map[string]any
	if err := json.Unmarshal(data, &store); err != nil {
		t.Fatalf("parse accounts file JSON: %v", err)
	}

	if len(store) != 1 {
		t.Fatalf("expected exactly one account entry, got %d", len(store))
	}

	acct, ok := store["user-1"]
	if !ok {
		t.Fatalf("expected user-1 in store")
	}

	if got, _ := acct["access"].(string); got != "token-v2" {
		t.Fatalf("expected access token to be updated, got %q", got)
	}

	if got, _ := acct["refresh"].(string); got != "refresh-v2" {
		t.Fatalf("expected refresh token to be updated, got %q", got)
	}
}

func TestExtractUsageWindowParsesResetAt(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"user_id": "user-window",
		"rate_limit": map[string]any{
			"primary_window": map[string]any{
				"used_percent": json.Number("81.5"),
				"reset_at":     json.Number("1777014899"),
			},
		},
	}

	window, err := extractUsageWindow(payload)
	if err != nil {
		t.Fatalf("extractUsageWindow returned error: %v", err)
	}

	if window.UsedPercent != "81.5" {
		t.Fatalf("unexpected usedPercent: %q", window.UsedPercent)
	}

	if window.ResetAt == nil || *window.ResetAt != 1777014899 {
		t.Fatalf("expected reset_at 1777014899, got %v", window.ResetAt)
	}

	if window.UserID != "user-window" {
		t.Fatalf("expected user_id user-window, got %q", window.UserID)
	}
}

func TestExtractUsageWindowParsesEmail(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"user_id": "user-email",
		"email":   "user-email@example.com",
		"rate_limit": map[string]any{
			"primary_window": map[string]any{
				"used_percent": json.Number("15.25"),
			},
		},
	}

	window, err := extractUsageWindow(payload)
	if err != nil {
		t.Fatalf("extractUsageWindow returned error: %v", err)
	}

	if window.Email != "user-email@example.com" {
		t.Fatalf("expected parsed email, got %q", window.Email)
	}
}

func TestExtractUsageWindowReturnsErrorWhenUserIDMissing(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"rate_limit": map[string]any{
			"primary_window": map[string]any{
				"used_percent": json.Number("22.5"),
			},
		},
	}

	_, err := extractUsageWindow(payload)
	if err == nil {
		t.Fatalf("expected missing user_id error")
	}

	if !strings.Contains(err.Error(), "user_id") {
		t.Fatalf("expected error to mention user_id, got %v", err)
	}
}

func TestPersistOpenAIAccountNormalizesEntryWithUserIDButLegacyKey(t *testing.T) {
	t.Parallel()

	accountsFile := filepath.Join(t.TempDir(), "openai-accounts.json")
	initial := map[string]map[string]any{
		"acct-legacy": {
			"user_id":   "user-legacy",
			"accountId": "acct-legacy",
			"access":    "legacy-token",
		},
	}

	encoded, err := json.Marshal(initial)
	if err != nil {
		t.Fatalf("marshal initial store: %v", err)
	}
	if err := os.WriteFile(accountsFile, encoded, 0o600); err != nil {
		t.Fatalf("write initial store: %v", err)
	}

	if err := persistOpenAIAccount(accountsFile, map[string]any{
		"user_id":   "user-legacy",
		"accountId": "acct-legacy",
		"access":    "updated-token",
	}); err != nil {
		t.Fatalf("persistOpenAIAccount failed: %v", err)
	}

	store := mustReadStore(t, accountsFile)
	if len(store) != 1 {
		t.Fatalf("expected one normalized entry, got %d", len(store))
	}

	if _, hasLegacy := store["acct-legacy"]; hasLegacy {
		t.Fatalf("expected legacy key acct-legacy to be normalized away")
	}

	entry, ok := store["user-legacy"]
	if !ok {
		t.Fatalf("expected normalized key user-legacy")
	}

	if got, _ := entry["access"].(string); got != "updated-token" {
		t.Fatalf("expected merged access updated-token, got %q", got)
	}
}

func TestRunPersistsCooldownWhenThresholdReached(t *testing.T) {
	t.Parallel()

	resetAt := int64(1777014899)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"user_id":"user-current","rate_limit":{"primary_window":{"used_percent":98,"reset_at":` + strconv.FormatInt(resetAt, 10) + `}}}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth.json")
	originalAuth := `{"openai.access":"current-token","openai.accountId":"acct-current"}`
	if err := os.WriteFile(authFile, []byte(originalAuth), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	accountsFile := filepath.Join(dir, "accounts.json")

	var out strings.Builder
	err := run(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		UsageURL:     server.URL,
		HTTPClient:   server.Client(),
	}, &out)
	if err != nil {
		t.Fatalf("run returned error: %v", err)
	}

	if out.String() != "98" {
		t.Fatalf("expected only used_percent value, got %q", out.String())
	}

	store := mustReadStore(t, accountsFile)
	acct := store["user-current"]
	if got, _ := acct["usedPercent"].(string); got != "98" {
		t.Fatalf("expected usedPercent 98, got %v", acct["usedPercent"])
	}

	if got, ok := valueToInt64(acct["resetAt"]); !ok || got != resetAt {
		t.Fatalf("expected resetAt %d, got %v", resetAt, acct["resetAt"])
	}

	if got, ok := valueToInt64(acct["cooldownUntil"]); !ok || got != resetAt {
		t.Fatalf("expected cooldownUntil %d, got %v", resetAt, acct["cooldownUntil"])
	}

	authAfter, err := os.ReadFile(authFile)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	if string(authAfter) != originalAuth {
		t.Fatalf("expected auth file unchanged without alternate account, got %s", string(authAfter))
	}
}

func TestSelectEligibleAlternateAccountSkipsCoolingDown(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()
	store := map[string]map[string]any{
		"user-current": {
			"user_id":   "user-current",
			"accountId": "acct-current",
			"access":    "current-token",
		},
		"user-hot": {
			"user_id":       "user-hot",
			"accountId":     "acct-hot",
			"access":        "hot-token",
			"cooldownUntil": now + 3600,
		},
		"user-ready": {
			"user_id":       "user-ready",
			"accountId":     "acct-ready",
			"access":        "ready-token",
			"cooldownUntil": now - 10,
		},
	}

	selected, ok := selectEligibleAlternateAccount(store, "user-current", now, false, defaultFiveHourThreshold, defaultWeeklyThreshold)
	if !ok {
		t.Fatalf("expected an eligible alternate account")
	}

	if got, _ := selected["user_id"].(string); got != "user-ready" {
		t.Fatalf("expected user-ready, got %q", got)
	}
}

func TestUpdateOpenAIAuthFileUpdatesFlatAndNestedFields(t *testing.T) {
	t.Parallel()

	authFile := filepath.Join(t.TempDir(), "auth.json")
	content := `{"openai.access":"old-flat","openai.accountId":"old-flat-id","openai":{"access":"old-nested","accountId":"old-nested-id"},"other":true}`
	if err := os.WriteFile(authFile, []byte(content), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	err := updateOpenAIAuthFile(authFile, map[string]any{
		"accountId": "acct-next",
		"access":    "next-token",
	})
	if err != nil {
		t.Fatalf("updateOpenAIAuthFile returned error: %v", err)
	}

	data, err := os.ReadFile(authFile)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("parse auth JSON: %v", err)
	}

	if got, _ := payload["openai.access"].(string); got != "next-token" {
		t.Fatalf("expected updated openai.access, got %q", got)
	}
	if got, _ := payload["openai.accountId"].(string); got != "acct-next" {
		t.Fatalf("expected updated openai.accountId, got %q", got)
	}

	openAI, ok := payload["openai"].(map[string]any)
	if !ok {
		t.Fatalf("expected nested openai object")
	}
	if got, _ := openAI["access"].(string); got != "next-token" {
		t.Fatalf("expected updated nested access, got %q", got)
	}
	if got, _ := openAI["accountId"].(string); got != "acct-next" {
		t.Fatalf("expected updated nested accountId, got %q", got)
	}
}

func TestUpdateOpenAIAuthFileClearsStaleAccountIDWhenTargetHasNone(t *testing.T) {
	t.Parallel()

	authFile := filepath.Join(t.TempDir(), "auth.json")
	content := `{"openai.access":"old-flat","openai.accountId":"old-flat-id","openai":{"access":"old-nested","accountId":"old-nested-id"},"other":true}`
	if err := os.WriteFile(authFile, []byte(content), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	// Target account intentionally has no `accountId` field.
	if err := updateOpenAIAuthFile(authFile, map[string]any{
		"access": "new-token",
	}); err != nil {
		t.Fatalf("updateOpenAIAuthFile returned error: %v", err)
	}

	data, err := os.ReadFile(authFile)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("parse auth JSON: %v", err)
	}

	if got, _ := payload["openai.access"].(string); got != "new-token" {
		t.Fatalf("expected updated openai.access, got %q", got)
	}
	if _, exists := payload["openai.accountId"]; exists {
		t.Fatalf("expected stale openai.accountId to be removed, payload=%v", payload)
	}

	openAI, ok := payload["openai"].(map[string]any)
	if !ok {
		t.Fatalf("expected nested openai object")
	}
	if got, _ := openAI["access"].(string); got != "new-token" {
		t.Fatalf("expected updated nested access, got %q", got)
	}
	if _, exists := openAI["accountId"]; exists {
		t.Fatalf("expected stale nested accountId to be removed, openai=%v", openAI)
	}

	// Unrelated top-level fields must be preserved.
	if got, _ := payload["other"].(bool); !got {
		t.Fatalf("expected unrelated top-level field 'other' to be preserved, got %v", payload["other"])
	}
}

func TestRunRotatesToEligibleAlternateAccount(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()
	// Current weekly window exhausted (>= 98) so rotation is required.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"user_id":"user-current","rate_limit":{"primary_window":{"used_percent":99,"reset_at":` + strconv.FormatInt(now+7200, 10) + `}}}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"openai.access":"current-token","openai.accountId":"acct-current"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	accountsFile := filepath.Join(dir, "accounts.json")
	initialStore := map[string]map[string]any{
		"user-current": {
			"user_id":   "user-current",
			"accountId": "acct-current",
			"access":    "current-token",
		},
		"user-cooling": {
			"user_id":       "user-cooling",
			"accountId":     "acct-cooling",
			"access":        "cooling-token",
			"cooldownUntil": now + 3600,
		},
		"user-ready": {
			"user_id":       "user-ready",
			"accountId":     "acct-ready",
			"access":        "ready-token",
			"cooldownUntil": now - 60,
		},
	}
	encoded, err := json.Marshal(initialStore)
	if err != nil {
		t.Fatalf("marshal store: %v", err)
	}
	if err := os.WriteFile(accountsFile, encoded, 0o600); err != nil {
		t.Fatalf("write accounts file: %v", err)
	}

	var out strings.Builder
	err = run(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		UsageURL:     server.URL,
		HTTPClient:   server.Client(),
	}, &out)
	if err != nil {
		t.Fatalf("run returned error: %v", err)
	}

	if out.String() != "99" {
		t.Fatalf("expected only used_percent output, got %q", out.String())
	}

	data, err := os.ReadFile(authFile)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}

	var authPayload map[string]any
	if err := json.Unmarshal(data, &authPayload); err != nil {
		t.Fatalf("parse auth JSON: %v", err)
	}

	if got, _ := authPayload["openai.accountId"].(string); got != "acct-ready" {
		t.Fatalf("expected rotation to acct-ready, got %q", got)
	}
	if got, _ := authPayload["openai.access"].(string); got != "ready-token" {
		t.Fatalf("expected token from acct-ready, got %q", got)
	}
}

func TestRunRotatesAndRegistersCurrentAccountWhenMissingFromStore(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()
	resetAt := now + 3600
	// Current weekly window exhausted (>= 98) so rotation is required.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"user_id":"user-current","rate_limit":{"primary_window":{"used_percent":99,"reset_at":` + strconv.FormatInt(resetAt, 10) + `}}}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"openai.access":"current-token","openai.accountId":"acct-current"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	accountsFile := filepath.Join(dir, "accounts.json")
	initialStore := map[string]map[string]any{
		"user-ready": {
			"user_id":       "user-ready",
			"accountId":     "acct-ready",
			"access":        "ready-token",
			"cooldownUntil": now - 1,
		},
	}
	encoded, err := json.Marshal(initialStore)
	if err != nil {
		t.Fatalf("marshal initial store: %v", err)
	}
	if err := os.WriteFile(accountsFile, encoded, 0o600); err != nil {
		t.Fatalf("write accounts file: %v", err)
	}

	var out strings.Builder
	err = run(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		UsageURL:     server.URL,
		HTTPClient:   server.Client(),
	}, &out)
	if err != nil {
		t.Fatalf("run returned error: %v", err)
	}

	if out.String() != "99" {
		t.Fatalf("expected only used_percent output, got %q", out.String())
	}

	store := mustReadStore(t, accountsFile)
	if len(store) != 2 {
		t.Fatalf("expected current account to be added before rotation, got %d accounts", len(store))
	}

	current, ok := store["user-current"]
	if !ok {
		t.Fatalf("expected user-current to be registered in store")
	}

	if got, _ := current["access"].(string); got != "current-token" {
		t.Fatalf("expected current token to be stored, got %q", got)
	}

	if got, ok := valueToInt64(current["cooldownUntil"]); !ok || got != resetAt {
		t.Fatalf("expected current cooldownUntil %d, got %v", resetAt, current["cooldownUntil"])
	}

	data, err := os.ReadFile(authFile)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}

	var authPayload map[string]any
	if err := json.Unmarshal(data, &authPayload); err != nil {
		t.Fatalf("parse auth JSON: %v", err)
	}

	if got, _ := authPayload["openai.accountId"].(string); got != "acct-ready" {
		t.Fatalf("expected rotation to acct-ready, got %q", got)
	}
	if got, _ := authPayload["openai.access"].(string); got != "ready-token" {
		t.Fatalf("expected token from acct-ready, got %q", got)
	}
}

func TestRunNoAlternateAccountLeavesAuthUnchanged(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"user_id":"user-current","rate_limit":{"primary_window":{"used_percent":99,"reset_at":` + strconv.FormatInt(now+7200, 10) + `}}}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth.json")
	original := `{"openai.access":"current-token","openai.accountId":"acct-current"}`
	if err := os.WriteFile(authFile, []byte(original), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	accountsFile := filepath.Join(dir, "accounts.json")
	store := map[string]map[string]any{
		"user-current": {
			"user_id":   "user-current",
			"accountId": "acct-current",
			"access":    "current-token",
		},
		"user-other": {
			"user_id":       "user-other",
			"accountId":     "acct-other",
			"access":        "other-token",
			"cooldownUntil": now + 7200,
		},
	}
	encoded, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("marshal store: %v", err)
	}
	if err := os.WriteFile(accountsFile, encoded, 0o600); err != nil {
		t.Fatalf("write accounts file: %v", err)
	}

	var out strings.Builder
	err = run(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		UsageURL:     server.URL,
		HTTPClient:   server.Client(),
	}, &out)
	if err != nil {
		t.Fatalf("run returned error: %v", err)
	}

	if out.String() != "99" {
		t.Fatalf("expected only used_percent output, got %q", out.String())
	}

	authAfter, err := os.ReadFile(authFile)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}

	if string(authAfter) != original {
		t.Fatalf("expected auth unchanged with no eligible alternate, got %s", string(authAfter))
	}
}

func TestRunWithArgsAccountsListsUsageHighlightsCurrentAndPersistsByUserID(t *testing.T) {
	t.Parallel()

	fixedNow := time.Unix(1_700_000_000, 0)
	currentReset := fixedNow.Unix() + (2 * 60 * 60) + (15 * 60)
	otherReset := fixedNow.Unix() + (24 * 60 * 60) + (3 * 60 * 60)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch auth {
		case "Bearer current-token":
			w.WriteHeader(http.StatusOK)
			// Current endpoint: primary_window IS the weekly window;
			// secondary_window is null and ignored entirely.
			_, _ = w.Write([]byte(`{"user_id":"user-current","email":"current@example.com","rate_limit":{"primary_window":{"used_percent":22.5,"reset_at":` + strconv.FormatInt(currentReset, 10) + `},"secondary_window":null}}`))
		case "Bearer other-token":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"user_id":"user-other","email":"other@example.com","rate_limit":{"primary_window":{"used_percent":88,"reset_at":` + strconv.FormatInt(otherReset, 10) + `}}}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("unauthorized"))
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"openai.access":"current-token","openai.accountId":"acct-current"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	accountsFile := filepath.Join(dir, "accounts.json")
	initialStore := map[string]map[string]any{
		"legacy-current": {
			"user_id": "user-current",
			"access":  "current-token",
		},
		"user-other": {
			"user_id": "user-other",
			"access":  "other-token",
		},
	}
	encoded, err := json.Marshal(initialStore)
	if err != nil {
		t.Fatalf("marshal initial store: %v", err)
	}
	if err := os.WriteFile(accountsFile, encoded, 0o600); err != nil {
		t.Fatalf("write accounts file: %v", err)
	}

	var out strings.Builder
	err = runWithArgs(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		UsageURL:     server.URL,
		HTTPClient:   server.Client(),
		Now:          func() time.Time { return fixedNow },
	}, &out, []string{"accounts"})
	if err != nil {
		t.Fatalf("runWithArgs returned error: %v", err)
	}

	printed := out.String()
	if !strings.Contains(printed, "| ID |") || !strings.Contains(printed, "CURRENT") ||
		!strings.Contains(printed, "EMAIL") || !strings.Contains(printed, "WEEK%") ||
		!strings.Contains(printed, "WEEK-RESET") {
		t.Fatalf("expected Markdown-style table header with WEEK columns, got %q", printed)
	}
	// Old USED%/RESET pair was the 5-hour window and must no longer appear
	// as a separate column: primary_window is now the weekly window.
	if strings.Contains(printed, "| USED% |") || strings.Contains(printed, "| RESET |") {
		t.Fatalf("expected no separate USED%%/RESET columns (primary is now weekly), got %q", printed)
	}
	// Header separator must use `-` characters and `|` boundaries.
	if !strings.Contains(printed, "| -- |") && !strings.Contains(printed, "|---") {
		t.Fatalf("expected Markdown-style table separator with `-`, got %q", printed)
	}
	if !strings.Contains(printed, "current@example.com") {
		t.Fatalf("expected current account email in output, got %q", printed)
	}
	if !strings.Contains(printed, "other@example.com") {
		t.Fatalf("expected other account email in output, got %q", printed)
	}
	if !strings.Contains(printed, "22.5") || !strings.Contains(printed, "88") {
		t.Fatalf("expected weekly used_percent values in output, got %q", printed)
	}
	if !strings.Contains(printed, "2h 15m") {
		t.Fatalf("expected remaining duration 2h 15m in output, got %q", printed)
	}
	if !strings.Contains(printed, "1d 3h") {
		t.Fatalf("expected remaining duration 1d 3h in output, got %q", printed)
	}
	if strings.Contains(printed, strconv.FormatInt(currentReset, 10)) || strings.Contains(printed, strconv.FormatInt(otherReset, 10)) {
		t.Fatalf("did not expect reset unix timestamps in output, got %q", printed)
	}
	if strings.Contains(printed, "202") {
		t.Fatalf("did not expect formatted date/time in output, got %q", printed)
	}

	currentLine := strings.TrimSpace(lineContaining(printed, "current@example.com"))
	if !strings.Contains(currentLine, "*") {
		t.Fatalf("expected current account line to be highlighted with '*', got %q", currentLine)
	}
	otherLine := strings.TrimSpace(lineContaining(printed, "other@example.com"))
	if strings.Contains(otherLine, "*") {
		t.Fatalf("expected non-current account line to NOT contain '*', got %q", otherLine)
	}

	// Index column should match the stable alphabetical order of user_id.
	currentIndexLine := strings.TrimSpace(lineContaining(printed, "current@example.com"))
	otherIndexLine := strings.TrimSpace(lineContaining(printed, "other@example.com"))
	if !strings.HasPrefix(currentIndexLine, "| 1 ") {
		t.Fatalf("expected current account to be listed first (index 1), got %q", currentIndexLine)
	}
	if !strings.HasPrefix(otherIndexLine, "| 2 ") {
		t.Fatalf("expected other account to be listed second (index 2), got %q", otherIndexLine)
	}

	store := mustReadStore(t, accountsFile)
	if _, hasLegacy := store["legacy-current"]; hasLegacy {
		t.Fatalf("expected legacy key to be normalized away")
	}

	currentEntry, ok := store["user-current"]
	if !ok {
		t.Fatalf("expected user-current key in persisted store")
	}
	if got, _ := currentEntry["email"].(string); got != "current@example.com" {
		t.Fatalf("expected persisted email current@example.com, got %q", got)
	}
	if got, _ := currentEntry["usedPercent"].(string); got != "22.5" {
		t.Fatalf("expected persisted usedPercent 22.5, got %v", currentEntry["usedPercent"])
	}
	if got, ok := valueToInt64(currentEntry["resetAt"]); !ok || got != currentReset {
		t.Fatalf("expected persisted resetAt %d, got %v", currentReset, currentEntry["resetAt"])
	}
	if _, hasSecondary := currentEntry["secondaryUsedPercent"]; hasSecondary {
		t.Fatalf("expected secondaryUsedPercent to be stripped, got %v", currentEntry["secondaryUsedPercent"])
	}
	if _, hasSecondary := currentEntry["secondaryResetAt"]; hasSecondary {
		t.Fatalf("expected secondaryResetAt to be stripped, got %v", currentEntry["secondaryResetAt"])
	}
}

func TestPrintAccountsTableRendersMarkdownStyleWithSeparator(t *testing.T) {
	t.Parallel()

	rows := []accountUsageRow{
		{
			Current:      true,
			Index:        1,
			Email:        "current@example.com",
			UsedPercent:  "22.5",
			ResetDisplay: "2h 15m",
		},
		{
			Current:      false,
			Index:        2,
			Email:        "other@example.com",
			UsedPercent:  "88",
			ResetDisplay: "1d 3h",
		},
	}

	var out strings.Builder
	if err := printAccountsTable(&out, rows); err != nil {
		t.Fatalf("printAccountsTable returned error: %v", err)
	}

	printed := out.String()

	// Header row: `|` boundaries around each column. The single usage pair
	// is now labeled WEEK% / WEEK-RESET because primary_window is the weekly
	// window in the current endpoint contract.
	if !strings.Contains(printed, "| ID |") {
		t.Fatalf("expected header to start with `| ID |`, got %q", printed)
	}
	if !strings.Contains(printed, "| WEEK-RESET |") {
		t.Fatalf("expected header to end with `| WEEK-RESET |`, got %q", printed)
	}
	if strings.Contains(printed, "| USED% |") || strings.Contains(printed, "| RESET |") {
		t.Fatalf("expected no separate USED%%/RESET columns (primary is now weekly), got %q", printed)
	}

	// Separator line: a row made of `-` and `|`.
	lines := strings.Split(printed, "\n")
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 lines (header, separator, data), got %d in %q", len(lines), printed)
	}
	separator := lines[1]
	if !strings.HasPrefix(separator, "|") || !strings.HasSuffix(separator, "|") {
		t.Fatalf("expected separator to start and end with `|`, got %q", separator)
	}
	if !strings.Contains(separator, "-") {
		t.Fatalf("expected separator to contain at least one `-`, got %q", separator)
	}
	// Separator must only contain `|`, `-`, and spaces.
	for _, r := range separator {
		if r != '|' && r != '-' && r != ' ' {
			t.Fatalf("separator must only contain `|`, `-`, and spaces, got %q (char %q)", separator, r)
		}
	}
	// The header columns and separator must align cell-by-cell: every column
	// in the header (a span between two `|`) must be the same width in the
	// separator. The width of a column is the count of characters between
	// `|` boundaries, including the visual padding.
	headerWidths := tableColumnWidths(lines[0])
	separatorWidths := tableColumnWidths(separator)
	if len(headerWidths) != len(separatorWidths) {
		t.Fatalf("header and separator cell counts differ: header=%d separator=%d (header=%q separator=%q)",
			len(headerWidths), len(separatorWidths), lines[0], separator)
	}
	for i := range headerWidths {
		if headerWidths[i] != separatorWidths[i] {
			t.Fatalf("header column %d width %d does not match separator column %d width %d (header=%q separator=%q) — widths must align",
				i, headerWidths[i], i, separatorWidths[i], lines[0], separator)
		}
	}

	// Data rows keep the active marker and stable row IDs.
	currentLine := lineContaining(printed, "current@example.com")
	if !strings.HasPrefix(currentLine, "| 1 ") {
		t.Fatalf("expected current row to start with `| 1 `, got %q", currentLine)
	}
	if !strings.Contains(currentLine, "*") {
		t.Fatalf("expected current row to contain `*` marker, got %q", currentLine)
	}
	otherLine := lineContaining(printed, "other@example.com")
	if !strings.HasPrefix(otherLine, "| 2 ") {
		t.Fatalf("expected other row to start with `| 2 `, got %q", otherLine)
	}
	if strings.Contains(otherLine, "*") {
		t.Fatalf("expected non-current row to NOT contain `*`, got %q", otherLine)
	}

	// Sanity: weekly used_percent values are present.
	if !strings.Contains(printed, "22.5") || !strings.Contains(printed, "88") {
		t.Fatalf("expected weekly used_percent values in output, got %q", printed)
	}
}

func TestPrintAccountsTablePreservesErrorSuffix(t *testing.T) {
	t.Parallel()

	rows := []accountUsageRow{
		{
			Index:       1,
			Email:       "broken@example.com",
			UsedPercent: "ERR",
			Err:         errors.New("missing access token"),
		},
	}

	var out strings.Builder
	if err := printAccountsTable(&out, rows); err != nil {
		t.Fatalf("printAccountsTable returned error: %v", err)
	}

	printed := out.String()
	if !strings.Contains(printed, "[ERR: missing access token]") {
		t.Fatalf("expected error suffix to be preserved, got %q", printed)
	}
	// Error suffix must be on the same row as the data (right after the `|`).
	rowLine := lineContaining(printed, "broken@example.com")
	if !strings.HasSuffix(strings.TrimRight(rowLine, "\n"), "]") {
		t.Fatalf("expected error suffix to live on the data row, got %q", rowLine)
	}
}

func TestRunRotatesWhenWeeklyUsageExhausted(t *testing.T) {
	t.Parallel()

	// The ChatGPT endpoint now reports `primary_window` as the weekly
	// window; rotating at >= 98% on primary matches the previous weekly
	// behavior. The 5-hour secondary window is gone.
	now := time.Now().Unix()
	weeklyReset := now + (5 * 24 * 60 * 60)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch auth {
		case "Bearer current-token":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"user_id":"user-current","rate_limit":{"primary_window":{"used_percent":98.5,"reset_at":` + strconv.FormatInt(weeklyReset, 10) + `},"secondary_window":null}}`))
		case "Bearer other-token":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"user_id":"user-other","rate_limit":{"primary_window":{"used_percent":40,"reset_at":` + strconv.FormatInt(now+7200, 10) + `}}}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("unauthorized"))
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"openai.access":"current-token","openai.accountId":"acct-current"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	accountsFile := filepath.Join(dir, "accounts.json")
	initialStore := map[string]map[string]any{
		"user-current": {
			"user_id":   "user-current",
			"accountId": "acct-current",
			"access":    "current-token",
		},
		"user-other": {
			"user_id":   "user-other",
			"accountId": "acct-other",
			"access":    "other-token",
		},
	}
	encoded, err := json.Marshal(initialStore)
	if err != nil {
		t.Fatalf("marshal store: %v", err)
	}
	if err := os.WriteFile(accountsFile, encoded, 0o600); err != nil {
		t.Fatalf("write accounts file: %v", err)
	}

	var out strings.Builder
	err = run(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		UsageURL:     server.URL,
		HTTPClient:   server.Client(),
	}, &out)
	if err != nil {
		t.Fatalf("run returned error: %v", err)
	}

	// Default stdout contract is preserved: only used_percent of primary
	// (the weekly window now).
	if out.String() != "98.5" {
		t.Fatalf("expected only used_percent value 98.5, got %q", out.String())
	}

	data, err := os.ReadFile(authFile)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	var authPayload map[string]any
	if err := json.Unmarshal(data, &authPayload); err != nil {
		t.Fatalf("parse auth JSON: %v", err)
	}
	if got, _ := authPayload["openai.access"].(string); got != "other-token" {
		t.Fatalf("expected rotation to other-token, got %q", got)
	}
	if got, _ := authPayload["openai.accountId"].(string); got != "acct-other" {
		t.Fatalf("expected rotation to acct-other, got %q", got)
	}
}

func TestRunDoesNotRotateWhenWeeklyBelowThreshold(t *testing.T) {
	t.Parallel()

	// Anything below 98% must not trigger rotation, regardless of reset.
	now := time.Now().Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"user_id":"user-current","rate_limit":{"primary_window":{"used_percent":30,"reset_at":` + strconv.FormatInt(now+3600, 10) + `}}}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth.json")
	original := `{"openai.access":"current-token","openai.accountId":"acct-current"}`
	if err := os.WriteFile(authFile, []byte(original), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	accountsFile := filepath.Join(dir, "accounts.json")
	store := map[string]map[string]any{
		"user-current": {
			"user_id":   "user-current",
			"accountId": "acct-current",
			"access":    "current-token",
		},
		"user-other": {
			"user_id":   "user-other",
			"accountId": "acct-other",
			"access":    "other-token",
		},
	}
	encoded, err := json.Marshal(store)
	if err != nil {
		t.Fatalf("marshal store: %v", err)
	}
	if err := os.WriteFile(accountsFile, encoded, 0o600); err != nil {
		t.Fatalf("write accounts file: %v", err)
	}

	var out strings.Builder
	if err := run(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		UsageURL:     server.URL,
		HTTPClient:   server.Client(),
	}, &out); err != nil {
		t.Fatalf("run returned error: %v", err)
	}

	if out.String() != "30" {
		t.Fatalf("expected only used_percent 30, got %q", out.String())
	}

	authAfter, err := os.ReadFile(authFile)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	if string(authAfter) != original {
		t.Fatalf("expected auth unchanged when below threshold, got %s", string(authAfter))
	}
}

func TestSelectEligibleAlternateAccountSkipsWeeklyExhausted(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()
	store := map[string]map[string]any{
		"user-current": {
			"user_id":   "user-current",
			"accountId": "acct-current",
			"access":    "current-token",
		},
		// FRESH weekly exhaustion: usedPercent >= 98 AND resetAt in future.
		"user-weekly-hot": {
			"user_id":       "user-weekly-hot",
			"accountId":     "acct-weekly-hot",
			"access":        "weekly-hot-token",
			"usedPercent":   "99",
			"resetAt":       now + 3600,
			"cooldownUntil": now + 3600,
		},
		"user-ready": {
			"user_id":       "user-ready",
			"accountId":     "acct-ready",
			"access":        "ready-token",
			"usedPercent":   "15",
			"cooldownUntil": now - 1,
		},
	}

	selected, ok := selectEligibleAlternateAccount(store, "user-current", now, false, defaultFiveHourThreshold, defaultWeeklyThreshold)
	if !ok {
		t.Fatalf("expected an eligible alternate account")
	}
	if got, _ := selected["user_id"].(string); got != "user-ready" {
		t.Fatalf("expected user-ready (weekly-hot with future reset must be skipped), got %q", got)
	}
}

func TestSelectEligibleAlternateAccountEligibleAfterPrimaryResetExpires(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()
	store := map[string]map[string]any{
		"user-current": {
			"user_id":   "user-current",
			"accountId": "acct-current",
			"access":    "current-token",
		},
		// Candidate with stale high usage: reset has already passed, so
		// the account is eligible again. Without this check the account
		// would stay out of rotation forever.
		"user-recovered-primary": {
			"user_id":     "user-recovered-primary",
			"accountId":   "acct-recovered-primary",
			"access":      "recovered-primary-token",
			"usedPercent": "85",
			"resetAt":     now - 1,
		},
	}

	selected, ok := selectEligibleAlternateAccount(store, "user-current", now, false, defaultFiveHourThreshold, defaultWeeklyThreshold)
	if !ok {
		t.Fatalf("expected candidate with expired reset to be eligible")
	}
	if got, _ := selected["user_id"].(string); got != "user-recovered-primary" {
		t.Fatalf("expected user-recovered-primary, got %q", got)
	}
}

func TestSelectEligibleAlternateAccountEligibleWhenResetMissingEntirely(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()
	store := map[string]map[string]any{
		"user-current": {
			"user_id":   "user-current",
			"accountId": "acct-current",
			"access":    "current-token",
		},
		// Candidate with high usage and NO recorded reset at all. Mirrors
		// the conservative "never block on missing data" rule used
		// elsewhere: the account is eligible.
		"user-no-reset": {
			"user_id":     "user-no-reset",
			"accountId":   "acct-no-reset",
			"access":      "no-reset-token",
			"usedPercent": "85",
		},
	}

	selected, ok := selectEligibleAlternateAccount(store, "user-current", now, false, defaultFiveHourThreshold, defaultWeeklyThreshold)
	if !ok {
		t.Fatalf("expected candidate with missing reset to be eligible")
	}
	if got, _ := selected["user_id"].(string); got != "user-no-reset" {
		t.Fatalf("expected user-no-reset, got %q", got)
	}
}

func TestSelectEligibleAlternateAccountSkipsHotWithFutureReset(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()
	store := map[string]map[string]any{
		"user-current": {
			"user_id":   "user-current",
			"accountId": "acct-current",
			"access":    "current-token",
		},
		// Candidate with FRESH weekly exhaustion: usedPercent >= 98 AND
		// reset is in the future.
		"user-primary-hot": {
			"user_id":     "user-primary-hot",
			"accountId":   "acct-primary-hot",
			"access":      "primary-hot-token",
			"usedPercent": "99",
			"resetAt":     now + 3600,
		},
		"user-ready": {
			"user_id":     "user-ready",
			"accountId":   "acct-ready",
			"access":      "ready-token",
			"usedPercent": "15",
		},
	}

	selected, ok := selectEligibleAlternateAccount(store, "user-current", now, false, defaultFiveHourThreshold, defaultWeeklyThreshold)
	if !ok {
		t.Fatalf("expected an eligible alternate account")
	}
	if got, _ := selected["user_id"].(string); got != "user-ready" {
		t.Fatalf("expected user-ready (hot with future reset must be skipped), got %q", got)
	}
}

func TestAccountWithUsageSetsCooldownWhenWeeklyExhausted(t *testing.T) {
	t.Parallel()

	primaryReset := int64(1777014899)

	account := map[string]any{
		"user_id":   "user-primary",
		"accountId": "acct-primary",
		"access":    "primary-token",
	}

	updated := accountWithUsage(account, usageWindow{
		UsedPercent: "99",
		ResetAt:     int64Ptr(primaryReset),
		UserID:      "user-primary",
	}, false, defaultRotationThresholds())

	if got, ok := valueToInt64(updated["cooldownUntil"]); !ok || got != primaryReset {
		t.Fatalf("expected cooldownUntil to be the weekly reset %d, got %v", primaryReset, updated["cooldownUntil"])
	}
	if _, hasSecondary := updated["secondaryUsedPercent"]; hasSecondary {
		t.Fatalf("expected secondaryUsedPercent to be stripped, got %v", updated["secondaryUsedPercent"])
	}
	if _, hasSecondary := updated["secondaryResetAt"]; hasSecondary {
		t.Fatalf("expected secondaryResetAt to be stripped, got %v", updated["secondaryResetAt"])
	}
}

func TestAccountWithUsageClearsCooldownWhenBelowThreshold(t *testing.T) {
	t.Parallel()

	primaryReset := int64(1777014899)

	account := map[string]any{
		"user_id":   "user-no-secondary",
		"accountId": "acct-no-secondary",
		"access":    "token",
		// Pretend the account is already in cooldown from a prior run.
		"cooldownUntil": primaryReset,
	}

	updated := accountWithUsage(account, usageWindow{
		UsedPercent: "35",
		ResetAt:     int64Ptr(primaryReset),
		UserID:      "user-no-secondary",
	}, false, defaultRotationThresholds())

	if _, exists := updated["cooldownUntil"]; exists {
		t.Fatalf("expected cooldownUntil to be cleared when usage is below threshold, got %v", updated["cooldownUntil"])
	}
}

func TestFormatResetDisplay(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)

	tests := []struct {
		name    string
		resetAt *int64
		want    string
	}{
		{name: "missing", resetAt: nil, want: "-"},
		{name: "expired", resetAt: int64Ptr(now.Unix()), want: "expired"},
		{name: "sub minute", resetAt: int64Ptr(now.Unix() + 30), want: "now"},
		{name: "minutes", resetAt: int64Ptr(now.Unix() + 45*60), want: "45m"},
		{name: "hours and minutes", resetAt: int64Ptr(now.Unix() + 2*60*60 + 15*60), want: "2h 15m"},
		{name: "days and hours", resetAt: int64Ptr(now.Unix() + 24*60*60 + 3*60*60), want: "1d 3h"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := formatResetDisplay(tc.resetAt, now); got != tc.want {
				t.Fatalf("formatResetDisplay() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunWithArgsListAliasWorks(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"user_id":"user-only","email":"only@example.com","rate_limit":{"primary_window":{"used_percent":11}}}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"openai.access":"token-only"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	accountsFile := filepath.Join(dir, "accounts.json")
	encoded, err := json.Marshal(map[string]map[string]any{
		"user-only": {
			"user_id": "user-only",
			"access":  "token-only",
		},
	})
	if err != nil {
		t.Fatalf("marshal store: %v", err)
	}
	if err := os.WriteFile(accountsFile, encoded, 0o600); err != nil {
		t.Fatalf("write accounts file: %v", err)
	}

	var out strings.Builder
	err = runWithArgs(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		UsageURL:     server.URL,
		HTTPClient:   server.Client(),
	}, &out, []string{"list"})
	if err != nil {
		t.Fatalf("runWithArgs(list) returned error: %v", err)
	}

	if !strings.Contains(out.String(), "only@example.com") {
		t.Fatalf("expected list alias output to include email, got %q", out.String())
	}
}

func TestExtractUsageWindowAcceptsMissingSecondaryWindow(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"user_id": "user-no-secondary",
		"rate_limit": map[string]any{
			"primary_window": map[string]any{
				"used_percent": json.Number("22.5"),
			},
		},
	}

	window, err := extractUsageWindow(payload)
	if err != nil {
		t.Fatalf("extractUsageWindow returned error: %v", err)
	}

	if window.UsedPercent != "22.5" {
		t.Fatalf("unexpected primary usedPercent: %q", window.UsedPercent)
	}
}

func TestExtractUsageWindowAcceptsNullSecondaryWindow(t *testing.T) {
	t.Parallel()

	// The current ChatGPT endpoint reports `secondary_window: null` because
	// the 5-hour window is gone. Extraction must succeed and ignore the
	// null value rather than rejecting it.
	payload := map[string]any{
		"user_id": "user-null-secondary",
		"rate_limit": map[string]any{
			"primary_window": map[string]any{
				"used_percent": json.Number("42.0"),
				"reset_at":     json.Number("1777014899"),
			},
			"secondary_window": nil,
		},
	}

	window, err := extractUsageWindow(payload)
	if err != nil {
		t.Fatalf("extractUsageWindow returned error for null secondary_window: %v", err)
	}

	if window.UsedPercent != "42.0" {
		t.Fatalf("unexpected primary usedPercent: %q", window.UsedPercent)
	}
	if window.ResetAt == nil || *window.ResetAt != 1777014899 {
		t.Fatalf("expected primary reset_at 1777014899, got %v", window.ResetAt)
	}
}

func TestExtractUsageWindowPopulatesSecondaryWindow(t *testing.T) {
	t.Parallel()

	// When the API exposes an object-shaped secondary_window, the parser
	// must extract both `used_percent` and `reset_at` without erroring.
	payload := map[string]any{
		"user_id": "user-legacy-secondary",
		"rate_limit": map[string]any{
			"primary_window": map[string]any{
				"used_percent": json.Number("11.0"),
				"reset_at":     json.Number("1777014899"),
			},
			"secondary_window": map[string]any{
				"used_percent": json.Number("3.25"),
				"reset_at":     json.Number("1777619699"),
			},
		},
	}

	window, err := extractUsageWindow(payload)
	if err != nil {
		t.Fatalf("extractUsageWindow returned error: %v", err)
	}

	if window.UsedPercent != "11.0" {
		t.Fatalf("unexpected primary usedPercent: %q", window.UsedPercent)
	}
	if window.ResetAt == nil || *window.ResetAt != 1777014899 {
		t.Fatalf("expected primary reset_at 1777014899, got %v", window.ResetAt)
	}
	if window.SecondaryUsedPercent != "3.25" {
		t.Fatalf("expected secondary used_percent 3.25, got %q", window.SecondaryUsedPercent)
	}
	if window.SecondaryResetAt == nil || *window.SecondaryResetAt != 1777619699 {
		t.Fatalf("expected secondary reset_at 1777619699, got %v", window.SecondaryResetAt)
	}
}

// TestExtractUsageWindowSecondaryWindowErrors pins the malformed-secondary
// contract: a non-null `secondary_window` that is not an object, that is
// missing/null `used_percent`, or that has a malformed `used_percent`/`reset_at`,
// must surface as an API error rather than silently fall back (and risk
// interpreting the 5h primary as the weekly window). Missing/null
// `secondary_window` itself remains the only valid fallback.
func TestExtractUsageWindowSecondaryWindowErrors(t *testing.T) {
	t.Parallel()

	primaryWindow := map[string]any{
		"used_percent": json.Number("11.0"),
		"reset_at":     json.Number("1777014899"),
	}

	cases := []struct {
		name     string
		seconday any
		wantErr  string // substring expected in the error; empty means no error
	}{
		{
			name:     "non-object string secondary_window is an error",
			seconday: "garbage",
			wantErr:  "must be an object",
		},
		{
			name:     "non-object numeric secondary_window is an error",
			seconday: json.Number("42"),
			wantErr:  "must be an object",
		},
		{
			name: "malformed used_percent is an error",
			seconday: map[string]any{
				"used_percent": map[string]any{"nested": true},
			},
			wantErr: "used_percent",
		},
		{
			name: "malformed reset_at is an error",
			seconday: map[string]any{
				"used_percent": json.Number("50"),
				"reset_at":     []any{1, 2, 3},
			},
			wantErr: "reset_at",
		},
		{
			name:     "empty object (no used_percent) is an error",
			seconday: map[string]any{},
			wantErr:  "used_percent",
		},
		{
			name: "missing used_percent key is an error",
			seconday: map[string]any{
				"reset_at": json.Number("1777619699"),
			},
			wantErr: "used_percent",
		},
		{
			name: "null used_percent is an error",
			seconday: map[string]any{
				"used_percent": nil,
				"reset_at":     json.Number("1777619699"),
			},
			wantErr: "used_percent",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			payload := map[string]any{
				"user_id": "user-mixed",
				"rate_limit": map[string]any{
					"primary_window":   primaryWindow,
					"secondary_window": tc.seconday,
				},
			}
			_, err := extractUsageWindow(payload)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error for %q, got %v", tc.name, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error for %q, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error to mention %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestResolveAccountIdentifierMatchesIndexUserIDAndEmail(t *testing.T) {
	t.Parallel()

	store := map[string]map[string]any{
		"user-alpha": {
			"user_id": "user-alpha",
			"email":   "alpha@example.com",
			"access":  "alpha-token",
		},
		"user-bravo": {
			"user_id": "user-bravo",
			"email":   "bravo@example.com",
			"access":  "bravo-token",
		},
		"user-charlie": {
			"user_id": "user-charlie",
			"email":   "charlie@example.com",
			"access":  "charlie-token",
		},
	}
	keys := sortedUserIDs(store)

	// Row index requires the `#` prefix: 1-based, 2 => bravo, 3 => charlie.
	account, userID, err := resolveAccountIdentifier(store, keys, "#2")
	if err != nil {
		t.Fatalf("resolveAccountIdentifier(#2) returned error: %v", err)
	}
	if userID != "user-bravo" {
		t.Fatalf("expected #2 to resolve to user-bravo, got %q", userID)
	}
	if got, _ := account["access"].(string); got != "bravo-token" {
		t.Fatalf("expected bravo access token, got %q", got)
	}

	// Exact user_id.
	account, userID, err = resolveAccountIdentifier(store, keys, "user-charlie")
	if err != nil {
		t.Fatalf("resolveAccountIdentifier(user_id) returned error: %v", err)
	}
	if userID != "user-charlie" {
		t.Fatalf("expected user_id resolution to user-charlie, got %q", userID)
	}

	// Email (case-insensitive).
	account, userID, err = resolveAccountIdentifier(store, keys, "Alpha@Example.com")
	if err != nil {
		t.Fatalf("resolveAccountIdentifier(email) returned error: %v", err)
	}
	if userID != "user-alpha" {
		t.Fatalf("expected email resolution to user-alpha, got %q", userID)
	}
}

func TestResolveAccountIdentifierDoesNotCollideNumericUserIDWithRowIndex(t *testing.T) {
	t.Parallel()

	// Store with numeric user_ids that sort lexicographically: "1", "10", "2".
	// Index 2 of the sorted list is "10", but `use 2` MUST select user_id "2"
	// (exact match), not user_id "10" via the row index.
	store := map[string]map[string]any{
		"1": {
			"user_id": "1",
			"email":   "one@example.com",
			"access":  "one-token",
		},
		"2": {
			"user_id": "2",
			"email":   "two@example.com",
			"access":  "two-token",
		},
		"10": {
			"user_id": "10",
			"email":   "ten@example.com",
			"access":  "ten-token",
		},
	}
	keys := sortedUserIDs(store)
	if len(keys) != 3 || keys[0] != "1" || keys[1] != "10" || keys[2] != "2" {
		t.Fatalf("expected sorted keys [1 10 2], got %v", keys)
	}

	// `use 2` must match the exact user_id "2", not the row at index 2.
	account, userID, err := resolveAccountIdentifier(store, keys, "2")
	if err != nil {
		t.Fatalf("resolveAccountIdentifier(2) returned error: %v", err)
	}
	if userID != "2" {
		t.Fatalf("expected exact user_id match for '2', got %q", userID)
	}
	if got, _ := account["access"].(string); got != "two-token" {
		t.Fatalf("expected two-token, got %q", got)
	}

	// `use #2` must match the row at index 2, which is user_id "10".
	account, userID, err = resolveAccountIdentifier(store, keys, "#2")
	if err != nil {
		t.Fatalf("resolveAccountIdentifier(#2) returned error: %v", err)
	}
	if userID != "10" {
		t.Fatalf("expected #2 to resolve to user_id '10' (row 2), got %q", userID)
	}
	if got, _ := account["access"].(string); got != "ten-token" {
		t.Fatalf("expected ten-token, got %q", got)
	}
}

func TestResolveAccountIdentifierReturnsErrorOnUnknown(t *testing.T) {
	t.Parallel()

	store := map[string]map[string]any{
		"user-alpha": {
			"user_id": "user-alpha",
			"access":  "alpha-token",
		},
	}
	keys := sortedUserIDs(store)

	_, _, err := resolveAccountIdentifier(store, keys, "user-zzz")
	if err == nil {
		t.Fatalf("expected error for unknown identifier")
	}

	_, _, err = resolveAccountIdentifier(store, keys, "#9")
	if err == nil {
		t.Fatalf("expected error for out-of-range index")
	}

	_, _, err = resolveAccountIdentifier(store, keys, "#0")
	if err == nil {
		t.Fatalf("expected error for non-positive index")
	}
}

func TestRunUseCommandSwitchesAuthFileByIndex(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"openai.access":"current-token","openai.accountId":"acct-current"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	accountsFile := filepath.Join(dir, "accounts.json")
	initialStore := map[string]map[string]any{
		"user-current": {
			"user_id":   "user-current",
			"accountId": "acct-current",
			"access":    "current-token",
		},
		"user-other": {
			"user_id":   "user-other",
			"accountId": "acct-other",
			"email":     "other@example.com",
			"access":    "other-token",
		},
	}
	encoded, err := json.Marshal(initialStore)
	if err != nil {
		t.Fatalf("marshal store: %v", err)
	}
	if err := os.WriteFile(accountsFile, encoded, 0o600); err != nil {
		t.Fatalf("write accounts file: %v", err)
	}

	var out strings.Builder
	err = runUseCommand(config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
	}, &out, "#2")
	if err != nil {
		t.Fatalf("runUseCommand returned error: %v", err)
	}

	if !strings.Contains(out.String(), "user-other") {
		t.Fatalf("expected confirmation message to mention user-other, got %q", out.String())
	}
	if !strings.Contains(out.String(), "other@example.com") {
		t.Fatalf("expected confirmation message to mention other@example.com, got %q", out.String())
	}
	if strings.Contains(out.String(), "other-token") {
		t.Fatalf("must not leak access token in output, got %q", out.String())
	}

	data, err := os.ReadFile(authFile)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}

	var authPayload map[string]any
	if err := json.Unmarshal(data, &authPayload); err != nil {
		t.Fatalf("parse auth JSON: %v", err)
	}

	if got, _ := authPayload["openai.access"].(string); got != "other-token" {
		t.Fatalf("expected openai.access to be other-token, got %q", got)
	}
	if got, _ := authPayload["openai.accountId"].(string); got != "acct-other" {
		t.Fatalf("expected openai.accountId to be acct-other, got %q", got)
	}
}

func TestRunUseCommandAcceptsEmailAndUserID(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"openai.access":"current-token","openai.accountId":"acct-current"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	accountsFile := filepath.Join(dir, "accounts.json")
	initialStore := map[string]map[string]any{
		"user-current": {
			"user_id":   "user-current",
			"accountId": "acct-current",
			"access":    "current-token",
		},
		"user-target": {
			"user_id":   "user-target",
			"accountId": "acct-target",
			"email":     "target@example.com",
			"access":    "target-token",
		},
	}
	encoded, err := json.Marshal(initialStore)
	if err != nil {
		t.Fatalf("marshal store: %v", err)
	}
	if err := os.WriteFile(accountsFile, encoded, 0o600); err != nil {
		t.Fatalf("write accounts file: %v", err)
	}

	t.Run("by user_id", func(t *testing.T) {
		// Reset auth file to a known state for this subtest.
		if err := os.WriteFile(authFile, []byte(`{"openai.access":"current-token","openai.accountId":"acct-current"}`), 0o600); err != nil {
			t.Fatalf("write auth file: %v", err)
		}

		var out strings.Builder
		if err := runUseCommand(config{
			AuthFile:     authFile,
			AccountsFile: accountsFile,
		}, &out, "user-target"); err != nil {
			t.Fatalf("runUseCommand(user_id) returned error: %v", err)
		}

		data, _ := os.ReadFile(authFile)
		var authPayload map[string]any
		if err := json.Unmarshal(data, &authPayload); err != nil {
			t.Fatalf("parse auth JSON: %v", err)
		}
		if got, _ := authPayload["openai.access"].(string); got != "target-token" {
			t.Fatalf("expected openai.access target-token, got %q", got)
		}
	})

	t.Run("by email case-insensitive", func(t *testing.T) {
		// Reset auth file again.
		if err := os.WriteFile(authFile, []byte(`{"openai.access":"current-token","openai.accountId":"acct-current"}`), 0o600); err != nil {
			t.Fatalf("write auth file: %v", err)
		}

		var out strings.Builder
		if err := runUseCommand(config{
			AuthFile:     authFile,
			AccountsFile: accountsFile,
		}, &out, "TARGET@example.com"); err != nil {
			t.Fatalf("runUseCommand(email) returned error: %v", err)
		}

		data, _ := os.ReadFile(authFile)
		var authPayload map[string]any
		if err := json.Unmarshal(data, &authPayload); err != nil {
			t.Fatalf("parse auth JSON: %v", err)
		}
		if got, _ := authPayload["openai.access"].(string); got != "target-token" {
			t.Fatalf("expected openai.access target-token, got %q", got)
		}
	})
}

func TestRunUseCommandRejectsUnknownIdentifier(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"openai.access":"current-token","openai.accountId":"acct-current"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	accountsFile := filepath.Join(dir, "accounts.json")
	initialStore := map[string]map[string]any{
		"user-current": {
			"user_id": "user-current",
			"access":  "current-token",
		},
	}
	encoded, err := json.Marshal(initialStore)
	if err != nil {
		t.Fatalf("marshal store: %v", err)
	}
	if err := os.WriteFile(accountsFile, encoded, 0o600); err != nil {
		t.Fatalf("write accounts file: %v", err)
	}

	err = runUseCommand(config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
	}, &strings.Builder{}, "user-missing")
	if err == nil {
		t.Fatalf("expected error for unknown identifier")
	}

	// Auth file should be unchanged after a failed switch.
	data, _ := os.ReadFile(authFile)
	if !strings.Contains(string(data), "current-token") {
		t.Fatalf("expected auth file unchanged after failed switch, got %s", string(data))
	}
}

func TestRunWithArgsUseRoutesToRunUseCommand(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"openai.access":"current-token","openai.accountId":"acct-current"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	accountsFile := filepath.Join(dir, "accounts.json")
	initialStore := map[string]map[string]any{
		"user-current": {
			"user_id":   "user-current",
			"accountId": "acct-current",
			"access":    "current-token",
		},
		"user-next": {
			"user_id":   "user-next",
			"accountId": "acct-next",
			"access":    "next-token",
		},
	}
	encoded, err := json.Marshal(initialStore)
	if err != nil {
		t.Fatalf("marshal store: %v", err)
	}
	if err := os.WriteFile(accountsFile, encoded, 0o600); err != nil {
		t.Fatalf("write accounts file: %v", err)
	}

	var out strings.Builder
	err = runWithArgs(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		UsageURL:     "http://unused.invalid/",
	}, &out, []string{"use", "user-next"})
	if err != nil {
		t.Fatalf("runWithArgs(use) returned error: %v", err)
	}

	data, _ := os.ReadFile(authFile)
	var authPayload map[string]any
	if err := json.Unmarshal(data, &authPayload); err != nil {
		t.Fatalf("parse auth JSON: %v", err)
	}
	if got, _ := authPayload["openai.access"].(string); got != "next-token" {
		t.Fatalf("expected openai.access next-token, got %q", got)
	}
}

func TestRunWithArgsUseRequiresIdentifier(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"openai.access":"current-token"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	accountsFile := filepath.Join(dir, "accounts.json")

	err := runWithArgs(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		UsageURL:     "http://unused.invalid/",
	}, &strings.Builder{}, []string{"use"})
	if err == nil {
		t.Fatalf("expected error for use without identifier")
	}
	if !strings.Contains(err.Error(), "identifier") {
		t.Fatalf("expected error to mention identifier, got %v", err)
	}
}

func lineContaining(text, needle string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

// splitTableRow splits a Markdown-style table row (`| a | b | c |`) into the
// trimmed cell values. Leading/trailing pipes are required.
func splitTableRow(row string) []string {
	trimmed := strings.TrimSpace(row)
	if !strings.HasPrefix(trimmed, "|") || !strings.HasSuffix(trimmed, "|") {
		return nil
	}
	inner := trimmed[1 : len(trimmed)-1]
	parts := strings.Split(inner, "|")
	cells := make([]string, len(parts))
	for i, p := range parts {
		cells[i] = strings.TrimSpace(p)
	}
	return cells
}

// tableColumnWidths returns the visual width of each column in a Markdown-style
// table row, measured as the number of characters between the `|` boundaries
// (including the single space of padding on each side). For
// `"| EMAIL               |"` it returns 19, matching the 5 chars of "EMAIL"
// plus the 14 spaces of right-padding required to align with the data cells.
func tableColumnWidths(row string) []int {
	trimmed := strings.TrimSpace(row)
	if !strings.HasPrefix(trimmed, "|") || !strings.HasSuffix(trimmed, "|") {
		return nil
	}
	inner := trimmed[1 : len(trimmed)-1]
	parts := strings.Split(inner, "|")
	widths := make([]int, len(parts))
	for i, p := range parts {
		widths[i] = len(p)
	}
	return widths
}

func mustReadStore(t *testing.T, accountsFile string) map[string]map[string]any {
	t.Helper()

	data, err := os.ReadFile(accountsFile)
	if err != nil {
		t.Fatalf("read accounts file: %v", err)
	}

	var store map[string]map[string]any
	if err := json.Unmarshal(data, &store); err != nil {
		t.Fatalf("parse accounts JSON: %v", err)
	}

	return store
}

func int64Ptr(v int64) *int64 {
	return &v
}

// mustReadFile reads content from path; used by the 5h toggle tests for
// assertions on the persisted config file.
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// writeConfigFile writes content to the given path. Production save paths use
// 0o600; tests share the helper for both pre-seeded files and follow-up reads.
func writeConfigFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
}

// 5h toggle contract: load/save, normalization, dual response mapping, and the `config` subcommand.

// TestLoadFiveHourConfig is the contract for the loader: missing file, missing
// key, on/off string reads, boolean reads, invalid values, malformed JSON, and
// the empty-path (the no-config-at-all runtime contract) all default to ON.
func TestLoadFiveHourConfig(t *testing.T) {
	t.Parallel()

	type want struct {
		enabled bool
		err     bool
	}
	tests := []struct {
		name    string
		setup   func(t *testing.T) string
		content string
		want    want
	}{
		{name: "empty path defaults to ON", setup: func(t *testing.T) string { return "" }, want: want{enabled: true}},
		{name: "missing file defaults to ON", setup: func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing.json") }, want: want{enabled: true}},
		{name: "missing key defaults to ON", setup: func(t *testing.T) string { return filepath.Join(t.TempDir(), "config.json") }, content: `{"other":"kept"}`, want: want{enabled: true}},
		{name: "reads on", setup: func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "c.json")
			writeConfigFile(t, p, `{"5h":"on"}`)
			return p
		}, want: want{enabled: true}},
		{name: "reads off", setup: func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "c.json")
			writeConfigFile(t, p, `{"5h":"off"}`)
			return p
		}, want: want{enabled: false}},
		{name: "boolean true", setup: func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "c.json")
			writeConfigFile(t, p, `{"5h":true}`)
			return p
		}, want: want{enabled: true}},
		{name: "boolean false", setup: func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "c.json")
			writeConfigFile(t, p, `{"5h":false}`)
			return p
		}, want: want{enabled: false}},
		{name: "invalid string", setup: func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "c.json")
			writeConfigFile(t, p, `{"5h":"maybe"}`)
			return p
		}, want: want{err: true}},
		{name: "invalid number", setup: func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "c.json")
			writeConfigFile(t, p, `{"5h":42}`)
			return p
		}, want: want{err: true}},
		{name: "malformed JSON", setup: func(t *testing.T) string {
			p := filepath.Join(t.TempDir(), "c.json")
			writeConfigFile(t, p, `{bad`)
			return p
		}, want: want{err: true}},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := tc.setup(t)
			enabled, err := loadFiveHourConfig(path)
			if tc.want.err {
				if err == nil {
					t.Fatalf("expected error, got enabled=%v", enabled)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadFiveHourConfig: %v", err)
			}
			if enabled != tc.want.enabled {
				t.Fatalf("enabled=%v, want %v", enabled, tc.want.enabled)
			}
		})
	}
}

// TestSaveFiveHourConfig pins the writer: on/off write the expected string,
// unknown keys are preserved, and the parent directory is created on demand.
func TestSaveFiveHourConfig(t *testing.T) {
	t.Parallel()

	t.Run("writes on", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "config.json")
		if err := saveFiveHourConfig(path, true); err != nil {
			t.Fatalf("save: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(mustReadFile(t, path), &payload); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got, _ := payload["5h"].(string); got != "on" {
			t.Fatalf("expected 5h=on, got %v", payload["5h"])
		}
	})

	t.Run("writes off and preserves unknown keys", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "config.json")
		writeConfigFile(t, path, `{"other":"kept","nested":{"x":1}}`)
		if err := saveFiveHourConfig(path, false); err != nil {
			t.Fatalf("save: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(mustReadFile(t, path), &payload); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got, _ := payload["5h"].(string); got != "off" {
			t.Fatalf("expected 5h=off, got %v", payload["5h"])
		}
		if got, _ := payload["other"].(string); got != "kept" {
			t.Fatalf("expected unknown key preserved, got %v", payload["other"])
		}
		if _, ok := payload["nested"]; !ok {
			t.Fatalf("expected nested object preserved, got %v", payload)
		}
	})

	t.Run("creates parent directory", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "nested", "subdir", "config.json")
		if err := saveFiveHourConfig(path, true); err != nil {
			t.Fatalf("save: %v", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected config file to exist: %v", err)
		}
	})

	t.Run("rejects empty path", func(t *testing.T) {
		t.Parallel()
		if err := saveFiveHourConfig("", true); err == nil {
			t.Fatalf("expected error for empty config path")
		}
	})

	// Permissions tightening: a pre-existing file with looser mode must be
	// chmod'd to 0o600 after save. os.WriteFile only applies 0o600 on initial
	// creation, so without the explicit chmod an existing 0o644 file would keep
	// its permissive mode. Skipped on Windows where chmod is a no-op.
	t.Run("tightens pre-existing permissive file to 0o600", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("chmod is a no-op on Windows")
		}
		t.Parallel()
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"other":"kept"}`), 0o644); err != nil {
			t.Fatalf("seed permissive file: %v", err)
		}
		before, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat before: %v", err)
		}
		if mode := before.Mode().Perm(); mode != 0o644 {
			t.Fatalf("expected seeded file mode 0o644, got %o", mode)
		}
		if err := saveFiveHourConfig(path, true); err != nil {
			t.Fatalf("save: %v", err)
		}
		after, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat after: %v", err)
		}
		if mode := after.Mode().Perm(); mode != 0o600 {
			t.Fatalf("expected file mode 0o600 after save, got %o", mode)
		}
		var payload map[string]any
		if err := json.Unmarshal(mustReadFile(t, path), &payload); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got, _ := payload["5h"].(string); got != "on" {
			t.Fatalf("expected 5h=on, got %v", payload["5h"])
		}
		if got, _ := payload["other"].(string); got != "kept" {
			t.Fatalf("expected unknown key preserved, got %v", payload["other"])
		}
	})
}

// TestRunConfigCommand covers the `config` subcommand end-to-end: enabling,
// disabling, state reporting, malformed/invalid input, and the requirement
// that auth/accounts are never consulted.
func TestRunConfigCommand(t *testing.T) {
	t.Parallel()

	t.Run("writes on/off and reports state", func(t *testing.T) {
		t.Parallel()
		for _, enable := range []string{"on", "off"} {
			enable := enable
			t.Run(enable, func(t *testing.T) {
				t.Parallel()
				path := filepath.Join(t.TempDir(), "config.json")
				var out strings.Builder
				if err := runConfigCommand(config{ConfigFile: path}, &out, []string{"5h", enable}); err != nil {
					t.Fatalf("runConfigCommand: %v", err)
				}
				if !strings.Contains(out.String(), enable) {
					t.Fatalf("expected %q in output, got %q", enable, out.String())
				}
				var payload map[string]any
				_ = json.Unmarshal(mustReadFile(t, path), &payload)
				if got, _ := payload["5h"].(string); got != enable {
					t.Fatalf("expected 5h=%s on disk, got %v", enable, payload["5h"])
				}
			})
		}
	})

	t.Run("effective state", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name, content, want string
		}{
			{"on", `{"5h":"on"}`, "on"},
			{"off", `{"5h":"off"}`, "off"},
			{"missing defaults to on", "", "on"},
		} {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				path := filepath.Join(t.TempDir(), "config.json")
				if tc.content != "" {
					writeConfigFile(t, path, tc.content)
				}
				var out strings.Builder
				if err := runConfigCommand(config{ConfigFile: path}, &out, []string{"5h"}); err != nil {
					t.Fatalf("runConfigCommand: %v", err)
				}
				if got := strings.TrimSpace(out.String()); got != tc.want {
					t.Fatalf("got %q, want %q", got, tc.want)
				}
			})
		}
	})

	t.Run("invalid value rejected", func(t *testing.T) {
		t.Parallel()
		if err := runConfigCommand(config{ConfigFile: filepath.Join(t.TempDir(), "c.json")}, &strings.Builder{}, []string{"5h", "maybe"}); err == nil {
			t.Fatalf("expected error for invalid value")
		}
	})

	t.Run("malformed config rejected", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "config.json")
		writeConfigFile(t, path, `{bad`)
		if err := runConfigCommand(config{ConfigFile: path}, &strings.Builder{}, []string{"5h"}); err == nil {
			t.Fatalf("expected error for malformed JSON")
		}
	})

	t.Run("missing feature name rejected", func(t *testing.T) {
		t.Parallel()
		if err := runConfigCommand(config{ConfigFile: filepath.Join(t.TempDir(), "c.json")}, &strings.Builder{}, nil); err == nil {
			t.Fatalf("expected error for missing feature")
		}
	})

	t.Run("unknown feature rejected", func(t *testing.T) {
		t.Parallel()
		err := runConfigCommand(config{ConfigFile: filepath.Join(t.TempDir(), "c.json")}, &strings.Builder{}, []string{"unknown", "on"})
		if err == nil || !strings.Contains(err.Error(), "unknown") {
			t.Fatalf("expected unknown feature error, got %v", err)
		}
	})

	t.Run("does not require auth or accounts file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		var out strings.Builder
		err := runConfigCommand(config{
			AuthFile:     filepath.Join(dir, "missing-auth.json"),
			AccountsFile: filepath.Join(dir, "missing-accounts.json"),
			ConfigFile:   filepath.Join(dir, "config.json"),
		}, &out, []string{"5h", "on"})
		if err != nil {
			t.Fatalf("config must work without auth/accounts, got: %v", err)
		}
	})
}

// TestRunWithArgsConfigRoutesBeforeAuthValidation pins the integration
// contract: `config` is the only subcommand that must succeed with empty
// AuthFile/AccountsFile/UsageURL. Every other command still rejects them.
func TestRunWithArgsConfigRoutesBeforeAuthValidation(t *testing.T) {
	t.Parallel()

	t.Run("config 5h routes on/off/state without auth/accounts/usage", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name, arg, content, wantOutput, wantStored string
		}{
			{"on", "on", "", "on", "on"},
			{"off", "off", "", "off", "off"},
			{"state", "", `{"5h":"off"}`, "off", ""}, // pre-seeded, no write
		} {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				path := filepath.Join(t.TempDir(), "config.json")
				if tc.content != "" {
					writeConfigFile(t, path, tc.content)
				}
				args := []string{"config", "5h"}
				if tc.arg != "" {
					args = append(args, tc.arg)
				}
				var out strings.Builder
				if err := runWithArgs(context.Background(), config{
					AuthFile: "", AccountsFile: "", UsageURL: "", ConfigFile: path,
				}, &out, args); err != nil {
					t.Fatalf("expected config to work without auth/accounts/usage, got: %v", err)
				}
				if got := strings.TrimSpace(out.String()); got != tc.wantOutput {
					t.Fatalf("stdout=%q, want %q", got, tc.wantOutput)
				}
				if tc.wantStored != "" {
					var payload map[string]any
					_ = json.Unmarshal(mustReadFile(t, path), &payload)
					if got, _ := payload["5h"].(string); got != tc.wantStored {
						t.Fatalf("stored 5h=%q, want %q", got, tc.wantStored)
					}
				}
			})
		}
	})

	t.Run("empty ConfigFile reports clear error", func(t *testing.T) {
		t.Parallel()
		err := runWithArgs(context.Background(), config{ConfigFile: ""}, &strings.Builder{}, []string{"config", "5h", "on"})
		if err == nil || !strings.Contains(err.Error(), "config file path is empty") {
			t.Fatalf("expected clear empty-config error, got %v", err)
		}
	})

	t.Run("non-config command still rejects empty Auth", func(t *testing.T) {
		t.Parallel()
		err := runWithArgs(context.Background(), config{
			AuthFile: "", AccountsFile: "", UsageURL: "http://unused.invalid/",
			ConfigFile: filepath.Join(t.TempDir(), "config.json"),
		}, &strings.Builder{}, nil)
		if err == nil || !strings.Contains(err.Error(), "auth file path is empty") {
			t.Fatalf("expected auth file path error, got %v", err)
		}
	})
}

// TestDefaultConfig pins the env override and the documented default location
// for the toggle config file.
func TestDefaultConfig(t *testing.T) {
	t.Run("env override wins", func(t *testing.T) {
		custom := filepath.Join(t.TempDir(), "my-config.json")
		t.Setenv("CODEX_USAGE_CONFIG_FILE", custom)
		if cfg := defaultConfig(); cfg.ConfigFile != custom {
			t.Fatalf("expected %q, got %q", custom, cfg.ConfigFile)
		}
	})

	t.Run("default location is codex-usage-cli/config.json", func(t *testing.T) {
		t.Setenv("CODEX_USAGE_CONFIG_FILE", "")
		t.Setenv("HOME", "")
		t.Setenv("USERPROFILE", t.TempDir())
		cfg := defaultConfig()
		want := filepath.Join(".local", "share", "codex-usage-cli", "config.json")
		if !strings.HasSuffix(cfg.ConfigFile, want) {
			t.Fatalf("expected path to end with %q, got %q", want, cfg.ConfigFile)
		}
	})
}

// TestRunWithArgsDefaults5hToOnWhenConfigFileEmpty is the zero-value contract:
// a `config{}` constructed without a ConfigFile must still treat 5h as ON,
// matching the documented runtime default. The CLI must rely on the loader as
// the single source of truth for that default rather than the zero value.
func TestRunWithArgsDefaults5hToOnWhenConfigFileEmpty(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"user-1","rate_limit":{"primary_window":{"used_percent":35},"secondary_window":{"used_percent":60}}}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"openai.access":"token"}`), 0o600); err != nil {
		t.Fatalf("write auth: %v", err)
	}

	var out strings.Builder
	err := runWithArgs(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: filepath.Join(dir, "accounts.json"),
		UsageURL:     server.URL,
		HTTPClient:   server.Client(),
		// ConfigFile intentionally empty; FiveHourEnabled intentionally false.
	}, &out, nil)
	if err != nil {
		t.Fatalf("runWithArgs: %v", err)
	}
	// 5h ON + dual response: stdout reports the 5h primary (35), not the
	// weekly secondary (60). If the loader's ON default is bypassed the test
	// would see 60 because OFF promotes the weekly.
	if out.String() != "35" {
		t.Fatalf("expected 5h primary 35 (default 5h ON), got %q", out.String())
	}
}

// TestNormalizeUsageForToggle is the single source of truth for the OFF/ON
// mapping: stdout, persistence, rotation and cooldown all observe whatever
// this helper returns. Each branch pins one contract.
func TestNormalizeUsageForToggle(t *testing.T) {
	t.Parallel()

	primaryReset := int64(1777014899)
	secondaryReset := int64(1777619699)
	withBoth := usageWindow{
		UsedPercent: "35", ResetAt: int64Ptr(primaryReset),
		SecondaryUsedPercent: "60", SecondaryResetAt: int64Ptr(secondaryReset),
	}
	primaryOnly := usageWindow{UsedPercent: "35", ResetAt: int64Ptr(primaryReset)}

	t.Run("5h on + dual keeps primary as 5h and secondary as weekly", func(t *testing.T) {
		t.Parallel()
		out, dual := normalizeUsageForToggle(withBoth, true)
		if !dual || out.UsedPercent != "35" || out.SecondaryUsedPercent != "60" {
			t.Fatalf("unexpected: dual=%v used=%q secondary=%q", dual, out.UsedPercent, out.SecondaryUsedPercent)
		}
	})

	t.Run("5h on + secondary null falls back to primary as weekly", func(t *testing.T) {
		t.Parallel()
		out, dual := normalizeUsageForToggle(primaryOnly, true)
		if dual || out.UsedPercent != "35" || strings.TrimSpace(out.SecondaryUsedPercent) != "" {
			t.Fatalf("unexpected: dual=%v used=%q secondary=%q", dual, out.UsedPercent, out.SecondaryUsedPercent)
		}
	})

	t.Run("5h off + dual promotes weekly secondary into primary", func(t *testing.T) {
		t.Parallel()
		out, dual := normalizeUsageForToggle(withBoth, false)
		if dual || out.UsedPercent != "60" {
			t.Fatalf("expected weekly promoted to 60, got dual=%v used=%q", dual, out.UsedPercent)
		}
		if out.ResetAt == nil || *out.ResetAt != secondaryReset {
			t.Fatalf("expected weekly reset promoted, got %v", out.ResetAt)
		}
		if strings.TrimSpace(out.SecondaryUsedPercent) != "" || out.SecondaryResetAt != nil {
			t.Fatalf("expected secondary cleared, got %+v", out)
		}
	})

	t.Run("5h off + secondary null keeps primary as weekly fallback", func(t *testing.T) {
		t.Parallel()
		out, dual := normalizeUsageForToggle(primaryOnly, false)
		if dual || out.UsedPercent != "35" {
			t.Fatalf("unexpected: dual=%v used=%q", dual, out.UsedPercent)
		}
	})
}

// TestRunDualStdoutAndPersistence covers the dual-response stdout contract and
// the OFF-promotes-weekly persistence contract end-to-end through runWithArgs.
func TestRunDualStdoutAndPersistence(t *testing.T) {
	t.Parallel()

	dualBody := `{"user_id":"user-1","rate_limit":{"primary_window":{"used_percent":35,"reset_at":1777014899},"secondary_window":{"used_percent":60,"reset_at":1777619699}}}`
	nullSecondaryBody := `{"user_id":"user-1","rate_limit":{"primary_window":{"used_percent":35,"reset_at":1777014899},"secondary_window":null}}`

	cases := []struct {
		name, body     string
		fiveHour       bool
		stdout, stored string
		stripSecondary bool
	}{
		{"5h on + dual: stdout is 5h primary (35)", dualBody, true, "35", "35", false},
		{"5h on + secondary null: stdout is primary as weekly (35)", nullSecondaryBody, true, "35", "35", false},
		{"5h off + dual: stdout is promoted weekly (60)", dualBody, false, "60", "60", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "config.json")
			if err := saveFiveHourConfig(cfgPath, tc.fiveHour); err != nil {
				t.Fatalf("save config: %v", err)
			}
			authFile := filepath.Join(dir, "auth.json")
			if err := os.WriteFile(authFile, []byte(`{"openai.access":"token"}`), 0o600); err != nil {
				t.Fatalf("write auth: %v", err)
			}
			accountsFile := filepath.Join(dir, "accounts.json")

			var out strings.Builder
			if err := runWithArgs(context.Background(), config{
				AuthFile: authFile, AccountsFile: accountsFile, ConfigFile: cfgPath,
				UsageURL: server.URL, HTTPClient: server.Client(),
			}, &out, nil); err != nil {
				t.Fatalf("runWithArgs: %v", err)
			}
			if out.String() != tc.stdout {
				t.Fatalf("stdout=%q, want %q", out.String(), tc.stdout)
			}
			store := mustReadStore(t, accountsFile)
			acct, ok := store["user-1"]
			if !ok {
				t.Fatalf("expected user-1 persisted")
			}
			if got, _ := acct["usedPercent"].(string); got != tc.stored {
				t.Fatalf("stored usedPercent=%q, want %q", got, tc.stored)
			}
			if tc.stripSecondary {
				if _, has := acct["secondaryUsedPercent"]; has {
					t.Fatalf("expected secondaryUsedPercent stripped")
				}
				if _, has := acct["secondaryResetAt"]; has {
					t.Fatalf("expected secondaryResetAt stripped")
				}
			}
		})
	}
}

// TestAccountWithUsageCooldownAndStripping pins the new dual-mode persistence
// behavior: 5h off strips any stale secondary fields left over from prior 5h-on
// runs; 5h on uses the later reset when both windows are exhausted.
func TestAccountWithUsageCooldownAndStripping(t *testing.T) {
	t.Parallel()

	primaryReset := int64(1777014899)
	weeklyReset := int64(1777619699)

	t.Run("5h off strips stale secondary fields", func(t *testing.T) {
		t.Parallel()
		stripped := accountWithUsage(map[string]any{
			"user_id": "u1", "access": "t",
			"secondaryUsedPercent": "stale", "secondaryResetAt": int64(1700000000),
		}, usageWindow{
			UsedPercent: "70", ResetAt: int64Ptr(primaryReset), UserID: "u1",
		}, false, defaultRotationThresholds())
		if _, has := stripped["secondaryUsedPercent"]; has {
			t.Fatalf("strip: secondaryUsedPercent should be removed")
		}
		if _, has := stripped["secondaryResetAt"]; has {
			t.Fatalf("strip: secondaryResetAt should be removed")
		}
	})

	t.Run("5h on + both exhausted takes later reset", func(t *testing.T) {
		t.Parallel()
		updated := accountWithUsage(map[string]any{"user_id": "u1", "access": "t"}, usageWindow{
			UsedPercent: "85", ResetAt: int64Ptr(primaryReset), // 5h reset (earlier)
			SecondaryUsedPercent: "99", SecondaryResetAt: int64Ptr(weeklyReset), // weekly (later)
			UserID: "u1",
		}, true, defaultRotationThresholds())
		if got, ok := valueToInt64(updated["cooldownUntil"]); !ok || got != weeklyReset {
			t.Fatalf("cooldownUntil=%v, want %d", updated["cooldownUntil"], weeklyReset)
		}
	})
}

// TestSelectEligibleAlternateAccountAcrossModes pins the per-candidate
// rotation eligibility matrix:
//
//	toggle | shape        | threshold (primary) | threshold (secondary)
//	------ | ------------ | ------------------- | ---------------------
//	ON     | dual         | 80%                 | 98%
//	ON     | weekly-only  | 98%                 | n/a
//	OFF    | stale dual   | ignored             | 98%
//	OFF    | weekly-only  | 98%                 | n/a
//
// Dual detection uses BOTH secondaryUsedPercent AND secondaryResetAt so legacy
// entries written before new metadata was added still classify as dual.
func TestSelectEligibleAlternateAccountAcrossModes(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()

	// 5h ON + dual candidate: primary 85 >= 80% blocks, even when the secondary
	// weekly is comfortably below 98%. We need a SECOND eligible candidate to
	// verify rotation skipped the hot one.
	t.Run("5h on + dual blocks primary at 80% with future reset", func(t *testing.T) {
		t.Parallel()
		store := map[string]map[string]any{
			"user-current": {"user_id": "user-current", "access": "current-token"},
			"user-5h-hot": {
				"user_id": "user-5h-hot", "access": "tok",
				"usedPercent": "85", "resetAt": now + 3600,
				"secondaryUsedPercent": "50", "secondaryResetAt": now + 86400,
			},
			"user-ready": {"user_id": "user-ready", "access": "ready-token"},
		}
		selected, ok := selectEligibleAlternateAccount(store, "user-current", now, true, defaultFiveHourThreshold, defaultWeeklyThreshold)
		if !ok || selected["user_id"] != "user-ready" {
			t.Fatalf("expected user-ready (5h-hot dual blocked at 80), got %v ok=%v", selected["user_id"], ok)
		}
	})

	// 5h ON + dual candidate: secondary 99 >= 98% blocks, even when the primary
	// 5h is below 80%. The primary's 80% threshold is irrelevant when secondary
	// is exhausted.
	t.Run("5h on + dual blocks secondary at 98% with future reset", func(t *testing.T) {
		t.Parallel()
		store := map[string]map[string]any{
			"user-current": {"user_id": "user-current", "access": "current-token"},
			"user-weekly-hot": {
				"user_id": "user-weekly-hot", "access": "tok",
				"usedPercent": "50", "resetAt": now + 3600,
				"secondaryUsedPercent": "99", "secondaryResetAt": now + 86400,
			},
			"user-ready": {"user_id": "user-ready", "access": "ready-token"},
		}
		selected, ok := selectEligibleAlternateAccount(store, "user-current", now, true, defaultFiveHourThreshold, defaultWeeklyThreshold)
		if !ok || selected["user_id"] != "user-ready" {
			t.Fatalf("expected user-ready (weekly-hot dual blocked at 98), got %v ok=%v", selected["user_id"], ok)
		}
	})

	// 5h ON + weekly-only candidate: primary is treated as the weekly window
	// (98%), not as the 5h window (80%). 85% must therefore stay eligible.
	t.Run("5h on + weekly-only treats primary as weekly (85 below 98)", func(t *testing.T) {
		t.Parallel()
		store := map[string]map[string]any{
			"user-current": {"user_id": "user-current", "access": "current-token"},
			"user-85":      {"user_id": "user-85", "access": "tok", "usedPercent": "85", "resetAt": now + 3600},
		}
		selected, ok := selectEligibleAlternateAccount(store, "user-current", now, true, defaultFiveHourThreshold, defaultWeeklyThreshold)
		if !ok || selected["user_id"] != "user-85" {
			t.Fatalf("expected user-85 eligible (5h ON + weekly-only -> primary as weekly 98), got %v ok=%v", selected["user_id"], ok)
		}
	})

	// 5h OFF + stale dual: persisted when 5h was on. The stale 5h primary (85
	// above 80%) and stored cooldown must be IGNORED; only the persisted
	// secondary weekly (50, below 98%) matters. The candidate stays eligible.
	t.Run("5h off + stale dual ignores primary 5h and cooldown, secondary 50 below 98", func(t *testing.T) {
		t.Parallel()
		store := map[string]map[string]any{
			"user-current": {"user_id": "user-current", "access": "current-token"},
			"user-stale-dual": {
				"user_id": "user-stale-dual", "access": "tok",
				"usedPercent": "85", "resetAt": now + 3600, // stale 5h
				"cooldownUntil":        now + 3600, // might have been derived from primary
				"secondaryUsedPercent": "50", "secondaryResetAt": now + 86400,
			},
		}
		selected, ok := selectEligibleAlternateAccount(store, "user-current", now, false, defaultFiveHourThreshold, defaultWeeklyThreshold)
		if !ok || selected["user_id"] != "user-stale-dual" {
			t.Fatalf("expected user-stale-dual eligible (stale primary+cooldown ignored, secondary 50 below 98), got %v ok=%v", selected["user_id"], ok)
		}
	})

	// 5h OFF + stale dual: persisted when 5h was on. The secondary 99 is
	// exhausted with a future reset, so the candidate must be blocked even
	// though the stored primary 5h/cooldown are stale.
	t.Run("5h off + stale dual blocks on secondary 99 with future reset", func(t *testing.T) {
		t.Parallel()
		store := map[string]map[string]any{
			"user-current": {"user_id": "user-current", "access": "current-token"},
			"user-stale-dual-hot": {
				"user_id": "user-stale-dual-hot", "access": "tok",
				"usedPercent": "85", "resetAt": now + 3600, // stale 5h (ignored)
				"cooldownUntil":        now + 3600, // ignored under toggle OFF
				"secondaryUsedPercent": "99", "secondaryResetAt": now + 86400,
			},
			"user-ready": {"user_id": "user-ready", "access": "ready-token"},
		}
		selected, ok := selectEligibleAlternateAccount(store, "user-current", now, false, defaultFiveHourThreshold, defaultWeeklyThreshold)
		if !ok || selected["user_id"] != "user-ready" {
			t.Fatalf("expected user-ready (stale-dual-hot blocked on secondary 98), got %v ok=%v", selected["user_id"], ok)
		}
	})

	// 5h OFF + stale dual: missing secondary used_percent is conservative/
	// eligible. Only secondaryResetAt is persisted (legacy dual entry without
	// new metadata); the candidate must NOT be blocked.
	t.Run("5h off + stale dual with missing secondary usage stays eligible", func(t *testing.T) {
		t.Parallel()
		store := map[string]map[string]any{
			"user-current": {"user_id": "user-current", "access": "current-token"},
			"user-stale-dual-partial": {
				"user_id": "user-stale-dual-partial", "access": "tok",
				"usedPercent": "99", "resetAt": now + 3600, // stale 5h (ignored)
				"cooldownUntil":    now + 3600,  // ignored under toggle OFF
				"secondaryResetAt": now + 86400, // legacy: only the reset, no usage
			},
		}
		selected, ok := selectEligibleAlternateAccount(store, "user-current", now, false, defaultFiveHourThreshold, defaultWeeklyThreshold)
		if !ok || selected["user_id"] != "user-stale-dual-partial" {
			t.Fatalf("expected user-stale-dual-partial eligible (missing secondary usage is conservative), got %v ok=%v", selected["user_id"], ok)
		}
	})

	// Mixed persisted-mode: a dual candidate AND a weekly-only candidate in the
	// same store, evaluated under toggle ON. The dual one is blocked on its
	// primary 5h; the weekly-only one is evaluated against the weekly 98%.
	t.Run("5h on + mixed persisted-mode evaluates dual vs weekly-only per candidate", func(t *testing.T) {
		t.Parallel()
		store := map[string]map[string]any{
			"user-current": {"user_id": "user-current", "access": "current-token"},
			"user-dual-85": {
				"user_id": "user-dual-85", "access": "tok",
				"usedPercent": "85", "resetAt": now + 3600, // blocked at 80 on dual
				"secondaryUsedPercent": "40", "secondaryResetAt": now + 86400,
			},
			"user-weekly-95": {
				"user_id": "user-weekly-95", "access": "tok",
				"usedPercent": "95", "resetAt": now + 86400, // blocked at 98 on weekly-only
			},
			"user-weekly-50": {
				"user_id": "user-weekly-50", "access": "tok",
				"usedPercent": "50", "resetAt": now + 86400, // eligible weekly-only
			},
		}
		selected, ok := selectEligibleAlternateAccount(store, "user-current", now, true, defaultFiveHourThreshold, defaultWeeklyThreshold)
		if !ok || selected["user_id"] != "user-weekly-50" {
			t.Fatalf("expected user-weekly-50 (dual-85 blocked on 80, weekly-95 blocked on 98), got %v ok=%v", selected["user_id"], ok)
		}
	})
}

// TestRunRotationAcrossToggleModes exercises the dual-response rotation
// contract through runWithArgs: 5h on rotates at 80% on the 5h primary; 5h off
// ignores the 5h value entirely and only rotates at 98% on the promoted
// weekly secondary. The 5h-off-primary-only path is covered by the baseline
// tests (TestRunRotatesWhenWeeklyUsageExhausted and
// TestRunDoesNotRotateWhenWeeklyBelowThreshold).
func TestRunRotationAcrossToggleModes(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()
	primaryReset := now + 7200
	weeklyReset := now + 86400*5
	dual := func(p, s string) string {
		return `{"user_id":"user-current","rate_limit":{"primary_window":{"used_percent":` + p + `,"reset_at":` + strconv.FormatInt(primaryReset, 10) + `},"secondary_window":{"used_percent":` + s + `,"reset_at":` + strconv.FormatInt(weeklyReset, 10) + `}}}`
	}

	cases := []struct {
		name, body string
		fiveHour   bool
		stdout     string
		rotated    bool
	}{
		{"5h on rotates at 80% on the 5h primary (weekly 50 below 98)", dual("85", "50"), true, "85", true},
		{"5h off does NOT rotate on 85 (below weekly 98, even when 5h is high)", dual("85", "50"), false, "50", false},
		{"5h off rotates on weekly 99 promoted from secondary", dual("10", "99"), false, "99", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "config.json")
			if err := saveFiveHourConfig(cfgPath, tc.fiveHour); err != nil {
				t.Fatalf("save config: %v", err)
			}
			authFile := filepath.Join(dir, "auth.json")
			if err := os.WriteFile(authFile, []byte(`{"openai.access":"current-token","openai.accountId":"acct-current"}`), 0o600); err != nil {
				t.Fatalf("write auth: %v", err)
			}
			accountsFile := filepath.Join(dir, "accounts.json")
			store := map[string]map[string]any{
				"user-current": {"user_id": "user-current", "accountId": "acct-current", "access": "current-token"},
				"user-other":   {"user_id": "user-other", "accountId": "acct-other", "access": "other-token"},
			}
			encoded, _ := json.Marshal(store)
			if err := os.WriteFile(accountsFile, encoded, 0o600); err != nil {
				t.Fatalf("write accounts: %v", err)
			}

			var out strings.Builder
			if err := runWithArgs(context.Background(), config{
				AuthFile: authFile, AccountsFile: accountsFile, ConfigFile: cfgPath,
				UsageURL: server.URL, HTTPClient: server.Client(),
			}, &out, nil); err != nil {
				t.Fatalf("runWithArgs: %v", err)
			}
			if out.String() != tc.stdout {
				t.Fatalf("stdout=%q, want %q", out.String(), tc.stdout)
			}
			data, _ := os.ReadFile(authFile)
			gotOther := strings.Contains(string(data), "other-token")
			if gotOther != tc.rotated {
				t.Fatalf("rotation=%v, want %v (auth=%s)", gotOther, tc.rotated, string(data))
			}
		})
	}
}

// TestPrintAccountsTableModeDetection pins the table-mode contract: dual mode
// only triggers when at least one successful row exposes the secondary window.
// Error-only rows must NOT accidentally flip the renderer into dual mode; when
// dual mode is active, error rows render "-" for the missing secondary cells.
func TestPrintAccountsTableModeDetection(t *testing.T) {
	t.Parallel()

	dualRow := func() accountUsageRow {
		return accountUsageRow{
			Current: true, Index: 1, Email: "ok@example.com",
			UsedPercent: "35", SecondaryUsedPercent: "60",
			ResetDisplay: "2h 15m", SecondaryResetDisplay: "1d 3h",
			IsDualRow: true,
		}
	}
	weeklyRow := func(idx int, email, used, reset string) accountUsageRow {
		return accountUsageRow{Index: idx, Email: email, UsedPercent: used, ResetDisplay: reset}
	}
	errRow := func(idx int, email, msg string) accountUsageRow {
		return accountUsageRow{Index: idx, Email: email, UsedPercent: "ERR", ResetDisplay: "-", Err: errors.New(msg)}
	}
	mustContain := func(t *testing.T, printed string, wants []string) {
		t.Helper()
		for _, w := range wants {
			if !strings.Contains(printed, w) {
				t.Fatalf("expected %q in output, got %q", w, printed)
			}
		}
	}
	mustNotContain := func(t *testing.T, printed string, banned []string) {
		t.Helper()
		for _, b := range banned {
			if strings.Contains(printed, b) {
				t.Fatalf("did not expect %q in output, got %q", b, printed)
			}
		}
	}
	render := func(t *testing.T, rows []accountUsageRow) string {
		t.Helper()
		var out strings.Builder
		if err := printAccountsTable(&out, rows); err != nil {
			t.Fatalf("printAccountsTable: %v", err)
		}
		return out.String()
	}

	t.Run("dual mode when any successful row has secondary", func(t *testing.T) {
		t.Parallel()
		mustContain(t, render(t, []accountUsageRow{dualRow()}), []string{"USED%", "WEEK%", "RESET", "WEEK-RESET"})
	})

	t.Run("weekly-only when no successful row has secondary", func(t *testing.T) {
		t.Parallel()
		printed := render(t, []accountUsageRow{weeklyRow(1, "ok@example.com", "22.5", "2h 15m")})
		mustNotContain(t, printed, []string{"| USED% |"})
		mustContain(t, printed, []string{"| WEEK% |", "| WEEK-RESET |"})
	})

	t.Run("error-only rows do NOT force dual mode", func(t *testing.T) {
		t.Parallel()
		rows := []accountUsageRow{
			errRow(1, "broken@example.com", "missing access token"),
			errRow(2, "broken-too@example.com", "usage endpoint returned 401"),
		}
		printed := render(t, rows)
		mustNotContain(t, printed, []string{"| USED% |", "| RESET |"})
		mustContain(t, printed, []string{"[ERR: missing access token]", "[ERR: usage endpoint returned 401]"})
	})

	t.Run("error row in dual mode renders ERR in USED% and '-' in secondary cells", func(t *testing.T) {
		t.Parallel()
		rows := []accountUsageRow{dualRow(), errRow(2, "broken@example.com", "missing access token")}
		printed := render(t, rows)
		mustContain(t, printed, []string{"USED%", "WEEK%", "RESET", "WEEK-RESET"})
		errLine := lineContaining(printed, "broken@example.com")
		// ERR must be in the USED% column of the error row, not silently
		// dropped or moved into the weekly column. The exact cell widths are
		// not pinned; we only verify the line starts with `| 2 ` and contains
		// `ERR` followed by a `-`.
		if !strings.HasPrefix(errLine, "| 2 ") {
			t.Fatalf("expected error row to start with `| 2 `, got %q", errLine)
		}
		if !strings.Contains(errLine, "ERR") {
			t.Fatalf("expected ERR in error row, got %q", errLine)
		}
		if !strings.Contains(errLine, "-") {
			t.Fatalf("expected '-' in error row secondary cells, got %q", errLine)
		}
	})

	// Mixed dual/fallback: when one row is dual and another row is a fallback
	// weekly row (no secondary data), the fallback row must NOT be mislabeled
	// under USED%/RESET — it must render its weekly value under WEEK%/WEEK-RESET
	// with "-" under USED%/RESET.
	t.Run("fallback weekly row in dual mode renders '-' under USED%/RESET", func(t *testing.T) {
		t.Parallel()
		rows := []accountUsageRow{
			dualRow(), // index 1, dual: USED=35 WEEK=60
			{
				Index: 2, Email: "fallback@example.com",
				UsedPercent: "22.5", ResetDisplay: "2h 15m",
			},
		}
		printed := render(t, rows)
		mustContain(t, printed, []string{"USED%", "WEEK%", "RESET", "WEEK-RESET"})

		fallbackLine := lineContaining(printed, "fallback@example.com")
		if !strings.HasPrefix(fallbackLine, "| 2 ") {
			t.Fatalf("expected fallback row to start with `| 2 `, got %q", fallbackLine)
		}
		cells := splitTableRow(fallbackLine)
		if len(cells) != 7 {
			t.Fatalf("expected 7 cells in dual fallback row, got %d (%q)", len(cells), fallbackLine)
		}
		if cells[3] != "-" {
			t.Fatalf("expected fallback row USED%% cell to be \"-\", got %q (line=%q)", cells[3], fallbackLine)
		}
		if cells[4] != "22.5" {
			t.Fatalf("expected fallback row WEEK%% cell to be 22.5, got %q (line=%q)", cells[4], fallbackLine)
		}
		if cells[5] != "-" {
			t.Fatalf("expected fallback row RESET cell to be \"-\", got %q (line=%q)", cells[5], fallbackLine)
		}
		if cells[6] != "2h 15m" {
			t.Fatalf("expected fallback row WEEK-RESET cell to be 2h 15m, got %q (line=%q)", cells[6], fallbackLine)
		}
	})
}

// TestPersistOpenAIAccountDeletesStaleDerivedFields pins the deletion policy
// enforced by persistOpenAIAccount for usage-derived keys. Subtests pin:
//
//   - A dual record refreshed under toggle OFF (or promoted to weekly-only)
//     must drop secondaryUsedPercent, secondaryResetAt, and any stale dual
//     cooldownUntil while keeping the canonical weekly usedPercent/resetAt.
//   - An exhausted record refreshed below threshold must drop cooldownUntil.
//   - Auth and account metadata (access, refresh, accountId, email, user_id)
//     and arbitrary custom fields must always survive the merge.
//
// No tombstone/null artifact is written: omitted derived keys are deleted, not
// blanked.
func TestPersistOpenAIAccountDeletesStaleDerivedFields(t *testing.T) {
	t.Parallel()

	seedStore := func(t *testing.T, accountsFile string, seed map[string]map[string]any) {
		t.Helper()
		encoded, err := json.Marshal(seed)
		if err != nil {
			t.Fatalf("marshal seed store: %v", err)
		}
		if err := os.WriteFile(accountsFile, encoded, 0o600); err != nil {
			t.Fatalf("write seed store: %v", err)
		}
	}

	t.Run("dual refreshed under toggle OFF strips stale secondary and dual cooldown while keeping canonical weekly", func(t *testing.T) {
		t.Parallel()

		accountsFile := filepath.Join(t.TempDir(), "openai-accounts.json")
		weeklyReset := int64(1777619699)

		// Pre-seed a record captured while 5h was ON (dual response). It carries
		// secondary window values plus a stale dual cooldownUntil derived from
		// "both windows exhausted → take later reset".
		seedStore(t, accountsFile, map[string]map[string]any{
			"user-promote": {
				"user_id":              "user-promote",
				"accountId":            "acct-promote",
				"access":               "token-promote",
				"refresh":              "refresh-promote",
				"email":                "promote@example.com",
				"usedPercent":          "85",              // stale 5h primary
				"resetAt":              int64(1777014899), // stale 5h reset
				"secondaryUsedPercent": "99",              // weekly secondary
				"secondaryResetAt":     weeklyReset,       // weekly reset
				"cooldownUntil":        weeklyReset,       // stale dual cooldown
				"notes":                "preserve-me",
			},
		})

		// Refresh: toggle OFF promotes the weekly window into the canonical
		// primary fields. The incoming copy omits secondaryUsedPercent,
		// secondaryResetAt, and cooldownUntil because they no longer apply.
		if err := persistOpenAIAccount(accountsFile, map[string]any{
			"user_id":     "user-promote",
			"accountId":   "acct-promote",
			"access":      "token-promote",
			"refresh":     "refresh-promote",
			"email":       "promote@example.com",
			"usedPercent": "22",        // weekly primary (promoted)
			"resetAt":     weeklyReset, // weekly reset (promoted)
			"notes":       "preserve-me",
		}); err != nil {
			t.Fatalf("persistOpenAIAccount: %v", err)
		}

		entry := mustReadStore(t, accountsFile)["user-promote"]

		if _, has := entry["secondaryUsedPercent"]; has {
			t.Fatalf("expected secondaryUsedPercent to be deleted, got %v", entry["secondaryUsedPercent"])
		}
		if _, has := entry["secondaryResetAt"]; has {
			t.Fatalf("expected secondaryResetAt to be deleted, got %v", entry["secondaryResetAt"])
		}
		if _, has := entry["cooldownUntil"]; has {
			t.Fatalf("expected stale dual cooldownUntil to be deleted, got %v", entry["cooldownUntil"])
		}

		// Canonical weekly usage/reset must be the refreshed values, not the
		// stale 5h ones from the prior dual record.
		if got, _ := entry["usedPercent"].(string); got != "22" {
			t.Fatalf("expected canonical usedPercent 22, got %v", entry["usedPercent"])
		}
		if got, ok := valueToInt64(entry["resetAt"]); !ok || got != weeklyReset {
			t.Fatalf("expected canonical resetAt %d, got %v", weeklyReset, entry["resetAt"])
		}
	})

	t.Run("exhausted record refreshed below threshold drops stale cooldownUntil", func(t *testing.T) {
		t.Parallel()

		accountsFile := filepath.Join(t.TempDir(), "openai-accounts.json")
		weeklyReset := int64(1777014899)

		seedStore(t, accountsFile, map[string]map[string]any{
			"user-cooldown": {
				"user_id":       "user-cooldown",
				"accountId":     "acct-cooldown",
				"access":        "token-cooldown",
				"refresh":       "refresh-cooldown",
				"email":         "cooldown@example.com",
				"usedPercent":   "98", // exhausted on prior run
				"resetAt":       weeklyReset,
				"cooldownUntil": weeklyReset, // stale cooldown from prior run
			},
		})

		// Refresh below threshold. accountWithUsage would have omitted
		// cooldownUntil; the merge must honour that deletion.
		if err := persistOpenAIAccount(accountsFile, map[string]any{
			"user_id":     "user-cooldown",
			"accountId":   "acct-cooldown",
			"access":      "token-cooldown",
			"refresh":     "refresh-cooldown",
			"email":       "cooldown@example.com",
			"usedPercent": "35",
			"resetAt":     weeklyReset,
		}); err != nil {
			t.Fatalf("persistOpenAIAccount: %v", err)
		}

		entry := mustReadStore(t, accountsFile)["user-cooldown"]

		if _, has := entry["cooldownUntil"]; has {
			t.Fatalf("expected stale cooldownUntil to be deleted, got %v", entry["cooldownUntil"])
		}
		// usedPercent/resetAt must be the refreshed values.
		if got, _ := entry["usedPercent"].(string); got != "35" {
			t.Fatalf("expected usedPercent 35, got %v", entry["usedPercent"])
		}
		if got, ok := valueToInt64(entry["resetAt"]); !ok || got != weeklyReset {
			t.Fatalf("expected resetAt %d, got %v", weeklyReset, entry["resetAt"])
		}
	})

	t.Run("auth and custom metadata survive the merge while stale derived fields are dropped", func(t *testing.T) {
		t.Parallel()

		accountsFile := filepath.Join(t.TempDir(), "openai-accounts.json")

		// Pre-seed a record with stale derived fields AND custom metadata
		// that is not owned by accountWithUsage (region, notes, label).
		seedStore(t, accountsFile, map[string]map[string]any{
			"user-meta": {
				"user_id":              "user-meta",
				"accountId":            "acct-meta",
				"access":               "token-meta-old",
				"refresh":              "refresh-meta-old",
				"email":                "meta@example.com",
				"usedPercent":          "99",
				"resetAt":              int64(1777014899),
				"cooldownUntil":        int64(1777014899),
				"secondaryUsedPercent": "70",
				"secondaryResetAt":     int64(1777619699),
				"region":               "us-east",
				"notes":                "tagged",
				"label":                "primary",
			},
		})

		// Refresh: access/refresh are rotated, canonical weekly fields are
		// updated, but region/notes/label are NOT present in the incoming
		// copy. They must survive untouched alongside access/accountId/email/
		// user_id. Derived fields are intentionally omitted.
		if err := persistOpenAIAccount(accountsFile, map[string]any{
			"user_id":     "user-meta",
			"accountId":   "acct-meta",
			"access":      "token-meta-new",
			"refresh":     "refresh-meta-new",
			"email":       "meta@example.com",
			"usedPercent": "20",
			"resetAt":     int64(1777619699),
		}); err != nil {
			t.Fatalf("persistOpenAIAccount: %v", err)
		}

		entry := mustReadStore(t, accountsFile)["user-meta"]

		// Auth and account metadata: updated when present, preserved when not.
		if got, _ := entry["access"].(string); got != "token-meta-new" {
			t.Fatalf("expected access rotated to token-meta-new, got %q", got)
		}
		if got, _ := entry["refresh"].(string); got != "refresh-meta-new" {
			t.Fatalf("expected refresh rotated to refresh-meta-new, got %q", got)
		}
		if got, _ := entry["accountId"].(string); got != "acct-meta" {
			t.Fatalf("expected accountId preserved as acct-meta, got %q", got)
		}
		if got, _ := entry["email"].(string); got != "meta@example.com" {
			t.Fatalf("expected email preserved as meta@example.com, got %q", got)
		}
		if got, _ := entry["user_id"].(string); got != "user-meta" {
			t.Fatalf("expected user_id preserved as user-meta, got %q", got)
		}

		// Custom metadata: untouched by the merge.
		if got, _ := entry["region"].(string); got != "us-east" {
			t.Fatalf("expected custom field region preserved, got %q", got)
		}
		if got, _ := entry["notes"].(string); got != "tagged" {
			t.Fatalf("expected custom field notes preserved, got %q", got)
		}
		if got, _ := entry["label"].(string); got != "primary" {
			t.Fatalf("expected custom field label preserved, got %q", got)
		}

		// Stale derived fields: dropped.
		if _, has := entry["cooldownUntil"]; has {
			t.Fatalf("expected stale cooldownUntil to be deleted, got %v", entry["cooldownUntil"])
		}
		if _, has := entry["secondaryUsedPercent"]; has {
			t.Fatalf("expected stale secondaryUsedPercent to be deleted, got %v", entry["secondaryUsedPercent"])
		}
		if _, has := entry["secondaryResetAt"]; has {
			t.Fatalf("expected stale secondaryResetAt to be deleted, got %v", entry["secondaryResetAt"])
		}
	})
}

// Pi auth synchronization: selected account is mirrored into Pi's auth.json on
// switch (manual `use` or automatic rotation). Registration, listing and the
// default no-rotation run are read-only w.r.t. both auth files. Pi sync is
// opt-in via cfg.PiAuthFile.

func fixturePiAccount() map[string]any {
	return map[string]any{"user_id": "user-1", "accountId": "acct-1",
		"access": "fixture-access-token", "refresh": "fixture-refresh-token",
		"expires": int64(1777014899000), "type": "oauth", "email": "fixture@example.com"}
}

func seedAuthFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

func readJSONObject(t *testing.T, path string) map[string]any {
	t.Helper()
	data, _ := os.ReadFile(path)
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var payload map[string]any
	if err := dec.Decode(&payload); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return payload
}

func assertPiCodex(t *testing.T, payload map[string]any, access, refresh string, expires int64, accountID string) {
	t.Helper()
	c, ok := payload["openai-codex"].(map[string]any)
	if !ok || c["type"] != "oauth" || c["access"] != access || c["refresh"] != refresh {
		t.Fatalf("openai-codex mismatch: %#v", payload["openai-codex"])
	}
	switch v := c["expires"].(type) {
	case json.Number:
		if v.String() != fmt.Sprintf("%d", expires) {
			t.Fatalf("expires: want %d, got %s", expires, v.String())
		}
	case float64:
		if int64(v) != expires {
			t.Fatalf("expires: want %d, got %v", expires, v)
		}
	default:
		t.Fatalf("expires type %T", c["expires"])
	}
	if accountID == "" {
		if _, present := c["accountId"]; present {
			t.Fatalf("accountId must be omitted")
		}
	} else if c["accountId"] != accountID {
		t.Fatalf("accountId: want %q, got %#v", accountID, c["accountId"])
	}
}

func piSyncFixture(t *testing.T) (oc, pi, acc string) {
	d := t.TempDir()
	return filepath.Join(d, "opencode.json"), filepath.Join(d, "pi.json"), filepath.Join(d, "accounts.json")
}

func containsAny(v any, needle string) bool {
	switch val := v.(type) {
	case string:
		return strings.Contains(val, needle)
	case map[string]any:
		for k, item := range val {
			if strings.Contains(k, needle) || containsAny(item, needle) {
				return true
			}
		}
	case []any:
		for _, item := range val {
			if containsAny(item, needle) {
				return true
			}
		}
	}
	return false
}

func writeStore(t *testing.T, accounts string, entries ...map[string]any) {
	t.Helper()
	store := map[string]map[string]any{}
	for _, e := range entries {
		store[e["user_id"].(string)] = e
	}
	encoded, _ := json.Marshal(store)
	if err := os.WriteFile(accounts, encoded, 0o600); err != nil {
		t.Fatalf("write accounts: %v", err)
	}
}

func TestParseExactInt64(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		value   any
		want    int64
		wantErr bool
	}{
		// Accepted: mathematically integral, in int64 range.
		{"json.Number integer", json.Number("1777014899000"), 1777014899000, false},
		{"json.Number exponent", json.Number("1e3"), 1000, false},
		{"json.Number .0 form", json.Number("100.0"), 100, false},
		{"json.Number int64 max", json.Number("9223372036854775807"), math.MaxInt64, false},
		{"float64 exact integer", float64(1777014899000), 1777014899000, false},
		{"int64", int64(math.MaxInt64), math.MaxInt64, false},
		{"uint64 in range", uint64(1777014899000), 1777014899000, false},
		// Rejected: no truncation, no rounding.
		{"json.Number fraction", json.Number("1.5"), 0, true},
		{"json.Number high precision fraction", json.Number("1." + strings.Repeat("0", 200) + "1"), 0, true},
		{"json.Number overflow", json.Number("9223372036854775808"), 0, true},
		{"json.Number negative overflow", json.Number("-9223372036854775809"), 0, true},
		{"float64 fraction", 1.5, 0, true},
		{"float64 NaN", math.NaN(), 0, true},
		{"float64 +Inf", math.Inf(1), 0, true},
		{"float64 overflow", math.MaxInt64 + 1.0, 0, true},
		{"string rejected", "x", 0, true},
		{"nil rejected", nil, 0, true},
	}
	for _, tt := range cases {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseExactInt64(tt.value)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseExactInt64(%v): expected error", tt.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseExactInt64(%v): %v", tt.value, err)
			}
			if got != tt.want {
				t.Fatalf("parseExactInt64(%v) = %d, want %d", tt.value, got, tt.want)
			}
		})
	}
}

func TestBuildPiCredential(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(map[string]any)
		wantErr string
	}{
		{"legacy missing type normalized to oauth", func(a map[string]any) { delete(a, "type") }, ""},
		{"non-oauth type rejected", func(a map[string]any) { a["type"] = "api_key" }, "type"},
		{"non-string type rejected", func(a map[string]any) { a["type"] = 42 }, "type"},
		{"missing access named (no value leak)", func(a map[string]any) { delete(a, "access") }, "access"},
		{"missing refresh named", func(a map[string]any) { delete(a, "refresh") }, "refresh"},
		{"missing expires named", func(a map[string]any) { delete(a, "expires") }, "expires"},
		{"fractional expires rejected", func(a map[string]any) { a["expires"] = 1.5 }, "expires"},
		{"string expires rejected", func(a map[string]any) { a["expires"] = "x" }, "expires"},
		{"json.Number integer expires parses", func(a map[string]any) { a["expires"] = json.Number("1777014899000") }, ""},
		{"empty accountId omitted", func(a map[string]any) { a["accountId"] = "" }, ""},
	}
	for _, tt := range cases {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			acct := fixturePiAccount()
			if tt.mutate != nil {
				tt.mutate(acct)
			}
			cred, err := buildPiCredential(acct)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
				}
				for _, leak := range []string{"fixture-access-token", "fixture-refresh-token", "acct-1", "fixture@example.com"} {
					if strings.Contains(err.Error(), leak) {
						t.Fatalf("error leaked %q: %v", leak, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cred.Type != "oauth" || cred.Access == "" || cred.Refresh == "" || cred.Expires == 0 {
				t.Fatalf("incomplete credential: %+v", cred)
			}
		})
	}
}

func TestUpdatePiAuthFile(t *testing.T) {
	t.Parallel()
	cred := PiAuthEntry{Type: "oauth", Access: "new-access", Refresh: "new-refresh", Expires: int64(1777014899000), AccountID: "new-acct"}

	t.Run("missing file initializes to {}; ensurePiAuthFile does not clobber existing (O_EXCL)", func(t *testing.T) {
		t.Parallel()
		missing := filepath.Join(t.TempDir(), "auth.json")
		if err := updatePiAuthFile(missing, cred); err != nil {
			t.Fatalf("updatePiAuthFile: %v", err)
		}
		assertPiCodex(t, readJSONObject(t, missing), "new-access", "new-refresh", 1777014899000, "new-acct")
		existing := filepath.Join(t.TempDir(), "auth.json")
		keep := `{"keep":"original"}`
		seedAuthFile(t, existing, keep)
		if err := ensurePiAuthFile(existing); err != nil {
			t.Fatalf("ensurePiAuthFile: %v", err)
		}
		if data, _ := os.ReadFile(existing); string(data) != keep {
			t.Fatalf("ensurePiAuthFile clobbered existing file: %q", string(data))
		}
	})

	t.Run("replaces codex and preserves unrelated providers", func(t *testing.T) {
		t.Parallel()
		piFile := filepath.Join(t.TempDir(), "auth.json")
		seedAuthFile(t, piFile, `{"openai-codex":{"type":"oauth","access":"old","refresh":"old","expires":100,"accountId":"old"},`+
			`"anthropic":{"access":"anth","refresh":"anth-r"},"google":{"nested":{"deep":"value"},"list":["a","b"]}}`)
		if err := updatePiAuthFile(piFile, cred); err != nil {
			t.Fatalf("updatePiAuthFile: %v", err)
		}
		if data, _ := os.ReadFile(piFile); strings.Contains(string(data), `"old"`) || strings.Contains(string(data), `"old-acct"`) {
			t.Fatalf("old openai-codex value leaked: %s", string(data))
		}
		payload := readJSONObject(t, piFile)
		assertPiCodex(t, payload, "new-access", "new-refresh", 1777014899000, "new-acct")
		if anth := payload["anthropic"].(map[string]any); anth["access"] != "anth" || anth["refresh"] != "anth-r" {
			t.Fatalf("anthropic altered: %v", anth)
		}
		if !containsAny(payload["google"], "deep") || !containsAny(payload["google"], "list") {
			t.Fatalf("google provider altered: %v", payload["google"])
		}
	})

	t.Run("malformed/empty/non-object/trailing Pi JSON is rejected without overwrite", func(t *testing.T) {
		t.Parallel()
		seeds := map[string]string{
			"malformed": "{not valid json", "empty": "", "non-object": `[{"openai-codex":{}}]`,
			"trailing": `{} {}`, "trailing2": `{"a":1} junk`,
		}
		for name, seed := range seeds {
			name, seed := name, seed
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				piFile := filepath.Join(t.TempDir(), "auth.json")
				seedAuthFile(t, piFile, seed)
				if err := updatePiAuthFile(piFile, cred); err == nil {
					t.Fatalf("expected error for %s file", name)
				}
				if data, _ := os.ReadFile(piFile); string(data) != seed {
					t.Fatalf("%s file overwritten: %q", name, string(data))
				}
			})
		}
	})

	t.Run("large provider integers survive round-trip via UseNumber", func(t *testing.T) {
		t.Parallel()
		piFile := filepath.Join(t.TempDir(), "auth.json")
		seedAuthFile(t, piFile, `{"bigint":{"id":9223372036854775807,"neg":-9223372036854775808,"list":[9223372036854775806,9223372036854775807]}}`)
		if err := updatePiAuthFile(piFile, cred); err != nil {
			t.Fatalf("updatePiAuthFile: %v", err)
		}
		data, _ := os.ReadFile(piFile)
		for _, want := range []string{"9223372036854775807", "-9223372036854775808", "9223372036854775806"} {
			if !strings.Contains(string(data), want) {
				t.Fatalf("bigint literal %s lost on round-trip: %s", want, string(data))
			}
		}
	})

	if runtime.GOOS != "windows" {
		t.Run("created parent dir is 0700 on POSIX", func(t *testing.T) {
			t.Parallel()
			nested := filepath.Join(t.TempDir(), "nested", "auth.json")
			if err := updatePiAuthFile(nested, cred); err != nil {
				t.Fatalf("updatePiAuthFile: %v", err)
			}
			if info, _ := os.Stat(filepath.Dir(nested)); info.Mode().Perm() != 0o700 {
				t.Fatalf("created dir perm: got %o, want 0700", info.Mode().Perm())
			}
		})
	}

	t.Run("legacy source missing type produces type:oauth; empty accountId omitted", func(t *testing.T) {
		t.Parallel()
		piFile := filepath.Join(t.TempDir(), "auth.json")
		acct := fixturePiAccount()
		delete(acct, "accountId")
		c, err := buildPiCredential(acct)
		if err != nil {
			t.Fatalf("buildPiCredential: %v", err)
		}
		if err := updatePiAuthFile(piFile, c); err != nil {
			t.Fatalf("updatePiAuthFile: %v", err)
		}
		js := string(mustReadFile(t, piFile))
		if !strings.Contains(js, `"type":"oauth"`) || strings.Contains(js, `"accountId"`) {
			t.Fatalf("type/oauth or accountId leak: %s", js)
		}
	})
}

func TestPiAuthLockSemantics(t *testing.T) {
	t.Parallel()
	t.Run("pre-existing lock blocks update; lock not deleted", func(t *testing.T) {
		t.Parallel()
		piFile := filepath.Join(t.TempDir(), "auth.json")
		seedAuthFile(t, piFile, `{"keep":"original"}`)
		lockPath := piFile + ".lock"
		if err := os.Mkdir(lockPath, 0o700); err != nil {
			t.Fatalf("seed lock dir: %v", err)
		}
		err := updatePiAuthFile(piFile, PiAuthEntry{Type: "oauth", Access: "new", Refresh: "r", Expires: 1})
		if err == nil || !strings.Contains(err.Error(), "lock") {
			t.Fatalf("expected lock error, got %v", err)
		}
		if _, statErr := os.Stat(lockPath); statErr != nil {
			t.Fatalf("lock dir was deleted speculatively: %v", statErr)
		}
		if data, _ := os.ReadFile(piFile); string(data) != `{"keep":"original"}` {
			t.Fatalf("auth file changed despite lock: %q", string(data))
		}
	})
	t.Run("successful update removes its own lock directory", func(t *testing.T) {
		t.Parallel()
		piFile := filepath.Join(t.TempDir(), "auth.json")
		seedAuthFile(t, piFile, `{}`)
		if err := updatePiAuthFile(piFile, PiAuthEntry{Type: "oauth", Access: "a", Refresh: "r", Expires: 1}); err != nil {
			t.Fatalf("updatePiAuthFile: %v", err)
		}
		if _, err := os.Stat(piFile + ".lock"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("lock dir not removed after success: %v", err)
		}
	})
	t.Run("release ownership loss and combined failures are surfaced", func(t *testing.T) {
		t.Parallel()
		if err := releasePiLock(filepath.Join(t.TempDir(), "missing.lock")); err == nil {
			t.Fatal("expected missing lock release to fail")
		}
		writeErr := errors.New("write failed")
		releaseErr := errors.New("release failed")
		combined := combinePiAuthWriteAndReleaseErrors(writeErr, releaseErr)
		if !errors.Is(combined, writeErr) || !errors.Is(combined, releaseErr) {
			t.Fatalf("combined error must retain both causes: %v", combined)
		}
	})
}

func TestUpdateOpenAIAuthFile(t *testing.T) {
	t.Parallel()
	fullOAuth := func(access, refresh string, expires int64) map[string]any {
		return map[string]any{"access": access, "accountId": access + "-acct", "refresh": refresh, "expires": expires, "type": "oauth"}
	}
	cases := []struct {
		name       string
		seed       string
		account    map[string]any
		keep       []string
		wantFlat   map[string]any
		wantNested map[string]any
	}{
		{
			name:       "flat and nested tuples updated for full OAuth source",
			seed:       `{"openai.access":"old","openai.accountId":"old-acct","openai.refresh":"old-r","openai.expires":1,"openai.type":"oauth","openai":{"access":"old","accountId":"old-acct","refresh":"old-r","expires":1,"type":"oauth"},"anthropic":{"k":"v"}}`,
			account:    fullOAuth("new", "new-r", 1777014899000),
			keep:       []string{"anthropic"},
			wantFlat:   map[string]any{"openai.access": "new", "openai.accountId": "new-acct", "openai.refresh": "new-r", "openai.expires": json.Number("1777014899000"), "openai.type": "oauth"},
			wantNested: map[string]any{"access": "new", "accountId": "new-acct", "refresh": "new-r", "expires": json.Number("1777014899000"), "type": "oauth"},
		},
		{
			name:     "nested object absent from file is not invented; absent source fields are removed",
			seed:     `{"openai.access":"old","openai.accountId":"old-acct","openai.refresh":"old-r","openai.expires":1,"openai.type":"oauth","other":"keep"}`,
			account:  map[string]any{"access": "new"},
			keep:     []string{"other"},
			wantFlat: map[string]any{"openai.access": "new"},
		},
		{
			name:       "unrelated providers and custom openai.* keys are preserved",
			seed:       `{"openai.access":"old","openai.workspaceId":"ws-1","openai.notes":"keep","openai":{"access":"old","workspaceId":"ws-1"},"anthropic":{"k":"v"}}`,
			account:    fullOAuth("new", "new-r", 3),
			keep:       []string{"anthropic", "openai.workspaceId", "openai.notes", "ws-1"},
			wantFlat:   map[string]any{"openai.access": "new", "openai.accountId": "new-acct", "openai.refresh": "new-r", "openai.expires": json.Number("3"), "openai.type": "oauth"},
			wantNested: map[string]any{"access": "new", "accountId": "new-acct", "refresh": "new-r", "expires": json.Number("3"), "type": "oauth"},
		},
	}
	for _, tt := range cases {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			authFile := filepath.Join(t.TempDir(), "auth.json")
			seedAuthFile(t, authFile, tt.seed)
			if err := updateOpenAIAuthFile(authFile, tt.account); err != nil {
				t.Fatalf("updateOpenAIAuthFile: %v", err)
			}
			payload := readJSONObject(t, authFile)
			for _, k := range tt.keep {
				if !containsAny(payload, k) {
					t.Fatalf("key/value %q dropped: %v", k, payload)
				}
			}
			for k, want := range tt.wantFlat {
				if got := payload[k]; got != want {
					t.Fatalf("flat %s: got %#v, want %#v", k, got, want)
				}
			}
			if tt.wantNested != nil {
				nested, ok := payload["openai"].(map[string]any)
				if !ok {
					t.Fatalf("nested openai map missing")
				}
				for k, want := range tt.wantNested {
					if got := nested[k]; got != want {
						t.Fatalf("nested %s: got %#v, want %#v", k, got, want)
					}
				}
			}
		})
	}
}

func TestActivateAccountAndRuntimeFlow(t *testing.T) {
	t.Parallel()
	expiry := int64(1777014899000)

	t.Run("validation failure leaves both auth files unchanged", func(t *testing.T) {
		t.Parallel()
		oc, pi, _ := piSyncFixture(t)
		ocSeed := `{"openai.access":"keep-opencode","other":"keep"}`
		piSeed := `{"openai-codex":{"access":"keep-pi"},"anthropic":{"k":"v"}}`
		seedAuthFile(t, oc, ocSeed)
		seedAuthFile(t, pi, piSeed)
		err := activateAccount(config{AuthFile: oc, PiAuthFile: pi}, map[string]any{"user_id": "user-1", "accountId": "acct-1", "access": "x"})
		if err == nil {
			t.Fatalf("expected validation error")
		}
		for _, want := range []string{"refresh", "expires"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error should name %q, got %v", want, err)
			}
		}
		for _, leak := range []string{"keep-opencode", "keep-pi", "fixture"} {
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("error leaked %q: %v", leak, err)
			}
		}
		for _, p := range []struct{ path, seed string }{{oc, ocSeed}, {pi, piSeed}} {
			if data, _ := os.ReadFile(p.path); string(data) != p.seed {
				t.Fatalf("%s altered: %s", p.path, string(data))
			}
		}
	})

	t.Run("successful activation updates both auth files coherently", func(t *testing.T) {
		t.Parallel()
		oc, pi, _ := piSyncFixture(t)
		seedAuthFile(t, oc, `{"other":"keep"}`)
		seedAuthFile(t, pi, `{"anthropic":{"access":"anth"}}`)
		if err := activateAccount(config{AuthFile: oc, PiAuthFile: pi}, fixturePiAccount()); err != nil {
			t.Fatalf("activateAccount: %v", err)
		}
		ocP := readJSONObject(t, oc)
		if ocP["openai.access"] != "fixture-access-token" || ocP["openai.expires"] != json.Number("1777014899000") || ocP["openai.type"] != "oauth" {
			t.Fatalf("OpenCode tuple not updated: %v", ocP)
		}
		assertPiCodex(t, readJSONObject(t, pi), "fixture-access-token", "fixture-refresh-token", 1777014899000, "acct-1")
	})

	t.Run("empty PiAuthFile skips Pi sync; partial-sync error names both stores", func(t *testing.T) {
		t.Parallel()
		oc, pi, _ := piSyncFixture(t)
		piSeed := `{"openai-codex":{"access":"keep-pi"}}`
		seedAuthFile(t, oc, `{}`)
		seedAuthFile(t, pi, piSeed)
		if err := activateAccount(config{AuthFile: oc, PiAuthFile: ""}, fixturePiAccount()); err != nil {
			t.Fatalf("activateAccount: %v", err)
		}
		if data, _ := os.ReadFile(pi); string(data) != piSeed {
			t.Fatalf("Pi altered despite empty PiAuthFile: %s", string(data))
		}
		piAsDir := filepath.Join(filepath.Dir(oc), "pi-as-dir")
		_ = os.MkdirAll(piAsDir, 0o700)
		seedAuthFile(t, oc, `{}`)
		err := activateAccount(config{AuthFile: oc, PiAuthFile: piAsDir}, fixturePiAccount())
		if err == nil {
			t.Fatalf("expected partial-sync error")
		}
		for _, want := range []string{"OpenCode", "Pi"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("partial-sync error should mention %q, got %v", want, err)
			}
		}
		for _, leak := range []string{"fixture-access-token", "fixture-refresh-token", "acct-1"} {
			if strings.Contains(err.Error(), leak) {
				t.Fatalf("partial-sync error leaked %q: %v", leak, err)
			}
		}
	})

	t.Run("`use` rotates both auth files; manual change preserves unrelated providers", func(t *testing.T) {
		t.Parallel()
		oc, pi, accounts := piSyncFixture(t)
		seedAuthFile(t, oc, `{"openai.access":"active-access","openai.accountId":"active-acct","other":"keep"}`)
		seedAuthFile(t, pi, `{"anthropic":{"access":"anth"}}`)
		writeStore(t, accounts,
			map[string]any{"user_id": "user-active", "accountId": "active-acct", "access": "active-access", "refresh": "active-refresh", "expires": expiry, "type": "oauth", "email": "active@example.com"},
			map[string]any{"user_id": "user-other", "accountId": "other-acct", "access": "other-access", "refresh": "other-refresh", "expires": expiry, "type": "oauth", "email": "other@example.com"},
		)
		var out strings.Builder
		err := runWithArgs(context.Background(), config{
			AuthFile: oc, PiAuthFile: pi, AccountsFile: accounts,
			UsageURL: "http://unused.local", HTTPClient: http.DefaultClient,
		}, &out, []string{"use", "other@example.com"})
		if err != nil {
			t.Fatalf("use: %v", err)
		}
		if strings.Contains(out.String(), "other-access") || strings.Contains(out.String(), "other-refresh") {
			t.Fatalf("stdout leaked credential: %q", out.String())
		}
		ocP := readJSONObject(t, oc)
		if ocP["openai.access"] != "other-access" || ocP["other"] != "keep" {
			t.Fatalf("OpenCode not rotated: %v", ocP)
		}
		assertPiCodex(t, readJSONObject(t, pi), "other-access", "other-refresh", expiry, "other-acct")
	})

	t.Run("automatic rotation syncs the rotated account to Pi", func(t *testing.T) {
		t.Parallel()
		body := fmt.Sprintf(`{"user_id":"user-current","rate_limit":{"primary_window":{"used_percent":99,"reset_at":%d}}}`, time.Now().Unix()+86400*5)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
		defer server.Close()
		oc, pi, accounts := piSyncFixture(t)
		seedAuthFile(t, oc, `{"openai.access":"current-access","openai.accountId":"current-acct"}`)
		seedAuthFile(t, pi, `{"anthropic":{"k":"v"}}`)
		writeStore(t, accounts,
			map[string]any{"user_id": "user-current", "access": "current-access", "accountId": "current-acct"},
			map[string]any{"user_id": "user-other", "access": "other-access", "accountId": "other-acct", "refresh": "other-refresh", "expires": expiry, "type": "oauth", "email": "other@example.com"},
		)
		var out strings.Builder
		if err := runWithArgs(context.Background(), config{
			AuthFile: oc, PiAuthFile: pi, AccountsFile: accounts,
			UsageURL: server.URL, HTTPClient: server.Client(),
		}, &out, nil); err != nil {
			t.Fatalf("runWithArgs: %v", err)
		}
		if !strings.Contains(out.String(), "99") {
			t.Fatalf("expected stdout 99, got %q", out.String())
		}
		piP := readJSONObject(t, pi)
		assertPiCodex(t, piP, "other-access", "other-refresh", expiry, "other-acct")
		if _, present := piP["anthropic"]; !present {
			t.Fatalf("Pi unrelated provider dropped after rotation")
		}
	})

	t.Run("registration and listing leave both auth files untouched", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"user_id":"user-1","email":"u@example.com","rate_limit":{"primary_window":{"used_percent":50,"reset_at":1777014899}}}`))
		}))
		defer server.Close()
		oc, pi, accounts := piSyncFixture(t)
		seedAuthFile(t, oc, `{"openai.access":"active-access","openai.refresh":"active-refresh","openai.expires":1777014899000,"openai.accountId":"active-acct"}`)
		piSeed := `{"openai-codex":{"access":"keep-pi","refresh":"keep-pi-r","expires":100,"accountId":"keep-pi-acct"},"anthropic":{"k":"v"}}`
		seedAuthFile(t, pi, piSeed)
		cfg := config{AuthFile: oc, PiAuthFile: pi, AccountsFile: accounts, UsageURL: server.URL, HTTPClient: server.Client()}
		for _, sub := range []struct {
			name string
			args []string
		}{{"registration", nil}, {"listing", []string{"accounts"}}} {
			var out strings.Builder
			if err := runWithArgs(context.Background(), cfg, &out, sub.args); err != nil {
				t.Fatalf("runWithArgs %s: %v", sub.name, err)
			}
			if data, _ := os.ReadFile(pi); string(data) != piSeed {
				t.Fatalf("Pi altered during %s: %s", sub.name, string(data))
			}
		}
	})
}

func TestDefaultConfigResolvesPiAuthFile(t *testing.T) {
	fakeHome := t.TempDir()
	customDir := t.TempDir()
	cases := []struct {
		name string
		env  string
		want string
	}{
		{"default home expands to ~/.pi/agent/auth.json", "", filepath.Join(fakeHome, ".pi", "agent", "auth.json")},
		{"PI_CODING_AGENT_DIR overrides directory and appends auth.json", customDir, filepath.Join(customDir, "auth.json")},
		{"tilde (separator form) expands to home directory", "~" + string(filepath.Separator) + "my-pi", filepath.Join(fakeHome, "my-pi", "auth.json")},
		{"bare tilde expands to home and still appends auth.json", "~", filepath.Join(fakeHome, "auth.json")},
	}
	for _, tt := range cases {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PI_CODING_AGENT_DIR", tt.env)
			t.Setenv("HOME", fakeHome)
			t.Setenv("USERPROFILE", fakeHome)
			if cfg := defaultConfig(); cfg.PiAuthFile != tt.want {
				t.Fatalf("expected PiAuthFile %q, got %q", tt.want, cfg.PiAuthFile)
			}
		})
	}
}

type piResponses map[string]string

func newPiServer(t *testing.T, r piResponses) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n.Add(1)
		if body, ok := r[strings.TrimSpace(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))]; ok {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return
		}
		http.Error(w, "unknown token", http.StatusUnauthorized)
	}))
	t.Cleanup(s.Close)
	return s, &n
}

func piPayload(userID, used, weekly string) string {
	if weekly == "" {
		return fmt.Sprintf(`{"user_id":%q,"email":%q,"rate_limit":{"primary_window":{"used_percent":%s,"reset_at":1777414800}}}`, userID, userID+"@example.com", used)
	}
	return fmt.Sprintf(`{"user_id":%q,"email":%q,"rate_limit":{"primary_window":{"used_percent":%s,"reset_at":1777414800},"secondary_window":{"used_percent":%s,"reset_at":1778203200}}}`, userID, userID+"@example.com", used, weekly)
}

func ocAuthBody(access, refresh string, expires int64, acctID string) string {
	if acctID == "" {
		return fmt.Sprintf(`{"openai.access":%q,"openai.refresh":%q,"openai.expires":%d,"openai.type":"oauth"}`, access, refresh, expires)
	}
	return fmt.Sprintf(`{"openai.access":%q,"openai.refresh":%q,"openai.expires":%d,"openai.accountId":%q,"openai.type":"oauth"}`, access, refresh, expires, acctID)
}

func piAuthBody(access, refresh string, expires int64, acctID string) string {
	if acctID == "" {
		return fmt.Sprintf(`{"openai-codex":{"type":"oauth","access":%q,"refresh":%q,"expires":%d}}`, access, refresh, expires)
	}
	return fmt.Sprintf(`{"openai-codex":{"type":"oauth","access":%q,"refresh":%q,"expires":%d,"accountId":%q}}`, access, refresh, expires, acctID)
}

func assertOCAuth(t *testing.T, path, wantAccess, wantRefresh string, wantExpires int64, wantAcctID string) {
	t.Helper()
	p := readJSONObject(t, path)
	if p["openai.access"] != wantAccess || p["openai.refresh"] != wantRefresh || p["openai.type"] != "oauth" {
		t.Fatalf("openai oauth mismatch: %#v", p)
	}
	if wantExpires == 0 {
		if _, has := p["openai.expires"]; has {
			t.Fatalf("openai.expires should be absent")
		}
	} else if got, ok := p["openai.expires"].(json.Number); !ok || got.String() != strconv.FormatInt(wantExpires, 10) {
		t.Fatalf("openai.expires: got %v, want %d", p["openai.expires"], wantExpires)
	}
	if wantAcctID == "" {
		if _, has := p["openai.accountId"]; has {
			t.Fatalf("openai.accountId should be absent")
		}
	} else if p["openai.accountId"] != wantAcctID {
		t.Fatalf("openai.accountId: got %v, want %q", p["openai.accountId"], wantAcctID)
	}
}

func storeInt64Must(t *testing.T, m map[string]any, key string) int64 {
	t.Helper()
	v, ok := valueToInt64(m[key])
	if !ok {
		t.Fatalf("%s not int64: %#v", key, m[key])
	}
	return v
}

func TestReadPiAuthUnderLock(t *testing.T) {
	t.Parallel()
	t.Run("missing or empty path returns errPiAuthMissing", func(t *testing.T) {
		t.Parallel()
		nested := filepath.Join(t.TempDir(), "nested", "auth.json")
		for _, p := range []string{"", nested} {
			if _, err := readPiAuthUnderLock(p); !errors.Is(err, errPiAuthMissing) {
				t.Fatalf("%q: got %v", p, err)
			}
		}
		for _, p := range []string{nested, nested + ".lock", filepath.Dir(nested)} {
			if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s created: %v", p, err)
			}
		}
	})
	t.Run("valid round-trips and releases lock", func(t *testing.T) {
		t.Parallel()
		f := filepath.Join(t.TempDir(), "auth.json")
		seedAuthFile(t, f, `{"openai-codex":{"type":"oauth","access":"a","refresh":"r","expires":42}}`)
		p, err := readPiAuthUnderLock(f)
		if err != nil || p["openai-codex"] == nil {
			t.Fatalf("err=%v payload=%v", err, p)
		}
		if _, err := os.Stat(f + ".lock"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("lock not released: %v", err)
		}
	})
	t.Run("directory path rejected", func(t *testing.T) {
		t.Parallel()
		if _, err := readPiAuthUnderLock(t.TempDir()); err == nil {
			t.Fatal("want error for directory path")
		}
	})
	t.Run("malformed/empty/non-object rejected as non-missing", func(t *testing.T) {
		t.Parallel()
		for _, s := range []string{"{not valid", "", `[{"openai-codex":{}}]`, "{} {}"} {
			s := s
			t.Run(s, func(t *testing.T) {
				t.Parallel()
				f := filepath.Join(t.TempDir(), "auth.json")
				seedAuthFile(t, f, s)
				_, err := readPiAuthUnderLock(f)
				if err == nil || errors.Is(err, errPiAuthMissing) {
					t.Fatalf("want non-missing error, got %v", err)
				}
				if data, _ := os.ReadFile(f); string(data) != s {
					t.Fatalf("file mutated: %q", string(data))
				}
				if _, err := os.Stat(f + ".lock"); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stale lock: %v", err)
				}
			})
		}
	})
	t.Run("release failure joins combined error (POSIX)", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("os.Remove of non-empty dir only fails on POSIX")
		}
		t.Parallel()
		piFile := filepath.Join(t.TempDir(), "auth.json")
		seedAuthFile(t, piFile, `{"openai-codex":{"type":"oauth","access":"a","refresh":"r","expires":1}}`)
		lock := piFile + ".lock"
		if err := acquirePiLock(lock); err != nil {
			t.Fatalf("acquirePiLock: %v", err)
		}
		if err := os.WriteFile(filepath.Join(lock, "stub"), []byte("x"), 0o600); err != nil {
			t.Fatalf("populate: %v", err)
		}
		releaseErr := releasePiLock(lock)
		if releaseErr == nil {
			t.Fatal("want release failure")
		}
		combined := combinePiAuthWriteAndReleaseErrors(errors.New("read"), releaseErr)
		if !strings.Contains(combined.Error(), "read") || !strings.Contains(combined.Error(), releaseErr.Error()) {
			t.Fatalf("combined must join both: %v", combined)
		}
	})
}

func TestReconcilePiInbound(t *testing.T) {
	t.Parallel()
	const (
		ocAccess, ocRefresh, piAccess           = "oc-access", "oc-refresh", "pi-access"
		piRefresh, piAcctID, curUser, otherUser = "pi-refresh", "pi-acct", "user-cur", "user-other"
		oldExpires, newExpires                  = int64(1776000000000), int64(1778000000000)
	)
	savedUser := func(userID, access, refresh string, expires int64, email string) map[string]any {
		e := map[string]any{"user_id": userID, "access": access, "refresh": refresh, "expires": expires, "type": "oauth"}
		if email != "" {
			e["email"] = email
		}
		return e
	}
	runCase := func(t *testing.T, ocExpires int64, store map[string]map[string]any, piSeed, piBodyStr string, omitOc bool) (bool, int32, string, string) {
		t.Helper()
		authFile, piAuthFile, accountsFile := piSyncFixture(t)
		seedAuthFile(t, authFile, ocAuthBody(ocAccess, ocRefresh, ocExpires, "oc-acct"))
		seedAuthFile(t, piAuthFile, piSeed)
		data, _ := json.Marshal(store)
		if err := os.WriteFile(accountsFile, data, 0o600); err != nil {
			t.Fatalf("write store: %v", err)
		}
		responses := piResponses{ocAccess: piPayload(curUser, "55", "")}
		if piBodyStr != "" {
			responses[piAccess] = piBodyStr
		}
		server, reqs := newPiServer(t, responses)
		cfg := config{AuthFile: authFile, PiAuthFile: piAuthFile, AccountsFile: accountsFile, UsageURL: server.URL, HTTPClient: server.Client()}
		acct := map[string]any{"access": ocAccess, "refresh": ocRefresh, "expires": ocExpires, "type": "oauth"}
		if omitOc {
			delete(acct, "expires")
		}
		_, _, changed, err := reconcilePiInbound(context.Background(), cfg, acct, usageWindow{UserID: curUser, UsedPercent: "55"}, true)
		if err != nil {
			t.Fatalf("reconcilePiInbound: %v", err)
		}
		return changed, reqs.Load(), accountsFile, authFile
	}

	cases := []struct {
		name, ocAccess, ocRefresh, ocAcctID, storeKey, storeAcc, piSeed, piBody string
		ocExpires, wantOcExp                                                    int64
		omitOc                                                                  bool
		store                                                                   map[string]map[string]any
		changed                                                                 bool
		reqs                                                                    int32
		storeExp                                                                int64
	}{
		{name: "active newer updates store+OpenCode", ocExpires: oldExpires, store: map[string]map[string]any{curUser: savedUser(curUser, ocAccess, ocRefresh, oldExpires, "saved@x")},
			piSeed: piAuthBody(piAccess, piRefresh, newExpires, piAcctID), piBody: piPayload(curUser, "55", ""),
			changed: true, reqs: 1,
			ocAccess: piAccess, ocRefresh: piRefresh, wantOcExp: newExpires, ocAcctID: piAcctID, storeAcc: piAccess, storeExp: newExpires,
		},
		{name: "non-active newer updates only store", ocExpires: oldExpires,
			store: map[string]map[string]any{
				curUser:   savedUser(curUser, ocAccess, ocRefresh, oldExpires, ""),
				otherUser: savedUser(otherUser, "old-other", "old-other-r", oldExpires, ""),
			},
			piSeed: piAuthBody(piAccess, piRefresh, newExpires, piAcctID), piBody: piPayload(otherUser, "12", ""),
			changed: true, reqs: 1, storeKey: otherUser, storeAcc: piAccess, storeExp: newExpires,
		},
		{name: "equal/older Pi expires no-op", ocExpires: newExpires, store: map[string]map[string]any{curUser: savedUser(curUser, ocAccess, ocRefresh, newExpires, "")},
			piSeed: piAuthBody(piAccess, piRefresh, oldExpires, ""), piBody: piPayload(curUser, "55", ""),
			changed: false, reqs: 1, wantOcExp: newExpires, storeAcc: ocAccess, storeExp: newExpires,
		},
		{name: "saved expires missing/invalid no-op", ocExpires: oldExpires, store: map[string]map[string]any{curUser: {"user_id": curUser, "access": ocAccess, "expires": 1.5, "type": "oauth"}},
			piSeed: piAuthBody(piAccess, piRefresh, newExpires, ""), piBody: piPayload(curUser, "55", ""),
			changed: false, reqs: 1,
		},
		{name: "no-match no-op", ocExpires: oldExpires, store: map[string]map[string]any{curUser: savedUser(curUser, ocAccess, ocRefresh, oldExpires, "")},
			piSeed: piAuthBody(piAccess, piRefresh, newExpires, ""), piBody: piPayload("user-unknown", "77", ""),
			changed: false, reqs: 1, storeAcc: ocAccess, storeExp: oldExpires,
		},
		{name: "current OpenCode expires missing no-op", ocExpires: oldExpires, omitOc: true, store: map[string]map[string]any{curUser: savedUser(curUser, ocAccess, ocRefresh, oldExpires, "")},
			piSeed: piAuthBody(piAccess, piRefresh, newExpires, ""), piBody: piPayload(curUser, "55", ""),
			changed: false, reqs: 1,
		},
		{name: "current OpenCode newer never downgrades", ocExpires: newExpires, store: map[string]map[string]any{curUser: savedUser(curUser, ocAccess, ocRefresh, oldExpires, "")},
			piSeed: piAuthBody(piAccess, piRefresh, oldExpires, ""), piBody: piPayload(curUser, "55", ""),
			changed: false, reqs: 1, wantOcExp: newExpires, storeAcc: ocAccess, storeExp: oldExpires,
		},
		{name: "saved account newer than Pi never downgrades", ocExpires: oldExpires, store: map[string]map[string]any{curUser: savedUser(curUser, ocAccess, ocRefresh, newExpires+1, "")},
			piSeed: piAuthBody(piAccess, piRefresh, newExpires, ""), piBody: piPayload(curUser, "55", ""),
			changed: false, reqs: 1, storeAcc: ocAccess, storeExp: newExpires + 1,
		},
		{name: "self-heal: Pi == saved but Pi > current -> only OpenCode", ocExpires: oldExpires, store: map[string]map[string]any{curUser: savedUser(curUser, "old-saved", "old-saved-r", newExpires, "saved@x")},
			piSeed: piAuthBody(ocAccess, "pi-r-shared", newExpires, piAcctID), piBody: piPayload(curUser, "55", ""),
			changed: true, reqs: 0, ocRefresh: "pi-r-shared", wantOcExp: newExpires, ocAcctID: piAcctID, storeAcc: "old-saved", storeExp: newExpires,
		},
		{name: "shape-change: dual Pi response persists secondary (5h ON)", ocExpires: oldExpires, store: map[string]map[string]any{curUser: savedUser(curUser, ocAccess, ocRefresh, oldExpires, "")},
			piSeed: piAuthBody(piAccess, piRefresh, newExpires, piAcctID), piBody: piPayload(curUser, "10", "20"),
			changed: true, reqs: 1,
			ocAccess: piAccess, ocRefresh: piRefresh, wantOcExp: newExpires, ocAcctID: piAcctID, storeAcc: piAccess, storeExp: newExpires,
		},
		{name: "invalid Pi credential no-op", ocExpires: oldExpires, store: map[string]map[string]any{curUser: savedUser(curUser, ocAccess, ocRefresh, oldExpires, "")},
			piSeed:  `{"openai-codex":{"type":"oauth","refresh":"r","expires":1}}`,
			changed: false, reqs: 0, storeAcc: ocAccess, storeExp: oldExpires,
		},
		{name: "Pi API rejection no-op", ocExpires: oldExpires, store: map[string]map[string]any{curUser: savedUser(curUser, ocAccess, ocRefresh, oldExpires, "")},
			piSeed:  piAuthBody(piAccess, piRefresh, newExpires, ""),
			changed: false, reqs: 1, storeAcc: ocAccess, storeExp: oldExpires,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			changed, reqs, accountsFile, authFile := runCase(t, tc.ocExpires, tc.store, tc.piSeed, tc.piBody, tc.omitOc)
			if changed != tc.changed || reqs != tc.reqs {
				t.Fatalf("changed=%v reqs=%d; want changed=%v reqs=%d", changed, reqs, tc.changed, tc.reqs)
			}
			if tc.ocAccess == "" {
				tc.ocAccess = ocAccess
			}
			if tc.ocRefresh == "" {
				tc.ocRefresh = ocRefresh
			}
			if tc.ocAcctID == "" {
				tc.ocAcctID = "oc-acct"
			}
			if tc.wantOcExp == 0 {
				tc.wantOcExp = tc.ocExpires
			}
			assertOCAuth(t, authFile, tc.ocAccess, tc.ocRefresh, tc.wantOcExp, tc.ocAcctID)
			if tc.storeAcc != "" {
				key := tc.storeKey
				if key == "" {
					key = curUser
				}
				s := mustReadStore(t, accountsFile)[key]
				if s["access"] != tc.storeAcc || storeInt64Must(t, s, "expires") != tc.storeExp {
					t.Fatalf("saved access=%v expires=%v; want %s %d", s["access"], s["expires"], tc.storeAcc, tc.storeExp)
				}
			}
		})
	}

	t.Run("store write failure leaves OpenCode unchanged", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("chmod only enforced on POSIX")
		}
		t.Parallel()
		authFile, piAuthFile, accountsFile := piSyncFixture(t)
		seedAuthFile(t, authFile, ocAuthBody(ocAccess, ocRefresh, oldExpires, "oc-acct"))
		seedAuthFile(t, piAuthFile, piAuthBody(piAccess, piRefresh, newExpires, piAcctID))
		writeStore(t, accountsFile, savedUser(curUser, ocAccess, ocRefresh, oldExpires, ""))
		if err := os.Chmod(accountsFile, 0o400); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(accountsFile, 0o600) })
		responses := piResponses{ocAccess: piPayload(curUser, "55", ""), piAccess: piPayload(curUser, "55", "")}
		server, _ := newPiServer(t, responses)
		cfg := config{AuthFile: authFile, PiAuthFile: piAuthFile, AccountsFile: accountsFile, UsageURL: server.URL, HTTPClient: server.Client()}
		_, _, _, err := reconcilePiInbound(context.Background(), cfg,
			map[string]any{"access": ocAccess, "refresh": ocRefresh, "expires": oldExpires, "type": "oauth"},
			usageWindow{UserID: curUser, UsedPercent: "55"}, true)
		if err == nil {
			t.Fatalf("want store-write error")
		}
		assertOCAuth(t, authFile, ocAccess, ocRefresh, oldExpires, "oc-acct")
	})

	t.Run("OpenCode write failure returns error and leaves store updated", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("chmod only enforced on POSIX")
		}
		t.Parallel()
		authFile, piAuthFile, accountsFile := piSyncFixture(t)
		seedAuthFile(t, authFile, ocAuthBody(ocAccess, ocRefresh, oldExpires, "oc-acct"))
		seedAuthFile(t, piAuthFile, piAuthBody(piAccess, piRefresh, newExpires, piAcctID))
		writeStore(t, accountsFile, savedUser(curUser, ocAccess, ocRefresh, oldExpires, ""))
		if err := os.Chmod(authFile, 0o400); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(authFile, 0o600) })
		responses := piResponses{ocAccess: piPayload(curUser, "55", ""), piAccess: piPayload(curUser, "55", "")}
		server, _ := newPiServer(t, responses)
		cfg := config{AuthFile: authFile, PiAuthFile: piAuthFile, AccountsFile: accountsFile, UsageURL: server.URL, HTTPClient: server.Client()}
		_, _, _, err := reconcilePiInbound(context.Background(), cfg,
			map[string]any{"access": ocAccess, "refresh": ocRefresh, "expires": oldExpires, "type": "oauth"},
			usageWindow{UserID: curUser, UsedPercent: "55"}, true)
		if err == nil {
			t.Fatalf("want opencode-write error")
		}
		if s := mustReadStore(t, accountsFile)[curUser]; s["access"] != piAccess {
			t.Fatalf("store not updated: access=%v", s["access"])
		}
		assertOCAuth(t, authFile, ocAccess, ocRefresh, oldExpires, "oc-acct")
	})
}

func TestRunDefaultPiInboundReconciliation(t *testing.T) {
	t.Parallel()
	t.Run("same access: no second API call", func(t *testing.T) {
		t.Parallel()
		authFile, piAuthFile, accountsFile := piSyncFixture(t)
		const shared = "shared-access"
		seedAuthFile(t, authFile, ocAuthBody(shared, "oc-r", 1776000000000, "oc-acct"))
		seedAuthFile(t, piAuthFile, piAuthBody(shared, "pi-r-shared", 1778000000000, "pi-acct"))
		writeStore(t, accountsFile, map[string]any{"user_id": "user-cur", "access": shared, "expires": int64(1776000000000), "type": "oauth"})
		server, requests := newPiServer(t, piResponses{shared: piPayload("user-cur", "42", "")})
		var out strings.Builder
		if err := runWithArgs(context.Background(), config{AuthFile: authFile, PiAuthFile: piAuthFile, AccountsFile: accountsFile, UsageURL: server.URL, HTTPClient: server.Client()}, &out, nil); err != nil {
			t.Fatalf("runWithArgs: %v", err)
		}
		if out.String() != "42" || requests.Load() != 1 {
			t.Fatalf("stdout=%q requests=%d; want 42, 1", out.String(), requests.Load())
		}
		assertOCAuth(t, authFile, shared, "pi-r-shared", 1778000000000, "pi-acct")
	})
	t.Run("shape-change: 5h ON recomputes mode from dual Pi response", func(t *testing.T) {
		t.Parallel()
		authFile, piAuthFile, accountsFile := piSyncFixture(t)
		seedAuthFile(t, authFile, ocAuthBody("oc-access", "oc-r", 1776000000000, "oc-acct"))
		seedAuthFile(t, piAuthFile, piAuthBody("pi-access", "pi-r", 1778000000000, "pi-acct"))
		writeStore(t, accountsFile, map[string]any{"user_id": "user-cur", "access": "oc-access", "expires": int64(1776000000000), "type": "oauth", "usedPercent": "10", "resetAt": int64(1777414800)})
		responses := piResponses{"oc-access": piPayload("user-cur", "10", ""), "pi-access": piPayload("user-cur", "10", "20")}
		server, _ := newPiServer(t, responses)
		cfgFile := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(cfgFile, []byte(`{"5h":"on"}`), 0o600); err != nil {
			t.Fatalf("seed cfg: %v", err)
		}
		var out strings.Builder
		if err := runWithArgs(context.Background(), config{AuthFile: authFile, PiAuthFile: piAuthFile, AccountsFile: accountsFile, ConfigFile: cfgFile, UsageURL: server.URL, HTTPClient: server.Client()}, &out, nil); err != nil {
			t.Fatalf("runWithArgs: %v", err)
		}
		if out.String() != "10" {
			t.Fatalf("stdout: got %q, want 10 (5h primary)", out.String())
		}
		saved := mustReadStore(t, accountsFile)["user-cur"]
		if saved["secondaryUsedPercent"] != "20" || storeInt64Must(t, saved, "secondaryResetAt") != 1778203200 {
			t.Fatalf("secondary not persisted: secondaryUsedPercent=%v secondaryResetAt=%v", saved["secondaryUsedPercent"], saved["secondaryResetAt"])
		}
	})
	t.Run("email preservation: saved email kept", func(t *testing.T) {
		t.Parallel()
		authFile, piAuthFile, accountsFile := piSyncFixture(t)
		seedAuthFile(t, authFile, ocAuthBody("oc-access", "oc-r", 1776000000000, "oc-acct"))
		seedAuthFile(t, piAuthFile, piAuthBody("pi-access", "pi-r", 1778000000000, "pi-acct"))
		writeStore(t, accountsFile, map[string]any{"user_id": "user-cur", "access": "oc-access", "refresh": "oc-r", "expires": int64(1776000000000), "type": "oauth", "email": "saved@example.com"})
		responses := piResponses{"oc-access": piPayload("user-cur", "55", ""), "pi-access": piPayload("user-cur", "55", "")}
		server, _ := newPiServer(t, responses)
		var out strings.Builder
		if err := runWithArgs(context.Background(), config{AuthFile: authFile, PiAuthFile: piAuthFile, AccountsFile: accountsFile, UsageURL: server.URL, HTTPClient: server.Client()}, &out, nil); err != nil {
			t.Fatalf("runWithArgs: %v", err)
		}
		saved := mustReadStore(t, accountsFile)["user-cur"]
		if saved["email"] != "saved@example.com" {
			t.Fatalf("saved email overwritten: got %v", saved["email"])
		}
	})
}

func TestRunNonDefaultCommandsDoNotImportPi(t *testing.T) {
	t.Parallel()
	const (
		ocAccess, ocRefresh, otherAccess, piNew = "opencode-access", "opencode-refresh", "other-access", "pi-new"
		ocExpires                               = int64(1776000000000)
	)
	setup := func(t *testing.T) (string, string, string, *httptest.Server, *atomic.Int32) {
		t.Helper()
		authFile, piAuthFile, accountsFile := piSyncFixture(t)
		seedAuthFile(t, authFile, ocAuthBody(ocAccess, ocRefresh, ocExpires, "oc-acct"))
		seedAuthFile(t, piAuthFile, piAuthBody(piNew, "pi-r", 1778000000000, "pi-acct"))
		writeStore(t, accountsFile, map[string]any{"user_id": "user-cur", "access": ocAccess, "refresh": ocRefresh, "expires": ocExpires, "type": "oauth"}, map[string]any{"user_id": "user-other", "access": otherAccess, "refresh": "other-r", "expires": ocExpires, "type": "oauth"})
		responses := piResponses{ocAccess: piPayload("user-cur", "13", ""), otherAccess: piPayload("user-other", "66", ""), piNew: piPayload("user-cur", "13", "")}
		server, requests := newPiServer(t, responses)
		return authFile, piAuthFile, accountsFile, server, requests
	}
	cases := []struct {
		name   string
		args   []string
		expect func(t *testing.T, a, p string, r *atomic.Int32)
	}{
		{"accounts", []string{"accounts"}, func(t *testing.T, a, _ string, r *atomic.Int32) {
			assertOCAuth(t, a, ocAccess, ocRefresh, ocExpires, "oc-acct")
			if got := r.Load(); got != 2 {
				t.Fatalf("accounts must not issue Pi request: got %d, want 2", got)
			}
		}},
		{"list", []string{"list"}, func(t *testing.T, a, _ string, _ *atomic.Int32) {
			assertOCAuth(t, a, ocAccess, ocRefresh, ocExpires, "oc-acct")
		}},
		{"use rotates Pi but does not inbound-import", []string{"use", "user-other"}, func(t *testing.T, _, p string, _ *atomic.Int32) {
			pc, _ := readJSONObject(t, p)["openai-codex"].(map[string]any)
			if pc == nil || pc["access"] == piNew {
				t.Fatalf("use triggered inbound import: %v", pc)
			}
			if pc == nil || pc["access"] != otherAccess {
				t.Fatalf("use did not rotate Pi to user-other: %v", pc)
			}
		}},
		{"config does not touch Pi auth", []string{"config", "5h"}, func(t *testing.T, a, p string, _ *atomic.Int32) {
			original := mustReadFile(t, p)
			cfgFile := filepath.Join(t.TempDir(), "config.json")
			if err := runWithArgs(context.Background(), config{AuthFile: a, PiAuthFile: p, AccountsFile: filepath.Dir(p) + "/accounts.json", ConfigFile: cfgFile, UsageURL: "http://unused", HTTPClient: http.DefaultClient}, &strings.Builder{}, []string{"config", "5h"}); err != nil {
				t.Fatalf("config: %v", err)
			}
			if string(mustReadFile(t, p)) != string(original) {
				t.Fatalf("config mutated Pi auth")
			}
			assertOCAuth(t, a, ocAccess, ocRefresh, ocExpires, "oc-acct")
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, p, acc, server, requests := setup(t)
			var out strings.Builder
			if err := runWithArgs(context.Background(), config{AuthFile: a, PiAuthFile: p, AccountsFile: acc, UsageURL: server.URL, HTTPClient: server.Client()}, &out, tc.args); err != nil {
				t.Fatalf("runWithArgs %v: %v", tc.args, err)
			}
			tc.expect(t, a, p, requests)
		})
	}
}

func TestRunDefaultCommand(t *testing.T) {
	t.Parallel()
	type seed struct{ oc, pi string }
	cases := []struct {
		name      string
		seed      seed
		resp      piResponses
		wantRun   bool
		wantOut   string
		wantErr   []string
		wantAPI   int32
		wantStore map[string]map[string]any
		cfgFile   string
		emptyPi   bool
	}{
		{
			name:    "both unavailable: explicit English error, no stdout, no API",
			wantErr: []string{errNeitherProviderAvailable.Error()},
			wantAPI: 0,
		},
		{
			name:    "explicit empty PiAuthFile + missing OC auth: same English error",
			wantErr: []string{errNeitherProviderAvailable.Error()},
			wantAPI: 0, emptyPi: true,
		},
		{
			name:    "OC available, Pi unavailable: legacy OC pipeline runs",
			seed:    seed{oc: ocAuthBody("oc-a", "oc-r", 1776000000000, "oc-acct")},
			resp:    piResponses{"oc-a": piPayload("user-cur", "55", "")},
			wantRun: true, wantOut: "55", wantAPI: 1,
		},
		{
			name:    "Pi malformed: routes to OC, no Pi request",
			seed:    seed{oc: ocAuthBody("oc-a", "oc-r", 1776000000000, "oc-acct"), pi: "{not valid"},
			resp:    piResponses{"oc-a": piPayload("user-cur", "37", "")},
			wantRun: true, wantOut: "37", wantAPI: 1,
		},
		{
			name:    "OC missing + Pi available: Pi-only path runs, persists store, no OC write",
			seed:    seed{pi: piAuthBody("pi-a", "pi-r", 1778000000000, "pi-acct")},
			resp:    piResponses{"pi-a": piPayload("user-cur", "42", "")},
			wantRun: true, wantOut: "42", wantAPI: 1,
			wantStore: map[string]map[string]any{
				"user-cur": {"access": "pi-a", "refresh": "pi-r", "expires": int64(1778000000000), "usedPercent": "42", "type": "oauth", "accountId": "pi-acct"},
			},
		},
		{
			name:    "OC malformed + Pi available: Pi-only path runs, OC untouched",
			seed:    seed{oc: "{not valid", pi: piAuthBody("pi-a", "pi-r", 1778000000000, "pi-acct")},
			resp:    piResponses{"pi-a": piPayload("user-cur", "55", "")},
			wantRun: true, wantOut: "55", wantAPI: 1,
		},
		{
			name:    "OC + Pi available, both API OK: OC pipeline + inbound reconcile",
			seed:    seed{oc: ocAuthBody("oc-a", "oc-r", 1776000000000, "oc-acct"), pi: piAuthBody("pi-a", "pi-r", 1778000000000, "pi-acct")},
			resp:    piResponses{"oc-a": piPayload("user-cur", "55", ""), "pi-a": piPayload("user-cur", "55", "")},
			wantRun: true, wantOut: "55", wantAPI: 2,
		},
		{
			name:    "OC API rejected + Pi valid: Pi-only fallback succeeds",
			seed:    seed{oc: ocAuthBody("oc-a", "oc-r", 1776000000000, "oc-acct"), pi: piAuthBody("pi-a", "pi-r", 1778000000000, "pi-acct")},
			resp:    piResponses{"pi-a": piPayload("user-cur", "72", "")},
			wantRun: true, wantOut: "72", wantAPI: 2,
		},
		{
			name:    "both APIs fail: joined runtime diagnostic, NEVER the not-installed error",
			seed:    seed{oc: ocAuthBody("oc-a", "oc-r", 1776000000000, "oc-acct"), pi: piAuthBody("pi-a", "pi-r", 1778000000000, "pi-acct")},
			wantErr: []string{"OpenCode usage fetch failed", "Pi usage fetch also failed"},
			wantAPI: 2,
		},
		{
			name:    "Pi-only: 5h ON dual response persists secondary; stdout primary",
			seed:    seed{pi: piAuthBody("pi-a", "pi-r", 1778000000000, "pi-acct")},
			resp:    piResponses{"pi-a": piPayload("user-cur", "10", "20")},
			wantRun: true, wantOut: "10", wantAPI: 1, cfgFile: `{"5h":"on"}`,
			wantStore: map[string]map[string]any{"user-cur": {"usedPercent": "10", "secondaryUsedPercent": "20"}},
		},
		{
			name:    "Pi-only: 5h OFF promotes secondary to primary; clears secondary",
			seed:    seed{pi: piAuthBody("pi-a", "pi-r", 1778000000000, "pi-acct")},
			resp:    piResponses{"pi-a": piPayload("user-cur", "10", "20")},
			wantRun: true, wantOut: "20", wantAPI: 1, cfgFile: `{"5h":"off"}`,
			wantStore: map[string]map[string]any{"user-cur": {"usedPercent": "20"}},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			authFile, piAuthFile, accountsFile := piSyncFixture(t)
			if tc.seed.oc != "" {
				seedAuthFile(t, authFile, tc.seed.oc)
			}
			if tc.seed.pi != "" {
				seedAuthFile(t, piAuthFile, tc.seed.pi)
			}
			server, requests := newPiServer(t, tc.resp)
			cfg := config{AuthFile: authFile, PiAuthFile: piAuthFile, AccountsFile: accountsFile, UsageURL: server.URL, HTTPClient: server.Client()}
			if tc.emptyPi {
				cfg.PiAuthFile = ""
			}
			if tc.cfgFile != "" {
				cfg.ConfigFile = filepath.Join(t.TempDir(), "config.json")
				if err := os.WriteFile(cfg.ConfigFile, []byte(tc.cfgFile), 0o600); err != nil {
					t.Fatalf("seed cfg: %v", err)
				}
			}
			var out strings.Builder
			err := runWithArgs(context.Background(), cfg, &out, nil)
			if tc.wantRun && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.wantRun && err == nil {
				t.Fatalf("expected error, got nil; stdout=%q", out.String())
			}
			if tc.wantRun && out.String() != tc.wantOut {
				t.Fatalf("stdout=%q want %q", out.String(), tc.wantOut)
			}
			if !tc.wantRun {
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("error %q must contain %q", err.Error(), want)
					}
				}
				if (tc.seed.oc != "" || tc.seed.pi != "") && strings.Contains(err.Error(), errNeitherProviderAvailable.Error()) {
					t.Fatalf("error must not be the not-installed error when a provider was configured: %v", err)
				}
				if out.Len() != 0 {
					t.Fatalf("must not print on failure, got %q", out.String())
				}
			}
			if got := requests.Load(); got != tc.wantAPI {
				t.Fatalf("api calls: got %d, want %d", got, tc.wantAPI)
			}
			for userID, wantFields := range tc.wantStore {
				saved := mustReadStore(t, accountsFile)[userID]
				for k, want := range wantFields {
					if k == "expires" {
						if got := storeInt64Must(t, saved, k); got != want.(int64) {
							t.Fatalf("stored %s.%s=%v want %v", userID, k, saved[k], want)
						}
						continue
					}
					if saved[k] != want {
						t.Fatalf("stored %s.%s=%v want %v", userID, k, saved[k], want)
					}
				}
			}
		})
	}
}

func TestLoadProviderAvailability(t *testing.T) {
	t.Parallel()
	t.Run("OC unavailable: missing/empty/malformed/no-access", func(t *testing.T) {
		t.Parallel()
		if _, ok := loadOpenCodeAccountIfAvailable(filepath.Join(t.TempDir(), "auth.json")); ok {
			t.Fatal("missing file must be unavailable")
		}
		if _, ok := loadOpenCodeAccountIfAvailable(""); ok {
			t.Fatal("empty path must be unavailable")
		}
		f := filepath.Join(t.TempDir(), "auth.json")
		for _, body := range []string{`{not valid`, `{"openai.accountId":"acct-1"}`} {
			seedAuthFile(t, f, body)
			if _, ok := loadOpenCodeAccountIfAvailable(f); ok {
				t.Fatalf("body %q must be unavailable", body)
			}
		}
	})
	t.Run("OC legacy non-empty access is available", func(t *testing.T) {
		t.Parallel()
		f := filepath.Join(t.TempDir(), "auth.json")
		seedAuthFile(t, f, `{"openai.access":"legacy-token"}`)
		a, ok := loadOpenCodeAccountIfAvailable(f)
		if !ok || a["access"] != "legacy-token" {
			t.Fatalf("legacy auth file must remain available, got ok=%v", ok)
		}
	})
	t.Run("Pi unavailable: empty/missing/missing-entry/malformed/invalid-oauth", func(t *testing.T) {
		t.Parallel()
		if _, ok := loadPiAuthIfUsable(""); ok {
			t.Fatal("empty path must be unavailable")
		}
		if _, ok := loadPiAuthIfUsable(filepath.Join(t.TempDir(), "nope.json")); ok {
			t.Fatal("missing file must be unavailable")
		}
		f := filepath.Join(t.TempDir(), "auth.json")
		for _, body := range []string{`{"anthropic":{"access":"anth"}}`, `{not valid`, `{"openai-codex":{"type":"oauth","access":"a"}}`} {
			seedAuthFile(t, f, body)
			if _, ok := loadPiAuthIfUsable(f); ok {
				t.Fatalf("body %q must be unavailable", body)
			}
		}
	})
	t.Run("Pi valid openai-codex is available and reusable", func(t *testing.T) {
		t.Parallel()
		f := filepath.Join(t.TempDir(), "auth.json")
		seedAuthFile(t, f, piAuthBody("pi-a", "pi-r", 1778000000000, "pi-acct"))
		info, ok := loadPiAuthIfUsable(f)
		if !ok || info == nil || info.CredMap["access"] != "pi-a" || info.Payload["openai-codex"] == nil {
			t.Fatalf("valid credential must be available: %#v", info)
		}
	})
}

func TestPiOnlyNoRotation(t *testing.T) {
	t.Parallel()
	authFile, piAuthFile, accountsFile := piSyncFixture(t)
	seedAuthFile(t, piAuthFile, piAuthBody("pi-a", "pi-r", 1778000000000, "pi-acct"))
	writeStore(t, accountsFile,
		map[string]any{"user_id": "user-cur", "access": "pi-a", "refresh": "pi-r", "expires": int64(1778000000000), "type": "oauth"},
		map[string]any{"user_id": "user-other", "access": "other-a", "refresh": "other-r", "expires": int64(1778000000000), "type": "oauth"},
	)
	server, _ := newPiServer(t, piResponses{"pi-a": piPayload("user-cur", "99", "")})
	var out strings.Builder
	if err := runWithArgs(context.Background(), config{AuthFile: authFile, PiAuthFile: piAuthFile, AccountsFile: accountsFile, UsageURL: server.URL, HTTPClient: server.Client()}, &out, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.String() != "99" {
		t.Fatalf("stdout=%q want 99", out.String())
	}
	if _, err := os.Stat(authFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Pi-only must NOT create OC auth, stat err=%v", err)
	}
	if _, ok := mustReadStore(t, accountsFile)["user-cur"]; !ok {
		t.Fatal("Pi account disappeared from store (rotated unexpectedly)")
	}
}

// ---------------------------------------------------------------------------------------
// Configurable rotation thresholds: 5h_threshold and weekly_threshold.
// ---------------------------------------------------------------------------------------

// thresholdFixture centralizes the column header used by table-driven cases for
// both threshold subcommands. Tests reuse the same matrix for the loader,
// writer, and CLI dispatch so the contract stays in one place.
type thresholdFixture struct {
	name      string
	raw       string
	wantValue float64
	wantErr   bool
}

// thresholdLoadCases returns the loader matrix shared by the two new keys. The
// "missing key" row uses the existing 5h toggle in the file so the loader's
// "ignore unrelated keys" contract is exercised alongside the threshold default.
// "missing file" / "empty path" rows exercise the documented default semantics:
// no config means default; existing JSON with the wrong shape returns an error.
//
// `key` is the on-disk key the loader looks up; every row in the matrix is
// templated so each test instance reads only its own key, never the sibling's.
func thresholdLoadCases(key string, defaultValue float64) []thresholdFixture {
	return []thresholdFixture{
		{name: "empty path returns default", raw: "", wantValue: defaultValue},
		{name: "missing file returns default", raw: "__missing__", wantValue: defaultValue},
		{name: "empty file returns default", raw: "__empty__", wantValue: defaultValue},
		{name: "missing key returns default", raw: `{"5h":"on"}`, wantValue: defaultValue},
		{name: "reads integer", raw: fmt.Sprintf(`{"%s":75}`, key), wantValue: 75},
		{name: "reads float", raw: fmt.Sprintf(`{"%s":97.5}`, key), wantValue: 97.5},
		{name: "rejects zero", raw: fmt.Sprintf(`{"%s":0}`, key), wantErr: true},
		{name: "rejects negative", raw: fmt.Sprintf(`{"%s":-1}`, key), wantErr: true},
		{name: "rejects over 100", raw: fmt.Sprintf(`{"%s":100.01}`, key), wantErr: true},
		{name: "rejects 101", raw: fmt.Sprintf(`{"%s":101}`, key), wantErr: true},
		{name: "rejects string", raw: fmt.Sprintf(`{"%s":"abc"}`, key), wantErr: true},
		{name: "rejects malformed JSON", raw: `{bad`, wantErr: true},
	}
}

func seedThresholdConfig(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "config.json")
	switch content {
	case "__missing__":
		_ = os.RemoveAll(path)
		return filepath.Join(t.TempDir(), "config.json")
	case "__empty__":
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("seed empty: %v", err)
		}
		return path
	case "":
		return filepath.Join(t.TempDir(), "config.json")
	default:
		writeConfigFile(t, path, content)
		return path
	}
}

// TestLoadFiveHourThresholdConfig exercises the loader for the 5-hour
// threshold: defaults (80), valid numeric reads (integer and fractional),
// rejection of out-of-range values, non-numeric types, and malformed JSON.
// The contract mirrors the existing 5h toggle loader: missing config means the
// runtime default, bad config means a hard error so the CLI refuses to start.
func TestLoadFiveHourThresholdConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range thresholdLoadCases("5h_threshold", 80.0) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := seedThresholdConfig(t, dir, tc.raw)
			value, err := loadFiveHourThresholdConfig(path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got value=%v", value)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadFiveHourThresholdConfig: %v", err)
			}
			if value != tc.wantValue {
				t.Fatalf("value=%v, want %v", value, tc.wantValue)
			}
		})
	}
}

// TestLoadWeeklyThresholdConfig mirrors the 5h loader contract for the weekly
// threshold, including its 98 default.
func TestLoadWeeklyThresholdConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range thresholdLoadCases("weekly_threshold", 98.0) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := seedThresholdConfig(t, dir, tc.raw)
			value, err := loadWeeklyThresholdConfig(path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got value=%v", value)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadWeeklyThresholdConfig: %v", err)
			}
			if value != tc.wantValue {
				t.Fatalf("value=%v, want %v", value, tc.wantValue)
			}
		})
	}
}

// TestSaveFiveHourThresholdConfig pins the writer: numeric values round-trip,
// unknown keys are preserved, the parent directory is created on demand, and
// empty paths are rejected. The numeric fidelity test ensures persisted values
// are not truncated to integers (the CLI accepts fractional inputs).
func TestSaveFiveHourThresholdConfig(t *testing.T) {
	t.Parallel()

	t.Run("writes value and preserves unknown keys", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "config.json")
		writeConfigFile(t, path, `{"other":"kept","nested":{"x":1}}`)
		if err := saveFiveHourThresholdConfig(path, 72.5); err != nil {
			t.Fatalf("save: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(mustReadFile(t, path), &payload); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got, _ := payload["5h_threshold"].(float64); got != 72.5 {
			t.Fatalf("expected 5h_threshold=72.5, got %v", payload["5h_threshold"])
		}
		if got, _ := payload["other"].(string); got != "kept" {
			t.Fatalf("expected unknown key preserved, got %v", payload["other"])
		}
		if _, ok := payload["nested"]; !ok {
			t.Fatalf("expected nested object preserved, got %v", payload)
		}
	})

	t.Run("creates parent directory", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "nested", "subdir", "config.json")
		if err := saveFiveHourThresholdConfig(path, 50); err != nil {
			t.Fatalf("save: %v", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected config file to exist: %v", err)
		}
	})

	t.Run("rejects empty path", func(t *testing.T) {
		t.Parallel()
		if err := saveFiveHourThresholdConfig("", 50); err == nil {
			t.Fatalf("expected error for empty config path")
		}
	})

	t.Run("rejects out-of-range values without writing", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "config.json")
		writeConfigFile(t, path, `{"5h":"on"}`)
		for _, bad := range []float64{0, -1, 100.01, 200} {
			if err := saveFiveHourThresholdConfig(path, bad); err == nil {
				t.Fatalf("expected error for value %v", bad)
			}
			var payload map[string]any
			if err := json.Unmarshal(mustReadFile(t, path), &payload); err != nil {
				t.Fatalf("parse after bad save: %v", err)
			}
			if _, ok := payload["5h_threshold"]; ok {
				t.Fatalf("config must not be modified on validation failure, got %v", payload)
			}
			if got, _ := payload["5h"].(string); got != "on" {
				t.Fatalf("expected 5h=on preserved, got %v", payload["5h"])
			}
		}
	})
}

// TestSaveWeeklyThresholdConfig mirrors the 5h threshold writer for the weekly
// key, with the same preservation/rejection guarantees.
func TestSaveWeeklyThresholdConfig(t *testing.T) {
	t.Parallel()

	t.Run("writes value and preserves unknown keys", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "config.json")
		writeConfigFile(t, path, `{"other":"kept"}`)
		if err := saveWeeklyThresholdConfig(path, 99.5); err != nil {
			t.Fatalf("save: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(mustReadFile(t, path), &payload); err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got, _ := payload["weekly_threshold"].(float64); got != 99.5 {
			t.Fatalf("expected weekly_threshold=99.5, got %v", payload["weekly_threshold"])
		}
		if got, _ := payload["other"].(string); got != "kept" {
			t.Fatalf("expected unknown key preserved, got %v", payload["other"])
		}
	})

	t.Run("rejects empty path", func(t *testing.T) {
		t.Parallel()
		if err := saveWeeklyThresholdConfig("", 50); err == nil {
			t.Fatalf("expected error for empty config path")
		}
	})

	t.Run("rejects out-of-range values without writing", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "config.json")
		writeConfigFile(t, path, `{"5h":"on"}`)
		for _, bad := range []float64{0, -1, 100.01, 200} {
			if err := saveWeeklyThresholdConfig(path, bad); err == nil {
				t.Fatalf("expected error for value %v", bad)
			}
			var payload map[string]any
			if err := json.Unmarshal(mustReadFile(t, path), &payload); err != nil {
				t.Fatalf("parse after bad save: %v", err)
			}
			if _, ok := payload["weekly_threshold"]; ok {
				t.Fatalf("config must not be modified on validation failure, got %v", payload)
			}
		}
	})
}

// TestValidateThresholdPercent centralizes the validator contract: finite
// numbers in the half-open interval (0, 100]. Non-numeric types, NaN, ±Inf,
// zero, and negatives are rejected with a stable error so the CLI can show a
// useful message to the user.
func TestValidateThresholdPercent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		want    float64
		wantErr bool
	}{
		{"integer", "80", 80, false},
		{"fractional", "97.5", 97.5, false},
		{"boundary 100", "100", 100, false},
		{"zero rejected", "0", 0, true},
		{"negative rejected", "-1", 0, true},
		{"over 100 rejected", "100.01", 0, true},
		{"non-numeric rejected", "abc", 0, true},
		{"empty rejected", "", 0, true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := validateThresholdPercent(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateThresholdPercent: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRunConfigThresholdSubcommands covers the `config <key> [value]` CLI:
// setting a value, reading the effective value (parseable number), preserving
// unrelated keys, rejecting malformed/out-of-range input without writing, and
// honoring the empty-path contract.
func TestRunConfigThresholdSubcommands(t *testing.T) {
	t.Parallel()

	type sub struct {
		key, field string
		defaultVal float64
	}

	subs := []sub{
		{key: "5h-threshold", field: "5h_threshold", defaultVal: 80},
		{key: "weekly-threshold", field: "weekly_threshold", defaultVal: 98},
	}

	for _, s := range subs {
		s := s
		t.Run(s.key+" sets value and reports it back", func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.json")
			var out strings.Builder
			if err := runConfigCommand(config{ConfigFile: path}, &out, []string{s.key, "75"}); err != nil {
				t.Fatalf("runConfigCommand: %v", err)
			}
			if got := strings.TrimSpace(out.String()); got != "75" {
				t.Fatalf("stdout=%q, want %q", got, "75")
			}
			var payload map[string]any
			if err := json.Unmarshal(mustReadFile(t, path), &payload); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got, _ := payload[s.field].(float64); got != 75 {
				t.Fatalf("expected %s=75 on disk, got %v", s.field, payload[s.field])
			}
		})

		t.Run(s.key+" reports effective default when missing", func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.json")
			var out strings.Builder
			if err := runConfigCommand(config{ConfigFile: path}, &out, []string{s.key}); err != nil {
				t.Fatalf("runConfigCommand: %v", err)
			}
			want := strconv.FormatFloat(s.defaultVal, 'f', -1, 64)
			if got := strings.TrimSpace(out.String()); got != want {
				t.Fatalf("stdout=%q, want %q", got, want)
			}
		})

		t.Run(s.key+" reports effective persisted value", func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.json")
			writeConfigFile(t, path, fmt.Sprintf(`{"%s":42}`, s.field))
			var out strings.Builder
			if err := runConfigCommand(config{ConfigFile: path}, &out, []string{s.key}); err != nil {
				t.Fatalf("runConfigCommand: %v", err)
			}
			if got := strings.TrimSpace(out.String()); got != "42" {
				t.Fatalf("stdout=%q, want %q", got, "42")
			}
		})

		t.Run(s.key+" rejects malformed value without writing", func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.json")
			writeConfigFile(t, path, `{"5h":"on"}`)
			err := runConfigCommand(config{ConfigFile: path}, &strings.Builder{}, []string{s.key, "not-a-number"})
			if err == nil {
				t.Fatalf("expected error for malformed value")
			}
			var payload map[string]any
			if perr := json.Unmarshal(mustReadFile(t, path), &payload); perr != nil {
				t.Fatalf("parse after bad save: %v", perr)
			}
			if _, ok := payload[s.field]; ok {
				t.Fatalf("config must not be modified on validation failure, got %v", payload)
			}
			if got, _ := payload["5h"].(string); got != "on" {
				t.Fatalf("expected 5h=on preserved, got %v", payload["5h"])
			}
		})

		t.Run(s.key+" rejects out-of-range value without writing", func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.json")
			// Pre-seed with both the 5h toggle and a sentinel custom key so we can
			// verify all known keys survive an invalid attempt.
			writeConfigFile(t, path, `{"5h":"on","custom":{"kept":true}}`)
			for _, bad := range []string{"0", "-1", "100.01", "200"} {
				err := runConfigCommand(config{ConfigFile: path}, &strings.Builder{}, []string{s.key, bad})
				if err == nil {
					t.Fatalf("expected error for value %q", bad)
				}
			}
			var payload map[string]any
			if perr := json.Unmarshal(mustReadFile(t, path), &payload); perr != nil {
				t.Fatalf("parse after rejected writes: %v", perr)
			}
			if _, ok := payload[s.field]; ok {
				t.Fatalf("config must not be modified on validation failure, got %v", payload)
			}
			if got, _ := payload["5h"].(string); got != "on" {
				t.Fatalf("expected 5h=on preserved, got %v", payload["5h"])
			}
			if _, ok := payload["custom"]; !ok {
				t.Fatalf("expected custom key preserved, got %v", payload)
			}
		})

		t.Run(s.key+" preserves 5h toggle and unknown keys", func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "config.json")
			writeConfigFile(t, path, `{"5h":"off","custom":{"kept":true}}`)
			if err := runConfigCommand(config{ConfigFile: path}, &strings.Builder{}, []string{s.key, "55"}); err != nil {
				t.Fatalf("runConfigCommand: %v", err)
			}
			var payload map[string]any
			if err := json.Unmarshal(mustReadFile(t, path), &payload); err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got, _ := payload["5h"].(string); got != "off" {
				t.Fatalf("expected 5h=off preserved, got %v", payload["5h"])
			}
			if _, ok := payload["custom"]; !ok {
				t.Fatalf("expected custom key preserved, got %v", payload)
			}
		})

		t.Run(s.key+" rejects too many positional args", func(t *testing.T) {
			t.Parallel()
			err := runConfigCommand(config{ConfigFile: filepath.Join(t.TempDir(), "c.json")}, &strings.Builder{}, []string{s.key, "10", "20"})
			if err == nil {
				t.Fatalf("expected error for extra args")
			}
		})
	}

	t.Run("unknown subcommand still rejected with helpful message", func(t *testing.T) {
		t.Parallel()
		err := runConfigCommand(config{ConfigFile: filepath.Join(t.TempDir(), "c.json")}, &strings.Builder{}, []string{"monthly-threshold", "80"})
		if err == nil || !strings.Contains(err.Error(), "unknown config feature") {
			t.Fatalf("expected unknown-feature error, got %v", err)
		}
	})
}

// TestRunWithArgsLoadsThresholdsFromConfig pins the integration contract: the
// runtime path (no `config` subcommand) loads the persisted thresholds from the
// config file before evaluating exhaustion. With 5h_threshold raised above the
// API value the default-5h-mode path must NOT rotate; with the threshold
// dropped below the API value the path must rotate. This proves the values
// flow through `runWithArgs` → `cfg.FiveHourThreshold/WeeklyThreshold` →
// `accountWithUsage`/rotation.
func TestRunWithArgsLoadsThresholdsFromConfig(t *testing.T) {
	t.Parallel()

	resetAt := time.Now().Add(2 * time.Hour).Unix()
	makeBody := func(used, secondary string) string {
		return fmt.Sprintf(
			`{"user_id":"user-current","rate_limit":{"primary_window":{"used_percent":%s,"reset_at":%d},"secondary_window":{"used_percent":%s,"reset_at":%d}}}`,
			used, resetAt, secondary, resetAt,
		)
	}

	t.Run("raised 5h threshold blocks rotation at default value", func(t *testing.T) {
		t.Parallel()
		// API returns 85% (above default 80) but below configured 90.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(makeBody("85", "10")))
		}))
		defer server.Close()

		dir := t.TempDir()
		authFile := filepath.Join(dir, "auth.json")
		if err := os.WriteFile(authFile, []byte(`{"openai.access":"current-token"}`), 0o600); err != nil {
			t.Fatalf("write auth: %v", err)
		}
		accountsFile := filepath.Join(dir, "accounts.json")
		writeStore(t, accountsFile,
			map[string]any{"user_id": "user-current", "access": "current-token"},
			map[string]any{"user_id": "user-alt", "access": "alt-token"},
		)
		configFile := filepath.Join(dir, "config.json")
		writeConfigFile(t, configFile, `{"5h_threshold":90}`)

		var out strings.Builder
		err := runWithArgs(context.Background(), config{
			AuthFile:     authFile,
			AccountsFile: accountsFile,
			UsageURL:     server.URL,
			HTTPClient:   server.Client(),
			ConfigFile:   configFile,
		}, &out, nil)
		if err != nil {
			t.Fatalf("runWithArgs: %v", err)
		}
		if out.String() != "85" {
			t.Fatalf("stdout=%q want 85", out.String())
		}
		store := mustReadStore(t, accountsFile)
		entry := store["user-current"]
		if _, has := entry["cooldownUntil"]; has {
			t.Fatalf("expected no cooldown when below configured 5h threshold, got %v", entry)
		}
	})

	t.Run("lowered 5h threshold forces rotation before default cutoff", func(t *testing.T) {
		t.Parallel()
		// API returns 70% (below default 80) but above configured 65.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(makeBody("70", "10")))
		}))
		defer server.Close()

		dir := t.TempDir()
		authFile := filepath.Join(dir, "auth.json")
		if err := os.WriteFile(authFile, []byte(`{"openai.access":"current-token"}`), 0o600); err != nil {
			t.Fatalf("write auth: %v", err)
		}
		accountsFile := filepath.Join(dir, "accounts.json")
		writeStore(t, accountsFile,
			map[string]any{"user_id": "user-current", "access": "current-token"},
			map[string]any{"user_id": "user-alt", "access": "alt-token"},
		)
		configFile := filepath.Join(dir, "config.json")
		writeConfigFile(t, configFile, `{"5h_threshold":65}`)

		var out strings.Builder
		err := runWithArgs(context.Background(), config{
			AuthFile:     authFile,
			AccountsFile: accountsFile,
			UsageURL:     server.URL,
			HTTPClient:   server.Client(),
			ConfigFile:   configFile,
		}, &out, nil)
		if err != nil {
			t.Fatalf("runWithArgs: %v", err)
		}
		// Rotation switched the active account to user-alt; auth.json now points to alt.
		data, _ := os.ReadFile(authFile)
		if !strings.Contains(string(data), "alt-token") {
			t.Fatalf("expected rotation to alt-token, got auth=%s", string(data))
		}
	})

	t.Run("raised weekly threshold blocks rotation in 5h-off path", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fmt.Sprintf(
				`{"user_id":"user-current","rate_limit":{"primary_window":{"used_percent":99,"reset_at":%d}}}`,
				resetAt,
			)))
		}))
		defer server.Close()

		dir := t.TempDir()
		authFile := filepath.Join(dir, "auth.json")
		if err := os.WriteFile(authFile, []byte(`{"openai.access":"current-token"}`), 0o600); err != nil {
			t.Fatalf("write auth: %v", err)
		}
		accountsFile := filepath.Join(dir, "accounts.json")
		writeStore(t, accountsFile,
			map[string]any{"user_id": "user-current", "access": "current-token"},
			map[string]any{"user_id": "user-alt", "access": "alt-token"},
		)
		configFile := filepath.Join(dir, "config.json")
		writeConfigFile(t, configFile, `{"5h":"off","weekly_threshold":99.5}`)

		var out strings.Builder
		err := runWithArgs(context.Background(), config{
			AuthFile:     authFile,
			AccountsFile: accountsFile,
			UsageURL:     server.URL,
			HTTPClient:   server.Client(),
			ConfigFile:   configFile,
		}, &out, nil)
		if err != nil {
			t.Fatalf("runWithArgs: %v", err)
		}
		// 99 < 99.5: no rotation, no cooldown.
		store := mustReadStore(t, accountsFile)
		entry := store["user-current"]
		if _, has := entry["cooldownUntil"]; has {
			t.Fatalf("expected no cooldown when below configured weekly threshold, got %v", entry)
		}
	})

	t.Run("lowered weekly threshold forces cooldown in 5h-off path", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fmt.Sprintf(
				`{"user_id":"user-current","rate_limit":{"primary_window":{"used_percent":96,"reset_at":%d}}}`,
				resetAt,
			)))
		}))
		defer server.Close()

		dir := t.TempDir()
		authFile := filepath.Join(dir, "auth.json")
		if err := os.WriteFile(authFile, []byte(`{"openai.access":"current-token"}`), 0o600); err != nil {
			t.Fatalf("write auth: %v", err)
		}
		accountsFile := filepath.Join(dir, "accounts.json")
		writeStore(t, accountsFile,
			map[string]any{"user_id": "user-current", "access": "current-token"},
			map[string]any{"user_id": "user-alt", "access": "alt-token"},
		)
		configFile := filepath.Join(dir, "config.json")
		writeConfigFile(t, configFile, `{"5h":"off","weekly_threshold":95}`)

		var out strings.Builder
		err := runWithArgs(context.Background(), config{
			AuthFile:     authFile,
			AccountsFile: accountsFile,
			UsageURL:     server.URL,
			HTTPClient:   server.Client(),
			ConfigFile:   configFile,
		}, &out, nil)
		if err != nil {
			t.Fatalf("runWithArgs: %v", err)
		}
		store := mustReadStore(t, accountsFile)
		entry := store["user-current"]
		got, ok := valueToInt64(entry["cooldownUntil"])
		if !ok || got != resetAt {
			t.Fatalf("expected cooldownUntil=%d, got %v", resetAt, entry["cooldownUntil"])
		}
	})
}

// TestAccountWithUsageHonorsCustomThresholds locks the per-call threshold
// override: accountWithUsage must use the thresholds passed in (so callers
// can wire the runtime-configured values) instead of hardcoded constants.
func TestAccountWithUsageHonorsCustomThresholds(t *testing.T) {
	t.Parallel()

	resetAt := int64(1777014899)
	account := map[string]any{
		"user_id": "user-x",
		"access":  "x-token",
	}

	t.Run("weekly 90 sets cooldown when usedPercent=95", func(t *testing.T) {
		t.Parallel()
		out := accountWithUsage(account, usageWindow{
			UsedPercent: "95",
			ResetAt:     int64Ptr(resetAt),
			UserID:      "user-x",
		}, false, rotationThresholds{FiveHour: defaultFiveHourThreshold, Weekly: 90})
		if got, ok := valueToInt64(out["cooldownUntil"]); !ok || got != resetAt {
			t.Fatalf("expected cooldownUntil=%d, got %v", resetAt, out["cooldownUntil"])
		}
	})

	t.Run("weekly 99 does NOT set cooldown when usedPercent=95", func(t *testing.T) {
		t.Parallel()
		out := accountWithUsage(account, usageWindow{
			UsedPercent: "95",
			ResetAt:     int64Ptr(resetAt),
			UserID:      "user-x",
		}, false, rotationThresholds{FiveHour: defaultFiveHourThreshold, Weekly: 99})
		if _, has := out["cooldownUntil"]; has {
			t.Fatalf("expected no cooldown below custom 99%% threshold, got %v", out["cooldownUntil"])
		}
	})
}

// (table-driven rejection test for out-of-range threshold writes pre-seeds the
// file with both the 5h toggle and a custom key, so no sentinel is required.)

func TestHideOwnConsoleWindowIsSafeToCall(t *testing.T) {
	t.Parallel()

	// Guards the build-tag pairing: exactly one implementation must compile on
	// every platform, and calling it must never panic or affect a console the
	// test process shares with its shell.
	hideOwnConsoleWindow()
}

func TestRunAccountsCommandListsStoreWhenOpenCodeAuthMissing(t *testing.T) {
	t.Parallel()

	fixedNow := time.Unix(1_700_000_000, 0)
	reset := fixedNow.Unix() + (45 * 60)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"user_id":"user-other","email":"other@example.com","rate_limit":{"primary_window":{"used_percent":88,"reset_at":` + strconv.FormatInt(reset, 10) + `}}}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	accountsFile := filepath.Join(dir, "accounts.json")
	encoded, err := json.Marshal(map[string]map[string]any{
		"user-other": {"user_id": "user-other", "access": "other-token"},
	})
	if err != nil {
		t.Fatalf("marshal store: %v", err)
	}
	if err := os.WriteFile(accountsFile, encoded, 0o600); err != nil {
		t.Fatalf("write accounts file: %v", err)
	}

	var out strings.Builder
	err = runAccountsCommand(context.Background(), config{
		// OpenCode is not installed: the path exists in config but not on disk.
		AuthFile:     filepath.Join(dir, "missing", "auth.json"),
		AccountsFile: accountsFile,
		UsageURL:     server.URL,
		HTTPClient:   server.Client(),
		Now:          func() time.Time { return fixedNow },
	}, &out)
	if err != nil {
		t.Fatalf("runAccountsCommand returned error: %v", err)
	}

	if !strings.Contains(out.String(), "other@example.com") {
		t.Fatalf("expected the saved account to be listed, got %q", out.String())
	}
	if strings.Contains(out.String(), "*") {
		t.Fatalf("no account should be marked current without OpenCode auth, got %q", out.String())
	}
}

func TestActivateAccountSkipsProvidersThatAreNotInstalled(t *testing.T) {
	t.Parallel()

	t.Run("missing OpenCode auth file still syncs Pi", func(t *testing.T) {
		t.Parallel()
		oc, pi, _ := piSyncFixture(t)
		seedAuthFile(t, pi, `{"anthropic":{"access":"anth"}}`)

		if err := activateAccount(config{AuthFile: oc, PiAuthFile: pi}, fixturePiAccount()); err != nil {
			t.Fatalf("activateAccount: %v", err)
		}
		assertPiCodex(t, readJSONObject(t, pi), "fixture-access-token", "fixture-refresh-token", 1777014899000, "acct-1")
		if _, err := os.Stat(oc); !os.IsNotExist(err) {
			t.Fatalf("OpenCode auth file must not be created when OpenCode is absent (stat err: %v)", err)
		}
	})

	t.Run("missing Pi directory still syncs OpenCode", func(t *testing.T) {
		t.Parallel()
		oc, pi, _ := piSyncFixture(t)
		seedAuthFile(t, oc, `{"other":"keep"}`)
		absentPi := filepath.Join(filepath.Dir(pi), "no-pi-here", "auth.json")

		if err := activateAccount(config{AuthFile: oc, PiAuthFile: absentPi}, fixturePiAccount()); err != nil {
			t.Fatalf("activateAccount: %v", err)
		}
		if got := readJSONObject(t, oc)["openai.access"]; got != "fixture-access-token" {
			t.Fatalf("OpenCode tuple not updated, got %v", got)
		}
		if _, err := os.Stat(filepath.Dir(absentPi)); !os.IsNotExist(err) {
			t.Fatalf("Pi directory must not be created when Pi is absent (stat err: %v)", err)
		}
	})

	t.Run("both providers absent reports the shared not-installed error", func(t *testing.T) {
		t.Parallel()
		oc, pi, _ := piSyncFixture(t)
		absentPi := filepath.Join(filepath.Dir(pi), "no-pi-here", "auth.json")

		err := activateAccount(config{AuthFile: oc, PiAuthFile: absentPi}, fixturePiAccount())
		if !errors.Is(err, errNeitherProviderAvailable) {
			t.Fatalf("expected errNeitherProviderAvailable, got %v", err)
		}
	})
}

// TestRunAccountsCommandMarksCurrentFromPiWhenOpenCodeAbsent covers the
// Pi-only install (e.g. a VPS with Pi Agent but no OpenCode): the CURRENT
// marker must still identify the account in use. The match key is exact
// access-token equality against the saved store, which needs no extra
// usage request.
func TestRunAccountsCommandMarksCurrentFromPiWhenOpenCodeAbsent(t *testing.T) {
	t.Parallel()

	fixedNow := time.Unix(1_700_000_000, 0)
	reset := fixedNow.Unix() + (45 * 60)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := "user-a"
		email := "a@example.com"
		if strings.Contains(r.Header.Get("Authorization"), "pi-token") {
			userID = "user-b"
			email = "b@example.com"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"user_id":"` + userID + `","email":"` + email +
			`","rate_limit":{"primary_window":{"used_percent":31,"reset_at":` +
			strconv.FormatInt(reset, 10) + `}}}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	accountsFile := filepath.Join(dir, "accounts.json")
	encoded, err := json.Marshal(map[string]map[string]any{
		"user-a": {"user_id": "user-a", "access": "other-token"},
		"user-b": {"user_id": "user-b", "access": "pi-token"},
	})
	if err != nil {
		t.Fatalf("marshal store: %v", err)
	}
	if err := os.WriteFile(accountsFile, encoded, 0o600); err != nil {
		t.Fatalf("write accounts file: %v", err)
	}

	piAuthFile := filepath.Join(dir, "pi-auth.json")
	seedAuthFile(t, piAuthFile, `{"openai-codex":{"type":"oauth","access":"pi-token",`+
		`"refresh":"pi-refresh","expires":1777014899000,"accountId":"acct-b"}}`)

	var out strings.Builder
	err = runAccountsCommand(context.Background(), config{
		// OpenCode is not installed: the path exists in config but not on disk.
		AuthFile:     filepath.Join(dir, "missing", "auth.json"),
		PiAuthFile:   piAuthFile,
		AccountsFile: accountsFile,
		UsageURL:     server.URL,
		HTTPClient:   server.Client(),
		Now:          func() time.Time { return fixedNow },
	}, &out)
	if err != nil {
		t.Fatalf("runAccountsCommand returned error: %v", err)
	}

	rendered := out.String()
	if !strings.Contains(rendered, "b@example.com") {
		t.Fatalf("expected the Pi account to be listed, got %q", rendered)
	}
	marked := currentMarkedEmails(t, rendered)
	if len(marked) != 1 || marked[0] != "b@example.com" {
		t.Fatalf("expected only b@example.com marked current from Pi, got %v in %q", marked, rendered)
	}
}

// TestRunAccountsCommandPrefersOpenCodeOverPiForCurrent pins the precedence:
// when OpenCode auth is usable it decides CURRENT, and Pi is never consulted.
func TestRunAccountsCommandPrefersOpenCodeOverPiForCurrent(t *testing.T) {
	t.Parallel()

	fixedNow := time.Unix(1_700_000_000, 0)
	reset := fixedNow.Unix() + (45 * 60)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := "user-a"
		email := "a@example.com"
		if strings.Contains(r.Header.Get("Authorization"), "pi-token") {
			userID = "user-b"
			email = "b@example.com"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"user_id":"` + userID + `","email":"` + email +
			`","rate_limit":{"primary_window":{"used_percent":31,"reset_at":` +
			strconv.FormatInt(reset, 10) + `}}}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	accountsFile := filepath.Join(dir, "accounts.json")
	encoded, err := json.Marshal(map[string]map[string]any{
		"user-a": {"user_id": "user-a", "access": "oc-token"},
		"user-b": {"user_id": "user-b", "access": "pi-token"},
	})
	if err != nil {
		t.Fatalf("marshal store: %v", err)
	}
	if err := os.WriteFile(accountsFile, encoded, 0o600); err != nil {
		t.Fatalf("write accounts file: %v", err)
	}

	authFile := filepath.Join(dir, "auth.json")
	seedAuthFile(t, authFile, `{"openai":{"type":"oauth","access":"oc-token",`+
		`"refresh":"oc-refresh","expires":1777014899000}}`)
	piAuthFile := filepath.Join(dir, "pi-auth.json")
	seedAuthFile(t, piAuthFile, `{"openai-codex":{"type":"oauth","access":"pi-token",`+
		`"refresh":"pi-refresh","expires":1777014899000,"accountId":"acct-b"}}`)

	var out strings.Builder
	err = runAccountsCommand(context.Background(), config{
		AuthFile:     authFile,
		PiAuthFile:   piAuthFile,
		AccountsFile: accountsFile,
		UsageURL:     server.URL,
		HTTPClient:   server.Client(),
		Now:          func() time.Time { return fixedNow },
	}, &out)
	if err != nil {
		t.Fatalf("runAccountsCommand returned error: %v", err)
	}

	rendered := out.String()
	marked := currentMarkedEmails(t, rendered)
	if len(marked) != 1 || marked[0] != "a@example.com" {
		t.Fatalf("expected only a@example.com marked current from OpenCode, got %v in %q", marked, rendered)
	}
}

// currentMarkedEmails extracts the EMAIL cell of every rendered row whose
// CURRENT column holds the `*` marker.
func currentMarkedEmails(t *testing.T, rendered string) []string {
	t.Helper()
	marked := make([]string, 0, 2)
	for _, line := range strings.Split(rendered, "\n") {
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if len(cells) < 3 || strings.TrimSpace(cells[1]) != "*" {
			continue
		}
		marked = append(marked, strings.TrimSpace(cells[2]))
	}
	return marked
}

// TestCurrentAccountTokenFallsBackToPiWhenOpenCodeTokenIsEmpty pins the
// fallthrough at the OpenCode branch: an OpenCode auth file that parses but
// yields a blank access token is NOT a usable provider, so Pi must decide.
// Without this the empty-token guard could silently return "" and drop the
// CURRENT marker on a host that has both providers.
func TestCurrentAccountTokenFallsBackToPiWhenOpenCodeTokenIsEmpty(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth.json")
	seedAuthFile(t, authFile, `{"openai":{"type":"oauth","access":"   ",`+
		`"refresh":"oc-refresh","expires":1777014899000}}`)
	piAuthFile := filepath.Join(dir, "pi-auth.json")
	seedAuthFile(t, piAuthFile, `{"openai-codex":{"type":"oauth","access":"pi-token",`+
		`"refresh":"pi-refresh","expires":1777014899000,"accountId":"acct-b"}}`)

	if got := currentAccountToken(config{AuthFile: authFile, PiAuthFile: piAuthFile}); got != "pi-token" {
		t.Fatalf("expected the Pi token when the OpenCode token is blank, got %q", got)
	}
}

// TestCurrentAccountTokenReturnsEmptyForUnusablePi covers every way Pi can
// fail to yield a token. Each case must return "" so no row is marked, never
// a partial or garbage token that would mark the WRONG account as current.
func TestCurrentAccountTokenReturnsEmptyForUnusablePi(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		content string
	}{
		{"malformed JSON", `{"openai-codex":`},
		{"missing openai-codex entry", `{"anthropic":{"access":"anth"}}`},
		{"null openai-codex entry", `{"openai-codex":null}`},
		{"entry is not an object", `{"openai-codex":"not-an-object"}`},
		{"blank access token", `{"openai-codex":{"type":"oauth","access":"",` +
			`"refresh":"pi-refresh","expires":1777014899000,"accountId":"acct-b"}}`},
		{"missing refresh token", `{"openai-codex":{"type":"oauth","access":"pi-token",` +
			`"expires":1777014899000,"accountId":"acct-b"}}`},
		{"non-oauth type", `{"openai-codex":{"type":"api","access":"pi-token",` +
			`"refresh":"pi-refresh","expires":1777014899000,"accountId":"acct-b"}}`},
		{"non-integer expires", `{"openai-codex":{"type":"oauth","access":"pi-token",` +
			`"refresh":"pi-refresh","expires":"soon","accountId":"acct-b"}}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			piAuthFile := filepath.Join(dir, "pi-auth.json")
			seedAuthFile(t, piAuthFile, tc.content)

			got := currentAccountToken(config{
				// OpenCode absent, so Pi is the only possible source.
				AuthFile:   filepath.Join(dir, "missing", "auth.json"),
				PiAuthFile: piAuthFile,
			})
			if got != "" {
				t.Fatalf("expected no token for an unusable Pi entry, got %q", got)
			}
		})
	}
}

// TestCurrentAccountTokenIsReadOnly backs the documented guarantee that
// listing accounts never mutates anything. The README and the function
// comment both promise this; without an assertion the promise can rot into a
// silent side effect that creates Pi state on a host where Pi is absent.
func TestCurrentAccountTokenIsReadOnly(t *testing.T) {
	t.Parallel()

	t.Run("absent Pi creates neither the file nor its directory", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		absentPi := filepath.Join(dir, "no-pi-here", "auth.json")

		if got := currentAccountToken(config{
			AuthFile:   filepath.Join(dir, "missing", "auth.json"),
			PiAuthFile: absentPi,
		}); got != "" {
			t.Fatalf("expected no token when both providers are absent, got %q", got)
		}
		if _, err := os.Stat(filepath.Dir(absentPi)); !os.IsNotExist(err) {
			t.Fatalf("Pi directory must not be created (stat err: %v)", err)
		}
		if _, err := os.Stat(absentPi); !os.IsNotExist(err) {
			t.Fatalf("Pi auth file must not be created (stat err: %v)", err)
		}
	})

	t.Run("present Pi is left byte-identical and unlocked", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		piAuthFile := filepath.Join(dir, "pi-auth.json")
		original := `{"openai-codex":{"type":"oauth","access":"pi-token",` +
			`"refresh":"pi-refresh","expires":1777014899000,"accountId":"acct-b"},` +
			`"minimax":{"access":"keep-me"}}`
		seedAuthFile(t, piAuthFile, original)

		if got := currentAccountToken(config{
			AuthFile:   filepath.Join(dir, "missing", "auth.json"),
			PiAuthFile: piAuthFile,
		}); got != "pi-token" {
			t.Fatalf("expected the Pi token, got %q", got)
		}

		after, err := os.ReadFile(piAuthFile)
		if err != nil {
			t.Fatalf("read Pi auth file: %v", err)
		}
		if string(after) != original {
			t.Fatalf("Pi auth file was rewritten:\n got %s\nwant %s", after, original)
		}
		// The read takes the proper-lockfile lock; it must also release it,
		// or the next writer deadlocks against a stale lock directory.
		if _, err := os.Stat(piAuthFile + ".lock"); !os.IsNotExist(err) {
			t.Fatalf("Pi lock must be released (stat err: %v)", err)
		}
	})

	t.Run("absent OpenCode creates neither the file nor its directory", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		absentOC := filepath.Join(dir, "no-opencode", "auth.json")
		piAuthFile := filepath.Join(dir, "pi-auth.json")
		seedAuthFile(t, piAuthFile, `{"openai-codex":{"type":"oauth","access":"pi-token",`+
			`"refresh":"pi-refresh","expires":1777014899000,"accountId":"acct-b"}}`)

		if got := currentAccountToken(config{AuthFile: absentOC, PiAuthFile: piAuthFile}); got != "pi-token" {
			t.Fatalf("expected the Pi token, got %q", got)
		}
		if _, err := os.Stat(filepath.Dir(absentOC)); !os.IsNotExist(err) {
			t.Fatalf("OpenCode directory must not be created (stat err: %v)", err)
		}
	})
}
