package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
		_, _ = w.Write([]byte(`{"user_id":"user-current","rate_limit":{"primary_window":{"used_percent":80,"reset_at":` + strconv.FormatInt(resetAt, 10) + `}}}`))
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

	if out.String() != "80" {
		t.Fatalf("expected only used_percent value, got %q", out.String())
	}

	store := mustReadStore(t, accountsFile)
	acct := store["user-current"]
	if got, _ := acct["usedPercent"].(string); got != "80" {
		t.Fatalf("expected usedPercent 80, got %v", acct["usedPercent"])
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

	selected, ok := selectEligibleAlternateAccount(store, "user-current", now)
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"user_id":"user-current","rate_limit":{"primary_window":{"used_percent":95,"reset_at":` + strconv.FormatInt(now+7200, 10) + `}}}`))
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

	if out.String() != "95" {
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"user_id":"user-current","rate_limit":{"primary_window":{"used_percent":95,"reset_at":` + strconv.FormatInt(resetAt, 10) + `}}}`))
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

	if out.String() != "95" {
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
	currentWeeklyReset := fixedNow.Unix() + (5 * 24 * 60 * 60) + (12 * 60 * 60)
	otherWeeklyReset := fixedNow.Unix() + (3 * 24 * 60 * 60) + (4 * 60 * 60)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch auth {
		case "Bearer current-token":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"user_id":"user-current","email":"current@example.com","rate_limit":{"primary_window":{"used_percent":22.5,"reset_at":` + strconv.FormatInt(currentReset, 10) + `},"secondary_window":{"used_percent":2.5,"reset_at":` + strconv.FormatInt(currentWeeklyReset, 10) + `}}}`))
		case "Bearer other-token":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"user_id":"user-other","email":"other@example.com","rate_limit":{"primary_window":{"used_percent":88,"reset_at":` + strconv.FormatInt(otherReset, 10) + `},"secondary_window":{"used_percent":12,"reset_at":` + strconv.FormatInt(otherWeeklyReset, 10) + `}}}`))
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
		!strings.Contains(printed, "EMAIL") || !strings.Contains(printed, "USED%") ||
		!strings.Contains(printed, "WEEK%") || !strings.Contains(printed, "RESET") ||
		!strings.Contains(printed, "WEEK-RESET") {
		t.Fatalf("expected Markdown-style table header with all columns, got %q", printed)
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
		t.Fatalf("expected primary used_percent values in output, got %q", printed)
	}
	if !strings.Contains(printed, "2.5") || !strings.Contains(printed, "12") {
		t.Fatalf("expected secondary used_percent values in output, got %q", printed)
	}
	if !strings.Contains(printed, "2h 15m") {
		t.Fatalf("expected primary remaining duration 2h 15m in output, got %q", printed)
	}
	if !strings.Contains(printed, "1d 3h") {
		t.Fatalf("expected primary remaining duration 1d 3h in output, got %q", printed)
	}
	if !strings.Contains(printed, "5d 12h") {
		t.Fatalf("expected weekly remaining duration 5d 12h in output, got %q", printed)
	}
	if !strings.Contains(printed, "3d 4h") {
		t.Fatalf("expected weekly remaining duration 3d 4h in output, got %q", printed)
	}
	if strings.Contains(printed, strconv.FormatInt(currentReset, 10)) || strings.Contains(printed, strconv.FormatInt(otherReset, 10)) {
		t.Fatalf("did not expect reset unix timestamps in output, got %q", printed)
	}
	if strings.Contains(printed, strconv.FormatInt(currentWeeklyReset, 10)) || strings.Contains(printed, strconv.FormatInt(otherWeeklyReset, 10)) {
		t.Fatalf("did not expect weekly reset unix timestamps in output, got %q", printed)
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
	if got, _ := currentEntry["secondaryUsedPercent"].(string); got != "2.5" {
		t.Fatalf("expected persisted secondaryUsedPercent 2.5, got %v", currentEntry["secondaryUsedPercent"])
	}
	if got, ok := valueToInt64(currentEntry["secondaryResetAt"]); !ok || got != currentWeeklyReset {
		t.Fatalf("expected persisted secondaryResetAt %d, got %v", currentWeeklyReset, currentEntry["secondaryResetAt"])
	}
}

func TestPrintAccountsTableRendersMarkdownStyleWithSeparator(t *testing.T) {
	t.Parallel()

	rows := []accountUsageRow{
		{
			Current:               true,
			Index:                 1,
			Email:                 "current@example.com",
			UsedPercent:           "22.5",
			SecondaryUsedPercent:  "2.5",
			ResetDisplay:          "2h 15m",
			SecondaryResetDisplay: "5d 12h",
		},
		{
			Current:               false,
			Index:                 2,
			Email:                 "other@example.com",
			UsedPercent:           "88",
			SecondaryUsedPercent:  "12",
			ResetDisplay:          "1d 3h",
			SecondaryResetDisplay: "3d 4h",
		},
	}

	var out strings.Builder
	if err := printAccountsTable(&out, rows); err != nil {
		t.Fatalf("printAccountsTable returned error: %v", err)
	}

	printed := out.String()

	// Header row: `|` boundaries around each column.
	if !strings.Contains(printed, "| ID |") {
		t.Fatalf("expected header to start with `| ID |`, got %q", printed)
	}
	if !strings.Contains(printed, "| WEEK-RESET |") {
		t.Fatalf("expected header to end with `| WEEK-RESET |`, got %q", printed)
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

	// Sanity: used_percent and weekly values are present and no longer joined
	// to the header with double spaces (which the old format produced).
	if !strings.Contains(printed, "22.5") || !strings.Contains(printed, "88") {
		t.Fatalf("expected primary used_percent values in output, got %q", printed)
	}
	if !strings.Contains(printed, "2.5") || !strings.Contains(printed, "12") {
		t.Fatalf("expected secondary used_percent values in output, got %q", printed)
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

func TestRunRotatesWhenSecondaryUsageExhausted(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()
	primaryReset := now + 3600
	weeklyReset := now + (5 * 24 * 60 * 60)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch auth {
		case "Bearer current-token":
			// Primary window is healthy but the weekly window is exhausted.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"user_id":"user-current","rate_limit":{"primary_window":{"used_percent":35,"reset_at":` + strconv.FormatInt(primaryReset, 10) + `},"secondary_window":{"used_percent":98.5,"reset_at":` + strconv.FormatInt(weeklyReset, 10) + `}}}`))
		case "Bearer other-token":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"user_id":"user-other","rate_limit":{"primary_window":{"used_percent":40,"reset_at":` + strconv.FormatInt(now+7200, 10) + `},"secondary_window":{"used_percent":10,"reset_at":` + strconv.FormatInt(now+7*24*60*60, 10) + `}}}`))
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

	// Default stdout contract is preserved: only primary used_percent.
	if out.String() != "35" {
		t.Fatalf("expected only used_percent value 35, got %q", out.String())
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

func TestRunDoesNotRotateWhenSecondaryBelowThreshold(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"user_id":"user-current","rate_limit":{"primary_window":{"used_percent":30,"reset_at":` + strconv.FormatInt(now+3600, 10) + `},"secondary_window":{"used_percent":50,"reset_at":` + strconv.FormatInt(now+5*24*60*60, 10) + `}}}`))
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
		t.Fatalf("expected auth unchanged when neither window is exhausted, got %s", string(authAfter))
	}
}

func TestSelectEligibleAlternateAccountSkipsPrimaryAndSecondaryExhausted(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()
	store := map[string]map[string]any{
		"user-current": {
			"user_id":   "user-current",
			"accountId": "acct-current",
			"access":    "current-token",
		},
		"user-primary-hot": {
			"user_id":       "user-primary-hot",
			"accountId":     "acct-primary-hot",
			"access":        "primary-hot-token",
			"usedPercent":   "85",
			"resetAt":       now + 3600,
			"cooldownUntil": now + 3600,
		},
		"user-weekly-hot": {
			"user_id":              "user-weekly-hot",
			"accountId":            "acct-weekly-hot",
			"access":               "weekly-hot-token",
			"usedPercent":          "20",
			"secondaryUsedPercent": "99",
			"secondaryResetAt":     now + 3600,
		},
		"user-ready": {
			"user_id":       "user-ready",
			"accountId":     "acct-ready",
			"access":        "ready-token",
			"usedPercent":   "15",
			"cooldownUntil": now - 1,
		},
	}

	selected, ok := selectEligibleAlternateAccount(store, "user-current", now)
	if !ok {
		t.Fatalf("expected an eligible alternate account")
	}
	if got, _ := selected["user_id"].(string); got != "user-ready" {
		t.Fatalf("expected user-ready (both hot candidates must be skipped), got %q", got)
	}
}

func TestSelectEligibleAlternateAccountAllowsMissingSecondaryData(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()
	store := map[string]map[string]any{
		"user-current": {
			"user_id":   "user-current",
			"accountId": "acct-current",
			"access":    "current-token",
		},
		// Candidate with NO secondaryUsedPercent recorded: must remain
		// eligible (conservative: we never block on absent data).
		"user-unknown-weekly": {
			"user_id":     "user-unknown-weekly",
			"accountId":   "acct-unknown-weekly",
			"access":      "unknown-weekly-token",
			"usedPercent": "20",
		},
	}

	selected, ok := selectEligibleAlternateAccount(store, "user-current", now)
	if !ok {
		t.Fatalf("expected an eligible alternate account even with missing weekly data")
	}
	if got, _ := selected["user_id"].(string); got != "user-unknown-weekly" {
		t.Fatalf("expected user-unknown-weekly, got %q", got)
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
		// Candidate with stale high primary usage: reset has already
		// passed, so the account is eligible again. Without this check the
		// account would stay out of rotation forever.
		"user-recovered-primary": {
			"user_id":     "user-recovered-primary",
			"accountId":   "acct-recovered-primary",
			"access":      "recovered-primary-token",
			"usedPercent": "85",
			"resetAt":     now - 1,
		},
	}

	selected, ok := selectEligibleAlternateAccount(store, "user-current", now)
	if !ok {
		t.Fatalf("expected candidate with expired reset to be eligible")
	}
	if got, _ := selected["user_id"].(string); got != "user-recovered-primary" {
		t.Fatalf("expected user-recovered-primary, got %q", got)
	}
}

func TestSelectEligibleAlternateAccountEligibleAfterWeeklyResetExpires(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()
	store := map[string]map[string]any{
		"user-current": {
			"user_id":   "user-current",
			"accountId": "acct-current",
			"access":    "current-token",
		},
		// Candidate with stale high weekly usage: secondary reset has
		// already passed, so the account is eligible again.
		"user-recovered-weekly": {
			"user_id":              "user-recovered-weekly",
			"accountId":            "acct-recovered-weekly",
			"access":               "recovered-weekly-token",
			"usedPercent":          "20",
			"secondaryUsedPercent": "99",
			"secondaryResetAt":     now - 1,
		},
	}

	selected, ok := selectEligibleAlternateAccount(store, "user-current", now)
	if !ok {
		t.Fatalf("expected candidate with expired weekly reset to be eligible")
	}
	if got, _ := selected["user_id"].(string); got != "user-recovered-weekly" {
		t.Fatalf("expected user-recovered-weekly, got %q", got)
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
		// Candidate with high primary usage and NO recorded reset at all.
		// Mirrors the conservative "never block on missing data" rule used
		// elsewhere: the account is eligible.
		"user-no-reset": {
			"user_id":     "user-no-reset",
			"accountId":   "acct-no-reset",
			"access":      "no-reset-token",
			"usedPercent": "85",
		},
	}

	selected, ok := selectEligibleAlternateAccount(store, "user-current", now)
	if !ok {
		t.Fatalf("expected candidate with missing reset to be eligible")
	}
	if got, _ := selected["user_id"].(string); got != "user-no-reset" {
		t.Fatalf("expected user-no-reset, got %q", got)
	}
}

func TestSelectEligibleAlternateAccountSkipsPrimaryHotWithFutureReset(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()
	store := map[string]map[string]any{
		"user-current": {
			"user_id":   "user-current",
			"accountId": "acct-current",
			"access":    "current-token",
		},
		// Candidate with FRESH high primary usage: reset is in the future.
		"user-primary-hot": {
			"user_id":     "user-primary-hot",
			"accountId":   "acct-primary-hot",
			"access":      "primary-hot-token",
			"usedPercent": "85",
			"resetAt":     now + 3600,
		},
		"user-ready": {
			"user_id":     "user-ready",
			"accountId":   "acct-ready",
			"access":      "ready-token",
			"usedPercent": "15",
		},
	}

	selected, ok := selectEligibleAlternateAccount(store, "user-current", now)
	if !ok {
		t.Fatalf("expected an eligible alternate account")
	}
	if got, _ := selected["user_id"].(string); got != "user-ready" {
		t.Fatalf("expected user-ready (primary-hot with future reset must be skipped), got %q", got)
	}
}

func TestSelectEligibleAlternateAccountSkipsWeeklyHotWithFutureReset(t *testing.T) {
	t.Parallel()

	now := time.Now().Unix()
	store := map[string]map[string]any{
		"user-current": {
			"user_id":   "user-current",
			"accountId": "acct-current",
			"access":    "current-token",
		},
		// Candidate with FRESH high weekly usage: secondary reset is in
		// the future.
		"user-weekly-hot": {
			"user_id":              "user-weekly-hot",
			"accountId":            "acct-weekly-hot",
			"access":               "weekly-hot-token",
			"usedPercent":          "20",
			"secondaryUsedPercent": "99",
			"secondaryResetAt":     now + 3600,
		},
		"user-ready": {
			"user_id":     "user-ready",
			"accountId":   "acct-ready",
			"access":      "ready-token",
			"usedPercent": "15",
		},
	}

	selected, ok := selectEligibleAlternateAccount(store, "user-current", now)
	if !ok {
		t.Fatalf("expected an eligible alternate account")
	}
	if got, _ := selected["user_id"].(string); got != "user-ready" {
		t.Fatalf("expected user-ready (weekly-hot with future reset must be skipped), got %q", got)
	}
}

func TestAccountWithUsagePersistsSecondaryCooldownWhenPrimaryHealthy(t *testing.T) {
	t.Parallel()

	primaryReset := int64(1777014899)
	secondaryReset := int64(1777619699)

	account := map[string]any{
		"user_id":   "user-secondary",
		"accountId": "acct-secondary",
		"access":    "secondary-token",
	}

	updated := accountWithUsage(account, usageWindow{
		UsedPercent:          "35",
		SecondaryUsedPercent: "98.5",
		ResetAt:              int64Ptr(primaryReset),
		SecondaryResetAt:     int64Ptr(secondaryReset),
		UserID:               "user-secondary",
	})

	if got, ok := valueToInt64(updated["cooldownUntil"]); !ok || got != secondaryReset {
		t.Fatalf("expected cooldownUntil to be the secondary reset %d (primary was healthy), got %v", secondaryReset, updated["cooldownUntil"])
	}
}

func TestAccountWithUsageKeepsPrimaryCooldownWhenPrimaryExhausted(t *testing.T) {
	t.Parallel()

	primaryReset := int64(1777014899)
	secondaryReset := int64(1777619699)

	account := map[string]any{
		"user_id":   "user-primary",
		"accountId": "acct-primary",
		"access":    "primary-token",
	}

	updated := accountWithUsage(account, usageWindow{
		UsedPercent:          "85",
		SecondaryUsedPercent: "50",
		ResetAt:              int64Ptr(primaryReset),
		SecondaryResetAt:     int64Ptr(secondaryReset),
		UserID:               "user-primary",
	})

	if got, ok := valueToInt64(updated["cooldownUntil"]); !ok || got != primaryReset {
		t.Fatalf("expected cooldownUntil to be the primary reset %d, got %v", primaryReset, updated["cooldownUntil"])
	}
}

func TestAccountWithUsageMissingSecondaryDoesNotSetCooldown(t *testing.T) {
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
		UsedPercent:          "35",
		ResetAt:              int64Ptr(primaryReset),
		SecondaryUsedPercent: "",
		UserID:               "user-no-secondary",
	})

	if _, exists := updated["cooldownUntil"]; exists {
		t.Fatalf("expected cooldownUntil to be cleared when both windows are healthy, got %v", updated["cooldownUntil"])
	}
}

func TestAccountWithUsageKeepsLaterResetWhenBothWindowsExhausted(t *testing.T) {
	t.Parallel()

	account := map[string]any{
		"user_id":   "user-both",
		"accountId": "acct-both",
		"access":    "both-token",
	}

	// Case 1: primary resets sooner, secondary is later — keep the later
	// (secondary) reset.
	earlierPrimary := int64(1777014899)
	laterWeekly := int64(1777619699)

	updated := accountWithUsage(account, usageWindow{
		UsedPercent:          "85",
		SecondaryUsedPercent: "98.5",
		ResetAt:              int64Ptr(earlierPrimary),
		SecondaryResetAt:     int64Ptr(laterWeekly),
		UserID:               "user-both",
	})

	if got, ok := valueToInt64(updated["cooldownUntil"]); !ok || got != laterWeekly {
		t.Fatalf("expected cooldownUntil to be the later (secondary) reset %d, got %v", laterWeekly, updated["cooldownUntil"])
	}

	// Case 2: secondary resets sooner, primary is later — keep the later
	// (primary) reset.
	earlierWeekly := int64(1777014899)
	laterPrimary := int64(1777619699)

	updated = accountWithUsage(account, usageWindow{
		UsedPercent:          "85",
		SecondaryUsedPercent: "98.5",
		ResetAt:              int64Ptr(laterPrimary),
		SecondaryResetAt:     int64Ptr(earlierWeekly),
		UserID:               "user-both",
	})

	if got, ok := valueToInt64(updated["cooldownUntil"]); !ok || got != laterPrimary {
		t.Fatalf("expected cooldownUntil to be the later (primary) reset %d, got %v", laterPrimary, updated["cooldownUntil"])
	}

	// Case 3: equal resets — must still record a single value (no
	// ambiguity).
	equalReset := int64(1777014899)
	updated = accountWithUsage(account, usageWindow{
		UsedPercent:          "85",
		SecondaryUsedPercent: "98.5",
		ResetAt:              int64Ptr(equalReset),
		SecondaryResetAt:     int64Ptr(equalReset),
		UserID:               "user-both",
	})

	if got, ok := valueToInt64(updated["cooldownUntil"]); !ok || got != equalReset {
		t.Fatalf("expected cooldownUntil to be %d when both resets are equal, got %v", equalReset, updated["cooldownUntil"])
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

func TestExtractUsageWindowParsesSecondaryWindow(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"user_id": "user-secondary",
		"email":   "secondary@example.com",
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
		t.Fatalf("unexpected secondary usedPercent: %q", window.SecondaryUsedPercent)
	}
	if window.SecondaryResetAt == nil || *window.SecondaryResetAt != 1777619699 {
		t.Fatalf("expected secondary reset_at 1777619699, got %v", window.SecondaryResetAt)
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
	if window.SecondaryUsedPercent != "" {
		t.Fatalf("expected empty secondary usedPercent when absent, got %q", window.SecondaryUsedPercent)
	}
	if window.SecondaryResetAt != nil {
		t.Fatalf("expected nil secondary reset_at when absent, got %v", window.SecondaryResetAt)
	}
}

func TestExtractUsageWindowRejectsInvalidSecondaryUsedPercent(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"user_id": "user-bad-secondary",
		"rate_limit": map[string]any{
			"primary_window": map[string]any{
				"used_percent": json.Number("22.5"),
			},
			"secondary_window": map[string]any{
				"used_percent": map[string]any{"not": "a number"},
			},
		},
	}

	_, err := extractUsageWindow(payload)
	if err == nil {
		t.Fatalf("expected error for unsupported secondary used_percent type")
	}
	if !strings.Contains(err.Error(), "secondary_window") {
		t.Fatalf("expected error to mention secondary_window, got %v", err)
	}
}

func TestExtractUsageWindowRejectsSecondaryWindowMissingUsedPercent(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"user_id": "user-no-secondary-pct",
		"rate_limit": map[string]any{
			"primary_window": map[string]any{
				"used_percent": json.Number("22.5"),
			},
			"secondary_window": map[string]any{
				"reset_at": json.Number("1777619699"),
			},
		},
	}

	_, err := extractUsageWindow(payload)
	if err == nil {
		t.Fatalf("expected error for secondary_window missing used_percent")
	}
	if !strings.Contains(err.Error(), "secondary_window.used_percent") {
		t.Fatalf("expected error to mention secondary_window.used_percent, got %v", err)
	}
}

func TestExtractUsageWindowRejectsNonObjectSecondaryWindow(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"user_id": "user-string-secondary",
		"rate_limit": map[string]any{
			"primary_window": map[string]any{
				"used_percent": json.Number("22.5"),
			},
			"secondary_window": "not-an-object",
		},
	}

	_, err := extractUsageWindow(payload)
	if err == nil {
		t.Fatalf("expected error when secondary_window is not an object")
	}
	if !strings.Contains(err.Error(), "secondary_window") {
		t.Fatalf("expected error to mention secondary_window, got %v", err)
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
