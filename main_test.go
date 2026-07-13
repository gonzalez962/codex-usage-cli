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

	selected, ok := selectEligibleAlternateAccount(store, "user-current", now)
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

	selected, ok := selectEligibleAlternateAccount(store, "user-current", now)
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

	selected, ok := selectEligibleAlternateAccount(store, "user-current", now)
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

	selected, ok := selectEligibleAlternateAccount(store, "user-current", now)
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
	})

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
	})

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

func TestExtractUsageWindowIgnoresPresentSecondaryWindow(t *testing.T) {
	t.Parallel()

	// Even if upstream sends an object-shaped secondary_window (e.g. an old
	// cached response), the parser must accept it without using the value
	// and without erroring.
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
		t.Fatalf("extractUsageWindow returned error for present secondary_window: %v", err)
	}

	if window.UsedPercent != "11.0" {
		t.Fatalf("unexpected primary usedPercent: %q", window.UsedPercent)
	}
	if window.ResetAt == nil || *window.ResetAt != 1777014899 {
		t.Fatalf("expected primary reset_at 1777014899, got %v", window.ResetAt)
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
