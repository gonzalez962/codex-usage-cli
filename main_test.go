package main

import (
	"context"
	"encoding/json"
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

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch auth {
		case "Bearer current-token":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"user_id":"user-current","email":"current@example.com","rate_limit":{"primary_window":{"used_percent":22.5,"reset_at":` + strconv.FormatInt(currentReset, 10) + `}}}`))
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
	if !strings.Contains(printed, "CURRENT  EMAIL  USED%  RESET") {
		t.Fatalf("expected accounts header, got %q", printed)
	}
	if !strings.Contains(printed, "current@example.com") {
		t.Fatalf("expected current account email in output, got %q", printed)
	}
	if !strings.Contains(printed, "other@example.com") {
		t.Fatalf("expected other account email in output, got %q", printed)
	}
	if !strings.Contains(printed, "22.5") || !strings.Contains(printed, "88") {
		t.Fatalf("expected used_percent values in output, got %q", printed)
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

	currentLine := lineContaining(printed, "current@example.com")
	if !strings.HasPrefix(strings.TrimSpace(currentLine), "*") {
		t.Fatalf("expected current account line to be highlighted, got %q", currentLine)
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

func lineContaining(text, needle string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
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
