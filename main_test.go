package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// TestRunReturnsErrorOnNon401Non200Response is the post-refresh-rework
// regression for non-200 responses on the usage endpoint. The reactive flow
// only kicks in for HTTP 401; every other non-200 status (e.g. 500) bypasses
// the token endpoint and surfaces the existing error verbatim. The error
// must remain value-free (no bearer token, no account metadata).
func TestRunReturnsErrorOnNon401Non200Response(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("server boom"))
	}))
	defer server.Close()

	dir := t.TempDir()
	authFile := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"openai.access":"test-token","openai.accountId":"acct-2","openai.refresh":"test-refresh","openai.expires":4070908800000,"openai.type":"oauth"}`), 0o600); err != nil {
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

	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected status code in error, got: %v", err)
	}
	for _, secret := range []string{"test-token", "test-refresh", "acct-2"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked secret %q: %v", secret, err)
		}
	}
}

// TestRunReturnsClearErrorWhen401MissingRefreshMetadata pins the
// 401-driven reactive path: when the usage endpoint returns 401 and the
// stored credential has no usable refresh metadata, the error is clear,
// value-free, and leaves auth/store bytes unchanged. The CLI must NEVER
// silently succeed or echo credential values.
func TestRunReturnsClearErrorWhen401MissingRefreshMetadata(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
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
		t.Fatalf("expected error from 401 with missing refresh metadata")
	}
	if !strings.Contains(err.Error(), "refresh required but refresh token is missing") {
		t.Fatalf("expected clear value-free refresh-metadata error, got %v", err)
	}
	for _, secret := range []string{"test-token", "acct-2"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked secret %q: %v", secret, err)
		}
	}
	if got := string(mustReadFile(t, authFile)); got != `{"openai.access":"test-token","openai.accountId":"acct-2"}` {
		t.Fatalf("auth file must remain byte-identical, got %q", got)
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
	err = runUseCommand(context.Background(), config{
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
		if err := runUseCommand(context.Background(), config{
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
		if err := runUseCommand(context.Background(), config{
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

	err = runUseCommand(context.Background(), config{
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
			Now: func() time.Time { return time.UnixMilli(1770000000000) },
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
		piSeed := `{"openai-codex":{"access":"keep-pi","refresh":"keep-pi-r","expires":9999999999999,"accountId":"keep-pi-acct"},"anthropic":{"k":"v"}}`
		seedAuthFile(t, pi, piSeed)
		cfg := config{AuthFile: oc, PiAuthFile: pi, AccountsFile: accounts, UsageURL: server.URL, HTTPClient: server.Client(),
			Now: func() time.Time { return time.UnixMilli(1770000000000) }}
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
		cfg := config{AuthFile: authFile, PiAuthFile: piAuthFile, AccountsFile: accountsFile, UsageURL: server.URL, HTTPClient: server.Client(),
			Now: func() time.Time { return time.UnixMilli(1770000000000) }}
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
		cfg := config{AuthFile: authFile, PiAuthFile: piAuthFile, AccountsFile: accountsFile, UsageURL: server.URL, HTTPClient: server.Client(),
			Now: func() time.Time { return time.UnixMilli(1770000000000) }}
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
		cfg := config{AuthFile: authFile, PiAuthFile: piAuthFile, AccountsFile: accountsFile, UsageURL: server.URL, HTTPClient: server.Client(),
			Now: func() time.Time { return time.UnixMilli(1770000000000) }}
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
		if err := runWithArgs(context.Background(), config{AuthFile: authFile, PiAuthFile: piAuthFile, AccountsFile: accountsFile, UsageURL: server.URL, HTTPClient: server.Client(),
			Now: func() time.Time { return time.UnixMilli(1770000000000) }}, &out, nil); err != nil {
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
		if err := runWithArgs(context.Background(), config{AuthFile: authFile, PiAuthFile: piAuthFile, AccountsFile: accountsFile, ConfigFile: cfgFile, UsageURL: server.URL, HTTPClient: server.Client(),
			Now: func() time.Time { return time.UnixMilli(1770000000000) }}, &out, nil); err != nil {
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
		if err := runWithArgs(context.Background(), config{AuthFile: authFile, PiAuthFile: piAuthFile, AccountsFile: accountsFile, UsageURL: server.URL, HTTPClient: server.Client(),
			Now: func() time.Time { return time.UnixMilli(1770000000000) }}, &out, nil); err != nil {
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
			if err := runWithArgs(context.Background(), config{AuthFile: a, PiAuthFile: p, AccountsFile: acc, UsageURL: server.URL, HTTPClient: server.Client(),
				Now: func() time.Time { return time.UnixMilli(1770000000000) }}, &out, tc.args); err != nil {
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
			cfg := config{AuthFile: authFile, PiAuthFile: piAuthFile, AccountsFile: accountsFile, UsageURL: server.URL, HTTPClient: server.Client(),
				Now: func() time.Time { return time.UnixMilli(1770000000000) }}
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
	server, _ := newPiServer(t, piResponses{"pi-a": piPayload("user-cur", "30", "")})
	var out strings.Builder
	if err := runWithArgs(context.Background(), config{AuthFile: authFile, PiAuthFile: piAuthFile, AccountsFile: accountsFile, UsageURL: server.URL, HTTPClient: server.Client(),
		Now: func() time.Time { return time.UnixMilli(1770000000000) }}, &out, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.String() != "30" {
		t.Fatalf("stdout=%q want 30", out.String())
	}
	if _, err := os.Stat(authFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Pi-only must NOT create OC auth, stat err=%v", err)
	}
	assertPiCodex(t, readJSONObject(t, piAuthFile), "pi-a", "pi-r", 1778000000000, "pi-acct")
	if _, ok := mustReadStore(t, accountsFile)["user-cur"]; !ok {
		t.Fatal("Pi account disappeared from store")
	}
}

// ---------------------------------------------------------------------------------------
// Pi-only rotation regression (weekly-exhausted, 5h below threshold).
// ---------------------------------------------------------------------------------------

// TestRunDefaultsRotatePiOnlyWhenWeeklyExhausted pins the Pi-only rotation
// defect: OpenCode auth is absent (VPS with Pi only); active 5h=65 (<80),
// WEEK=100 (>=98); CLI must rotate Pi auth to a complete eligible alternate
// and must not create or touch OpenCode auth. Numeric stdout contract preserved.
func TestRunDefaultsRotatePiOnlyWhenWeeklyExhausted(t *testing.T) {
	t.Parallel()

	resetAt := time.Now().Add(2 * time.Hour).Unix()
	server, _ := newPiServer(t, piResponses{
		"pi-current-access": fmt.Sprintf(
			`{"user_id":"user-current","email":"user-current@example.com","rate_limit":{"primary_window":{"used_percent":65,"reset_at":%d},"secondary_window":{"used_percent":100,"reset_at":%d}}}`,
			resetAt, resetAt),
	})
	defer server.Close()

	// authFile intentionally not seeded: loadOpenCodeAccountIfAvailable reports
	// unavailable and the dispatcher routes to runDefaultCommandPiOnlyPath.
	authFile, piAuthFile, accountsFile := piSyncFixture(t)
	seedAuthFile(t, piAuthFile, piAuthBody("pi-current-access", "pi-current-refresh", 1778000000000, "pi-current-acct"))

	// Alternate carries the complete OAuth tuple so activateAccount validates
	// it via buildPiCredential before touching either auth file.
	expires := int64(1778000000000)
	writeStore(t, accountsFile,
		map[string]any{"user_id": "user-current", "accountId": "pi-current-acct", "access": "pi-current-access", "refresh": "pi-current-refresh", "expires": expires, "type": "oauth"},
		map[string]any{"user_id": "user-alt", "accountId": "pi-alt-acct", "access": "pi-alt-access", "refresh": "pi-alt-refresh", "expires": expires, "type": "oauth"},
	)

	// ConfigFile omitted so runtime defaults (5h on, 5h_threshold=80,
	// weekly_threshold=98) apply.
	var out strings.Builder
	if err := run(context.Background(), config{
		AuthFile:     authFile,
		PiAuthFile:   piAuthFile,
		AccountsFile: accountsFile,
		UsageURL:     server.URL,
		HTTPClient:   server.Client(),
	}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if out.String() != "65" {
		t.Fatalf("stdout=%q want 65", out.String())
	}
	assertPiCodex(t, readJSONObject(t, piAuthFile), "pi-alt-access", "pi-alt-refresh", expires, "pi-alt-acct")
	if _, err := os.Stat(authFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Pi-only must NOT create OpenCode auth, stat err=%v", err)
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

// TestRunDefaultsRotateAbove5hThreshold proves a value above the default
// 5-hour threshold rotates to an eligible alternate.
func TestRunDefaultsRotateAbove5hThreshold(t *testing.T) {
	t.Parallel()

	resetAt := time.Now().Add(2 * time.Hour).Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 85% primary (above default 80), 10% secondary (well below 98).
		_, _ = w.Write([]byte(fmt.Sprintf(
			`{"user_id":"user-current","rate_limit":{"primary_window":{"used_percent":85,"reset_at":%d},"secondary_window":{"used_percent":10,"reset_at":%d}}}`,
			resetAt, resetAt,
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

	var out strings.Builder
	// Intentionally omit ConfigFile so the default 5h_threshold (80) applies.
	err := run(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		UsageURL:     server.URL,
		HTTPClient:   server.Client(),
	}, &out)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Preserve the numeric stdout contract.
	if out.String() != "85" {
		t.Fatalf("stdout=%q want 85", out.String())
	}
	// Rotation flipped the active credential to the eligible alternate.
	data, _ := os.ReadFile(authFile)
	if !strings.Contains(string(data), "alt-token") {
		t.Fatalf("expected rotation to alt-token at default 80 threshold, got auth=%s", string(data))
	}
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

// ---------------------------------------------------------------------------------------
// OAuth refresh: reactive 401-driven flow (fetchUsageWithReactiveRefresh +
// refreshOAuthAccount) plus command-level wiring.
// ---------------------------------------------------------------------------------------

// makeTestJWT encodes an unsigned JWT whose payload mirrors the nested
// account-identity object Pi emits on its rotated access tokens (the
// `https://api.openai.com/auth` claim carries a JSON object whose
// `chatgpt_account_id` field stores the id) so refreshOAuthAccount can
// derive accountId from the rotated access token. The signature segment is
// opaque.
func makeTestJWT(accountID string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payloadBytes, _ := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
		},
	})
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	return header + "." + payload + ".sig"
}

// makeFlatTestJWT is the legacy flat-claim variant (used only by the
// extractChatGPTAccountIDFromJWT regression tests; production Pi traffic has
// switched to the nested form).
func makeFlatTestJWT(accountID string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payloadBytes, _ := json.Marshal(map[string]any{
		"https://api.openai.com/auth.chatgpt_account_id": accountID,
	})
	payload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	return header + "." + payload + ".sig"
}

// TestExtractChatGPTAccountIDFromJWTNestedClaim is the regression for the
// real Pi JWT shape: the chatgpt_account_id lives inside an object under
// the `https://api.openai.com/auth` namespace, not as a flat top-level claim.
func TestExtractChatGPTAccountIDFromJWTNestedClaim(t *testing.T) {
	t.Parallel()

	got, ok := extractChatGPTAccountIDFromJWT(makeTestJWT("nested-acct-id"))
	if !ok {
		t.Fatalf("extractChatGPTAccountIDFromJWT did not extract the nested chatgpt_account_id")
	}
	if got != "nested-acct-id" {
		t.Fatalf("extracted id: got %q, want %q", got, "nested-acct-id")
	}
}

// TestExtractChatGPTAccountIDFromJWTLegacyFlatFallback keeps the old flat
// claim working so issuers that have not switched yet still drive the
// accountId derivation.
func TestExtractChatGPTAccountIDFromJWTLegacyFlatFallback(t *testing.T) {
	t.Parallel()

	got, ok := extractChatGPTAccountIDFromJWT(makeFlatTestJWT("legacy-acct-id"))
	if !ok {
		t.Fatalf("extractChatGPTAccountIDFromJWT did not extract the legacy flat claim")
	}
	if got != "legacy-acct-id" {
		t.Fatalf("extracted id: got %q, want %q", got, "legacy-acct-id")
	}
}

// TestExtractChatGPTAccountIDFromJWTMissingReturnsEmpty preserves the prior
// contract that a token lacking any usable claim leaves the existing
// accountId untouched (refreshOAuthAccount relies on this).
func TestExtractChatGPTAccountIDFromJWTMissingReturnsEmpty(t *testing.T) {
	t.Parallel()

	token := makeTestJWT("")
	if _, ok := extractChatGPTAccountIDFromJWT(token); ok {
		t.Fatalf("empty nested chatgpt_account_id must not be reported as present")
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	empty := base64.RawURLEncoding.EncodeToString([]byte(`{}`))
	if _, ok := extractChatGPTAccountIDFromJWT(header + "." + empty + ".sig"); ok {
		t.Fatalf("missing claim must not be reported as present")
	}
	if _, ok := extractChatGPTAccountIDFromJWT("not-a-jwt"); ok {
		t.Fatalf("malformed token must not be reported as present")
	}
}

// newRefreshServer wraps an http.HandlerFunc, increments a per-call counter,
// and registers t.Cleanup so callers never leak the listener.
func newRefreshServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(s.Close)
	return s, &hits
}

// TestRefreshOAuthAccountRotatesAndPreservesInput is the positive path of
// the reactive helper: when called by a 401-driven flow, the helper POSTs
// to the configured token URL with the required form fields, validates the
// response shape, rotates the OAuth tuple, derives accountId from the new
// JWT claim when valid, and computes the new expires via cfg.Now. The
// helper never inspects the stored `expires`; time-based policies live
// exclusively at the call sites.
//
// The input map is never mutated: it remains the same object that the
// caller passed in, with its pre-refresh values intact.
func TestRefreshOAuthAccountRotatesAndPreservesInput(t *testing.T) {
	t.Parallel()

	fixedNow := time.Unix(1_700_000_000, 0)
	var capturedForm url.Values
	newAccess := makeTestJWT("rotated-acct-id")
	server, hits := newRefreshServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		capturedForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"rotated-refresh-token","expires_in":3600,"token_type":"Bearer"}`, newAccess)
	})

	// Use a stale-looking expires (within the obsolete 5-minute window) so
	// the test also proves the helper rotates even when a proactive policy
	// would have called it. This is the documented reactive contract.
	expiresMs := fixedNow.Add(30 * time.Second).UnixMilli()
	account := map[string]any{
		"access":    "stale-access-token",
		"refresh":   "stale-refresh-token",
		"expires":   int64(expiresMs),
		"accountId": "stale-acct-id",
		"email":     "preserved@example.com",
	}

	got, err := refreshOAuthAccount(context.Background(), config{
		TokenURL:   server.URL,
		HTTPClient: server.Client(),
		Now:        func() time.Time { return fixedNow },
	}, account)
	if err != nil {
		t.Fatalf("refreshOAuthAccount: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected 1 POST, got %d", hits.Load())
	}

	if v := capturedForm.Get("grant_type"); v != "refresh_token" {
		t.Fatalf("grant_type: got %q", v)
	}
	if v := capturedForm.Get("refresh_token"); v != "stale-refresh-token" {
		t.Fatalf("refresh_token: got %q", v)
	}
	if v := capturedForm.Get("client_id"); v != "app_EMoamEEZ73f0CkXaXp7hrann" {
		t.Fatalf("client_id: got %q", v)
	}

	if got["access"] != newAccess {
		t.Fatalf("access: got %q, want %q", got["access"], newAccess)
	}
	if got["refresh"] != "rotated-refresh-token" {
		t.Fatalf("refresh: got %v", got["refresh"])
	}
	wantExpires := fixedNow.Add(time.Hour).UnixMilli()
	if v, ok := valueToInt64(got["expires"]); !ok || v != wantExpires {
		t.Fatalf("expires: got %v, want %d", got["expires"], wantExpires)
	}
	if got["accountId"] != "rotated-acct-id" {
		t.Fatalf("accountId not derived from JWT claim: got %v", got["accountId"])
	}
	if got["email"] != "preserved@example.com" {
		t.Fatalf("unrelated metadata lost: got %v", got["email"])
	}
	if account["access"] != "stale-access-token" || account["refresh"] != "stale-refresh-token" {
		t.Fatalf("input map was mutated: %#v", account)
	}
}

// TestRefreshOAuthAccountIgnoresExpiresWindow pins the reactive contract:
// the helper rotates regardless of the stored `expires`. A credential with
// an hour of remaining lifetime still rotates when a 401-driven caller
// invokes it; a missing `expires` field does not block rotation either.
// This is the regression against any future reintroduction of proactive
// due-window logic.
func TestRefreshOAuthAccountIgnoresExpiresWindow(t *testing.T) {
	t.Parallel()

	fixedNow := time.Unix(1_700_000_000, 0)
	server, hits := newRefreshServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"rotated-fresh","refresh_token":"rotated-fresh-r","expires_in":3600}`)
	})

	t.Run("fresh expires still rotates", func(t *testing.T) {
		t.Parallel()
		account := map[string]any{
			"access":  "fresh-access",
			"refresh": "fresh-refresh",
			"expires": int64(fixedNow.Add(time.Hour).UnixMilli()),
		}
		got, err := refreshOAuthAccount(context.Background(), config{
			TokenURL:   server.URL,
			HTTPClient: server.Client(),
			Now:        func() time.Time { return fixedNow },
		}, account)
		if err != nil {
			t.Fatalf("refreshOAuthAccount: %v", err)
		}
		if got["access"] != "rotated-fresh" {
			t.Fatalf("expected rotation, got access=%v", got["access"])
		}
		if account["access"] != "fresh-access" {
			t.Fatalf("input mutated: %v", account)
		}
	})

	t.Run("missing expires still rotates", func(t *testing.T) {
		t.Parallel()
		account := map[string]any{
			"access":  "legacy-access",
			"refresh": "legacy-refresh",
		}
		got, err := refreshOAuthAccount(context.Background(), config{
			TokenURL:   server.URL,
			HTTPClient: server.Client(),
			Now:        func() time.Time { return fixedNow },
		}, account)
		if err != nil {
			t.Fatalf("refreshOAuthAccount: %v", err)
		}
		if got["access"] != "rotated-fresh" {
			t.Fatalf("expected rotation, got access=%v", got["access"])
		}
		if hits.Load() < 1 {
			t.Fatalf("expected refresh POSTs to have been counted, got %d", hits.Load())
		}
	})
}

// TestRefreshOAuthAccountMissingRefreshReturnsClearError pins the failure
// contract: when the helper is invoked (by a 401-driven caller) but the
// account has no usable refresh metadata, it returns a value-free error
// without contacting the token endpoint. The input map stays unchanged so
// the caller's auth/store bytes are never mutated.
func TestRefreshOAuthAccountMissingRefreshReturnsClearError(t *testing.T) {
	t.Parallel()

	server, hits := newRefreshServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("refresh endpoint must not be called when refresh metadata is missing")
		w.WriteHeader(http.StatusOK)
	})

	account := map[string]any{
		"access": "no-refresh-access",
		// no "refresh" field at all
	}
	got, err := refreshOAuthAccount(context.Background(), config{
		TokenURL:   server.URL,
		HTTPClient: server.Client(),
	}, account)
	if err == nil {
		t.Fatalf("expected error on missing refresh metadata, got nil; got=%#v", got)
	}
	if got != nil {
		t.Fatalf("expected nil map on failure, got %#v", got)
	}
	if hits.Load() != 0 {
		t.Fatalf("expected 0 POSTs, got %d", hits.Load())
	}
	msg := err.Error()
	for _, secret := range []string{"no-refresh-access"} {
		if strings.Contains(msg, secret) {
			t.Fatalf("error leaked %q: %v", secret, err)
		}
	}
	if account["access"] != "no-refresh-access" {
		t.Fatalf("input mutated on failure: %v", account)
	}
}

// TestRefreshOAuthAccountFailurePreservesInputAndNoLeak is the failure
// path: a non-200 response with echoed credential material must return a
// value-free error and leave the input map byte-identical. The bearer
// tokens never appear in the error so they never reach user-visible output.
func TestRefreshOAuthAccountFailurePreservesInputAndNoLeak(t *testing.T) {
	t.Parallel()

	server, _ := newRefreshServer(t, func(w http.ResponseWriter, _ *http.Request) {
		// Echo back what the client sent so we can prove the helper doesn't
		// include any of it in the returned error.
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `error="invalid_request" detail="refresh_token leaked: SECRET-REFRESH-XYZ access=SECRET-ACCESS-ABC"`)
	})

	account := map[string]any{
		"access":  "SECRET-ACCESS-ABC",
		"refresh": "SECRET-REFRESH-XYZ",
		"expires": int64(time.Unix(1_700_000_000, 0).Add(30 * time.Second).UnixMilli()),
	}

	got, err := refreshOAuthAccount(context.Background(), config{
		TokenURL:   server.URL,
		HTTPClient: server.Client(),
	}, account)
	if err == nil {
		t.Fatalf("expected error on non-200 refresh response")
	}
	if got != nil {
		t.Fatalf("expected nil map on failure, got %#v", got)
	}
	msg := err.Error()
	for _, secret := range []string{"SECRET-ACCESS-ABC", "SECRET-REFRESH-XYZ", "new-access"} {
		if strings.Contains(msg, secret) {
			t.Fatalf("error leaked %q: %v", secret, err)
		}
	}
	if account["access"] != "SECRET-ACCESS-ABC" || account["refresh"] != "SECRET-REFRESH-XYZ" {
		t.Fatalf("input mutated on failure: %#v", account)
	}
}

// TestRunAccountsCommandRefreshesOnUnauthorizedAndRetainsCurrentMarker
// exercises the reactive accounts wiring: the usage endpoint returns 401
// for the CURRENT row and 200 for the second row; only the 401 row
// triggers a single refresh POST and a single retry that succeeds. The
// CURRENT marker is preserved (computed against the pre-refresh access),
// the rotated credential is mirrored to the installed auth stores, and the
// non-current row is left untouched.
func TestRunAccountsCommandRefreshesOnUnauthorizedAndRetainsCurrentMarker(t *testing.T) {
	t.Parallel()

	fixedNow := time.Unix(1_700_000_000, 0)
	farExpiresMs := fixedNow.Add(2 * time.Hour).UnixMilli()
	rotatedExpiresMs := fixedNow.Add(time.Hour).UnixMilli()

	refreshServer, refreshHits := newRefreshServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if r.PostForm.Get("refresh_token") != "current-refresh" {
			t.Errorf("unexpected refresh_token: %q", r.PostForm.Get("refresh_token"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"rotated-current-access","refresh_token":"rotated-current-refresh","expires_in":3600}`)
	})

	// Usage server: first call with the CURRENT row's pre-refresh access
	// returns 401; second call (after refresh) with the rotated access
	// returns 200; the second row's single call returns 200 immediately.
	usageHits := atomic.Int32{}
	usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		usageHits.Add(1)
		auth := r.Header.Get("Authorization")
		switch {
		case strings.Contains(auth, "rotated-current-access"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_id":"user-current","email":"current@example.com","rate_limit":{"primary_window":{"used_percent":50,"reset_at":1777414800}}}`))
		case strings.Contains(auth, "current-access"):
			w.WriteHeader(http.StatusUnauthorized)
		case strings.Contains(auth, "other-access"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_id":"user-other","email":"other@example.com","rate_limit":{"primary_window":{"used_percent":60,"reset_at":1777414800}}}`))
		default:
			t.Errorf("unexpected Authorization header: %q", auth)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(usageServer.Close)

	authFile, _, accountsFile := piSyncFixture(t)
	seedAuthFile(t, authFile, ocAuthBody("current-access", "current-refresh", farExpiresMs, "current-acct"))

	writeStore(t, accountsFile,
		map[string]any{"user_id": "user-current", "access": "current-access", "refresh": "current-refresh",
			"expires": int64(farExpiresMs), "type": "oauth", "accountId": "current-acct"},
		map[string]any{"user_id": "user-other", "access": "other-access", "refresh": "other-refresh",
			"expires": int64(farExpiresMs), "type": "oauth", "accountId": "other-acct"},
	)

	var out strings.Builder
	err := runAccountsCommand(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		UsageURL:     usageServer.URL,
		TokenURL:     refreshServer.URL,
		HTTPClient:   usageServer.Client(),
		Now:          func() time.Time { return fixedNow },
	}, &out)
	if err != nil {
		t.Fatalf("runAccountsCommand: %v", err)
	}
	if v := refreshHits.Load(); v != 1 {
		t.Fatalf("expected exactly 1 refresh POST, got %d", v)
	}
	// Usage: 1 (initial 401) + 1 (retry) for the current row + 1 for the
	// other row = 3.
	if v := usageHits.Load(); v != 3 {
		t.Fatalf("expected exactly 3 usage calls (one initial 401, one retry, one for the other row), got %d", v)
	}
	if !strings.Contains(out.String(), "*") {
		t.Fatalf("expected CURRENT marker to be retained, got %q", out.String())
	}
	assertOCAuth(t, authFile, "rotated-current-access", "rotated-current-refresh", rotatedExpiresMs, "current-acct")

	store := mustReadStore(t, accountsFile)
	if v, _ := store["user-current"]["access"].(string); v != "rotated-current-access" {
		t.Fatalf("store user-current access not rotated: %v", store["user-current"]["access"])
	}
	if v, _ := store["user-other"]["access"].(string); v != "other-access" {
		t.Fatalf("user-other access mutated: %v", store["user-other"]["access"])
	}
}

// TestRunAccountsCommandSecondUnauthorizedStopsAfterOneRefresh pins the
// retry-once contract: after a successful OAuth refresh, a second 401
// from the usage endpoint aborts without a second refresh POST. The
// current row is rendered as ERR and the table still includes the other
// rows (row-local isolation).
func TestRunAccountsCommandSecondUnauthorizedStopsAfterOneRefresh(t *testing.T) {
	t.Parallel()

	fixedNow := time.Unix(1_700_000_000, 0)
	farExpiresMs := fixedNow.Add(2 * time.Hour).UnixMilli()
	rotatedMs := fixedNow.Add(time.Hour).UnixMilli()

	refreshServer, refreshHits := newRefreshServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"rotated-still-bad-access","refresh_token":"rotated-still-bad-refresh","expires_in":3600}`)
	})

	var usageHits atomic.Int32
	usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Every call returns 401 (initial and post-refresh retry). The
		// reactive helper must stop after one retry; the second 401 from the
		// usage endpoint is surfaced as the row error.
		usageHits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(usageServer.Close)

	authFile, _, accountsFile := piSyncFixture(t)
	seedAuthFile(t, authFile, ocAuthBody("current-access", "current-refresh", farExpiresMs, "current-acct"))

	writeStore(t, accountsFile,
		map[string]any{"user_id": "user-current", "access": "current-access", "refresh": "current-refresh",
			"expires": int64(farExpiresMs), "type": "oauth", "accountId": "current-acct"},
	)

	var out strings.Builder
	err := runAccountsCommand(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		UsageURL:     usageServer.URL,
		TokenURL:     refreshServer.URL,
		HTTPClient:   usageServer.Client(),
		Now:          func() time.Time { return fixedNow },
	}, &out)
	if err == nil {
		t.Fatalf("expected error from second 401, got nil; stdout=%q", out.String())
	}
	if !strings.Contains(err.Error(), "failed to fetch usage for all saved accounts") {
		t.Fatalf("expected overall-failure error, got %v", err)
	}
	if v := refreshHits.Load(); v != 1 {
		t.Fatalf("expected exactly 1 refresh POST (no recursive refresh), got %d", v)
	}
	if v := usageHits.Load(); v != 2 {
		t.Fatalf("expected exactly 2 usage calls (initial 401 + one retry), got %d", v)
	}
	if !strings.Contains(out.String(), "ERR") {
		t.Fatalf("row must render as ERR, got %q", out.String())
	}
	// The sync ran BEFORE the retry (documented sequence: refresh →
	// persist → retry). On a second 401 the auth file already carries the
	// rotated (still bad) tuple. The next run treats it as just another
	// 401 and refreshes again — that is across-run behavior, not recursion
	// within a single run.
	assertOCAuth(t, authFile, "rotated-still-bad-access", "rotated-still-bad-refresh", rotatedMs, "current-acct")
}

// TestRunAccountsCommandNon401DoesNotInvokeRefresh pins the contract that
// non-401 responses (400, 403, 429, 500) bypass the token endpoint. The
// reactive helper must return the existing error verbatim and never POST
// to the refresh endpoint.
func TestRunAccountsCommandNon401DoesNotInvokeRefresh(t *testing.T) {
	t.Parallel()

	fixedNow := time.Unix(1_700_000_000, 0)
	farExpiresMs := fixedNow.Add(2 * time.Hour).UnixMilli()

	refreshServer, refreshHits := newRefreshServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("refresh endpoint must not be called for non-401 responses")
		w.WriteHeader(http.StatusOK)
	})

	cases := []struct {
		name       string
		statusCode int
		body       string
	}{
		{"400", http.StatusBadRequest, "bad request"},
		{"403", http.StatusForbidden, "forbidden"},
		{"429", http.StatusTooManyRequests, "rate limited"},
		{"500", http.StatusInternalServerError, "server error"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.statusCode)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(usageServer.Close)

			authFile, _, accountsFile := piSyncFixture(t)
			seedAuthFile(t, authFile, ocAuthBody("current-access", "current-refresh", farExpiresMs, "current-acct"))
			writeStore(t, accountsFile,
				map[string]any{"user_id": "user-current", "access": "current-access", "refresh": "current-refresh",
					"expires": int64(farExpiresMs), "type": "oauth", "accountId": "current-acct"},
			)

			var out strings.Builder
			err := runAccountsCommand(context.Background(), config{
				AuthFile:     authFile,
				AccountsFile: accountsFile,
				UsageURL:     usageServer.URL,
				TokenURL:     refreshServer.URL,
				HTTPClient:   usageServer.Client(),
				Now:          func() time.Time { return fixedNow },
			}, &out)
			if err == nil {
				t.Fatalf("expected error from %d response, got nil", tc.statusCode)
			}
			if !strings.Contains(err.Error(), "failed to fetch usage for all saved accounts") {
				t.Fatalf("expected overall-failure error, got %v", err)
			}
			if v := refreshHits.Load(); v != 0 {
				t.Fatalf("expected 0 refresh POSTs for %d response, got %d", tc.statusCode, v)
			}
		})
	}
}

// TestRunAccountsCommandMissingRefreshMetadataIsRowLocal pins the row-local
// behavior: a row whose 401-driven refresh fails because the stored
// refresh token is missing renders as ERR, but other rows are still
// rendered. The token endpoint is contacted for that row's refresh attempt
// (via the reactive helper) but the missing-refresh error short-circuits
// before any POST.
func TestRunAccountsCommandMissingRefreshMetadataIsRowLocal(t *testing.T) {
	t.Parallel()

	fixedNow := time.Unix(1_700_000_000, 0)
	farExpiresMs := fixedNow.Add(2 * time.Hour).UnixMilli()

	refreshServer, refreshHits := newRefreshServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("refresh endpoint must not be called when stored refresh token is empty")
		w.WriteHeader(http.StatusOK)
	})

	usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if strings.Contains(auth, "current-access") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"user-other","email":"other@example.com","rate_limit":{"primary_window":{"used_percent":60,"reset_at":1777414800}}}`))
	}))
	t.Cleanup(usageServer.Close)

	authFile, _, accountsFile := piSyncFixture(t)
	seedAuthFile(t, authFile, ocAuthBody("current-access", "current-refresh", farExpiresMs, "current-acct"))

	// First row has no `refresh` field: a 401-driven refresh attempt must
	// surface a value-free error and never contact the token endpoint.
	writeStore(t, accountsFile,
		map[string]any{"user_id": "user-current", "access": "current-access",
			"expires": int64(farExpiresMs), "type": "oauth", "accountId": "current-acct"},
		map[string]any{"user_id": "user-other", "access": "other-access", "refresh": "other-refresh",
			"expires": int64(farExpiresMs), "type": "oauth", "accountId": "other-acct"},
	)

	var out strings.Builder
	err := runAccountsCommand(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		UsageURL:     usageServer.URL,
		TokenURL:     refreshServer.URL,
		HTTPClient:   usageServer.Client(),
		Now:          func() time.Time { return fixedNow },
	}, &out)
	if err != nil {
		t.Fatalf("runAccountsCommand: %v", err)
	}
	if v := refreshHits.Load(); v != 0 {
		t.Fatalf("expected 0 refresh POSTs (missing refresh metadata), got %d", v)
	}
	if !strings.Contains(out.String(), "ERR") {
		t.Fatalf("expected ERR row, got %q", out.String())
	}
	if !strings.Contains(out.String(), "other@example.com") {
		t.Fatalf("expected the other row to render despite the first row's failure, got %q", out.String())
	}
	for _, secret := range []string{"current-access", "current-refresh", "current-acct"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("row error leaked secret %q: %q", secret, out.String())
		}
	}
}

// TestRunAccountsCommandRefreshFailureLeavesFilesUnchanged pins the
// contract: a 401-driven refresh POST that fails (e.g. the token endpoint
// returns 400) leaves auth/store bytes byte-identical to the pre-call
// state. Errors never echo credential values, JWT claims, or response
// bodies.
func TestRunAccountsCommandRefreshFailureLeavesFilesUnchanged(t *testing.T) {
	t.Parallel()

	fixedNow := time.Unix(1_700_000_000, 0)
	farExpiresMs := fixedNow.Add(2 * time.Hour).UnixMilli()

	refreshServer, refreshHits := newRefreshServer(t, func(w http.ResponseWriter, _ *http.Request) {
		// Echo back the caller's credential material so the test can prove
		// the surfaced row error never includes it.
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `error="invalid_grant" refresh_token=SECRET-REFRESH-XYZ access=SECRET-ACCESS-ABC`)
	})

	usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "current-access") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(usageServer.Close)

	authFile, _, accountsFile := piSyncFixture(t)
	seedAuthFile(t, authFile, ocAuthBody("current-access", "current-refresh", farExpiresMs, "current-acct"))
	writeStore(t, accountsFile,
		map[string]any{"user_id": "user-current", "access": "current-access", "refresh": "current-refresh",
			"expires": int64(farExpiresMs), "type": "oauth", "accountId": "current-acct"},
	)

	var out strings.Builder
	err := runAccountsCommand(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		UsageURL:     usageServer.URL,
		TokenURL:     refreshServer.URL,
		HTTPClient:   usageServer.Client(),
		Now:          func() time.Time { return fixedNow },
	}, &out)
	if err == nil {
		t.Fatalf("expected overall error from refresh failure, got nil; stdout=%q", out.String())
	}
	if v := refreshHits.Load(); v != 1 {
		t.Fatalf("expected exactly 1 refresh POST, got %d", v)
	}
	if !strings.Contains(out.String(), "ERR") {
		t.Fatalf("row must render as ERR, got %q", out.String())
	}
	// Auth/store bytes must remain byte-identical because the refresh POST
	// failed: persistFn was never invoked, so no sync touched any file.
	assertOCAuth(t, authFile, "current-access", "current-refresh", farExpiresMs, "current-acct")
	store := mustReadStore(t, accountsFile)
	if v, _ := store["user-current"]["access"].(string); v != "current-access" {
		t.Fatalf("store must NOT carry rotated access on refresh failure: got %v", v)
	}
	// No secret may leak through the row error.
	rendered := out.String()
	for _, secret := range []string{"SECRET-ACCESS-ABC", "SECRET-REFRESH-XYZ", "current-refresh"} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("row error leaked secret %q: %q", secret, rendered)
		}
	}
}

// TestRunUseCommandDoesNotInvokeRefresh pins the reactive contract: `use`
// does NOT query the usage endpoint, so OAuth refresh is out of scope.
// The token endpoint is never contacted even when the stored `expires`
// would have qualified under the obsolete 5-minute proactive window. The
// active auth file simply receives the stored OAuth tuple from the
// selected account via activateAccount.
func TestRunUseCommandDoesNotInvokeRefresh(t *testing.T) {
	t.Parallel()

	fixedNow := time.Unix(1_700_000_000, 0)
	dueExpiresMs := fixedNow.Add(30 * time.Second).UnixMilli()
	farExpiresMs := fixedNow.Add(2 * time.Hour).UnixMilli()

	refreshServer, refreshHits := newRefreshServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("refresh endpoint must not be called from `use`")
		w.WriteHeader(http.StatusOK)
	})

	authFile, _, accountsFile := piSyncFixture(t)
	seedAuthFile(t, authFile, ocAuthBody("current-access", "current-refresh", farExpiresMs, "current-acct"))

	writeStore(t, accountsFile,
		map[string]any{"user_id": "user-current", "access": "current-access", "refresh": "current-refresh",
			"expires": int64(farExpiresMs), "type": "oauth", "accountId": "current-acct"},
		map[string]any{"user_id": "user-target", "access": "target-access", "refresh": "target-refresh",
			"expires": int64(dueExpiresMs), "type": "oauth", "accountId": "target-acct"},
	)

	var out strings.Builder
	if err := runUseCommand(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		TokenURL:     refreshServer.URL,
		Now:          func() time.Time { return fixedNow },
	}, &out, "user-target"); err != nil {
		t.Fatalf("runUseCommand: %v", err)
	}
	if v := refreshHits.Load(); v != 0 {
		t.Fatalf("expected 0 refresh POSTs from `use`, got %d", v)
	}
	p := readJSONObject(t, authFile)
	if p["openai.access"] != "target-access" {
		t.Fatalf("active auth access must carry the stored target access (not rotated), got %v", p["openai.access"])
	}
	if p["openai.refresh"] != "target-refresh" {
		t.Fatalf("active auth refresh must carry the stored target refresh, got %v", p["openai.refresh"])
	}
	if v, ok := valueToInt64(p["openai.expires"]); !ok || v != dueExpiresMs {
		t.Fatalf("active auth expires must carry the stored target expires, got %v", p["openai.expires"])
	}
	store := mustReadStore(t, accountsFile)
	if v, _ := store["user-target"]["access"].(string); v != "target-access" {
		t.Fatalf("store target access must remain on the pre-rotation token, got %v", store["user-target"]["access"])
	}
	// Sanity: unrelated rows are untouched.
	if v, _ := store["user-current"]["access"].(string); v != "current-access" {
		t.Fatalf("current row mutated: %v", store["user-current"]["access"])
	}
}

// TestRunUseCommandExpiredMetadataDoesNotInvokeRefresh pins the contract
// that `use` ignores the stored `expires` field. Even when the target
// account has a near-expiry metadata that would have triggered the
// obsolete proactive refresh, the token endpoint is never contacted.
func TestRunUseCommandExpiredMetadataDoesNotInvokeRefresh(t *testing.T) {
	t.Parallel()

	fixedNow := time.Unix(1_700_000_000, 0)
	expiredMs := fixedNow.Add(-1 * time.Hour).UnixMilli()

	refreshServer, refreshHits := newRefreshServer(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("refresh endpoint must not be called from `use` even with expired metadata")
		w.WriteHeader(http.StatusOK)
	})

	authFile, _, accountsFile := piSyncFixture(t)
	seedAuthFile(t, authFile, ocAuthBody("current-access", "current-refresh",
		fixedNow.Add(2*time.Hour).UnixMilli(), "current-acct"))

	writeStore(t, accountsFile,
		map[string]any{"user_id": "user-current", "access": "current-access", "refresh": "current-refresh",
			"expires": int64(fixedNow.Add(2 * time.Hour).UnixMilli()), "type": "oauth", "accountId": "current-acct"},
		map[string]any{"user_id": "user-target", "access": "target-access", "refresh": "target-refresh",
			"expires": int64(expiredMs), "type": "oauth", "accountId": "target-acct"},
	)

	var out strings.Builder
	if err := runUseCommand(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		TokenURL:     refreshServer.URL,
		Now:          func() time.Time { return fixedNow },
	}, &out, "user-target"); err != nil {
		t.Fatalf("runUseCommand: %v", err)
	}
	if v := refreshHits.Load(); v != 0 {
		t.Fatalf("expected 0 refresh POSTs from `use` with expired metadata, got %d", v)
	}
	p := readJSONObject(t, authFile)
	if p["openai.access"] != "target-access" {
		t.Fatalf("active auth access must carry the stored (expired-metadata) target access, got %v", p["openai.access"])
	}
}

// TestRunAccountsCommandCurrentSyncFailureReturnsClearError pins the
// reactive coherence contract: a 401-driven refresh of the CURRENT row
// whose sync to an installed auth store fails must (a) keep the row
// visible with its per-row error, (b) NOT be counted as a successful
// persisted refresh (successCount), and (c) cause the command to return a
// clear error so the caller does not silently see overall success. Auth/
// store bytes are unchanged because syncRefreshedCredentialToBoth's
// preflight / rollback path keeps the post-mutation failure from leaking.
func TestRunAccountsCommandCurrentSyncFailureReturnsClearError(t *testing.T) {
	t.Parallel()

	fixedNow := time.Unix(1_700_000_000, 0)
	farExpiresMs := fixedNow.Add(2 * time.Hour).UnixMilli()

	refreshServer, _ := newRefreshServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"rotated-current-access","refresh_token":"rotated-current-refresh","expires_in":3600}`)
	})
	usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "current-access") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(usageServer.Close)

	dir := t.TempDir()
	authFile := filepath.Join(dir, "opencode.json")
	accountsFile := filepath.Join(dir, "accounts.json")
	if err := os.WriteFile(authFile, []byte(`{"openai.access":"current-access"}`), 0o600); err != nil {
		t.Fatalf("seed auth: %v", err)
	}
	// Make the OC auth file un-writable so syncAccountToOpenCodeAuth errors.
	if err := os.Chmod(authFile, 0o400); err != nil {
		t.Fatalf("chmod auth: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(authFile, 0o600) })

	writeStore(t, accountsFile,
		map[string]any{"user_id": "user-current", "access": "current-access", "refresh": "current-refresh",
			"expires": int64(farExpiresMs), "type": "oauth", "accountId": "current-acct"},
	)

	var out strings.Builder
	err := runAccountsCommand(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		UsageURL:     usageServer.URL,
		TokenURL:     refreshServer.URL,
		HTTPClient:   usageServer.Client(),
		Now:          func() time.Time { return fixedNow },
	}, &out)
	if err == nil {
		t.Fatalf("expected current-sync-failure error, got nil; stdout=%q", out.String())
	}
	if !strings.Contains(err.Error(), "current account auth synchronization failed") {
		t.Fatalf("expected current-account-coherence error, got %v", err)
	}
	if !strings.Contains(out.String(), "ERR") {
		t.Fatalf("row-local error must still render in the table, got %q", out.String())
	}
	// The coherence failure must not inflate successCount into "fetched at
	// least one row"; persistence was skipped for the failed row, so the
	// store must still carry the pre-refresh tuple.
	store := mustReadStore(t, accountsFile)
	if v, _ := store["user-current"]["access"].(string); v != "current-access" {
		t.Fatalf("store must NOT claim a successful persisted refresh on failed sync: got %v", v)
	}
	// Auth file: must remain on the pre-rotation token. The sync failed
	// (write to a chmod-0o400 file), so the OC file is untouched byte-for-byte.
	if got := string(mustReadFile(t, authFile)); got != `{"openai.access":"current-access"}` {
		t.Fatalf("auth file must remain byte-identical to its pre-call state, got %q", got)
	}
}

// TestRunDefaultCommandReactiveRefreshCoherence is the consolidated
// reactive-flow regression. The helper issues a refresh POST ONLY when the
// usage endpoint returns 401 for the current access token; the rotated
// credential is mirrored into the in-memory piInfo snapshot so the later
// reconcilePiInboundFromPayload call observes the rotated access instead of
// the stale pre-refresh token. Rotation is suppressed with thresholds = 100
// so the assertions isolate the refresh path itself.
func TestRunDefaultCommandReactiveRefreshCoherence(t *testing.T) {
	t.Parallel()

	fixedNow := time.Unix(1_700_000_000, 0)
	farMs := fixedNow.Add(2 * time.Hour).UnixMilli()
	rotatedMs := fixedNow.Add(time.Hour).UnixMilli()

	type seed struct {
		name        string
		shared      bool   // OC and Pi carry the same access token
		refreshBody string // empty when no refresh should happen
		usageTokens map[string]string
		// usageStatus: optional map from access token to HTTP status. Absent
		// entries default to 200 (success). Used to simulate 401 + retry.
		usageStatus  map[string]int
		wantHits     int32
		wantOCAccess string
		wantPiAccess string
		wantStdout   string
	}
	cases := []seed{
		{
			// Different access; OC gets 401 once, retry 200; Pi is left
			// untouched (no Pi usage call when OC has the active row). One
			// POST; both stores converge on the rotated access; stdout uses
			// the rotated usage; piInfo mirrors the rotated access.
			name:         "shared_access_converges_after_401",
			shared:       true,
			refreshBody:  `{"access_token":"rotated-shared-access","refresh_token":"rotated-shared-refresh","expires_in":3600}`,
			usageStatus:  map[string]int{"shared-access": http.StatusUnauthorized},
			usageTokens:  map[string]string{"rotated-shared-access": "33"},
			wantHits:     1,
			wantOCAccess: "rotated-shared-access",
			wantPiAccess: "rotated-shared-access",
			wantStdout:   "33",
		},
		{
			// Different access; OC and Pi carry different access tokens; OC
			// gets 401 once. The reactive helper refreshes OC, syncs both
			// stores (since OC is the active row), and retries with the
			// rotated access. The Pi auth file is also synced because OC is
			// the active row.
			name:         "different_access_oc_401_converges",
			refreshBody:  `{"access_token":"rotated-oc-access","refresh_token":"rotated-oc-refresh","expires_in":3600}`,
			usageStatus:  map[string]int{"oc-access": http.StatusUnauthorized},
			usageTokens:  map[string]string{"rotated-oc-access": "44"},
			wantHits:     1,
			wantOCAccess: "rotated-oc-access",
			wantPiAccess: "rotated-oc-access",
			wantStdout:   "44",
		},
		{
			// No 401 from the usage endpoint: zero refresh POSTs; both
			// stores and stdout remain on the pre-refresh access.
			name:         "no_401_no_refresh",
			shared:       true,
			usageTokens:  map[string]string{"shared-access": "12"},
			wantHits:     0,
			wantOCAccess: "shared-access",
			wantPiAccess: "shared-access",
			wantStdout:   "12",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ocAccess := "oc-access"
			piAccess := "pi-access"
			if tc.shared {
				ocAccess, piAccess = "shared-access", "shared-access"
			}
			ocRefresh, piRefresh := "oc-refresh", "pi-refresh"
			if tc.shared {
				ocRefresh, piRefresh = "shared-refresh", "shared-refresh"
			}

			refreshServer, refreshHits := newRefreshServer(t, func(w http.ResponseWriter, _ *http.Request) {
				if tc.wantHits == 0 {
					t.Errorf("refresh POST must NOT run for case %q", tc.name)
					w.WriteHeader(http.StatusOK)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.refreshBody))
			})

			usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth := r.Header.Get("Authorization")
				if status, ok := tc.usageStatus[extractBearer(auth)]; ok && status == http.StatusUnauthorized {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				used, ok := tc.usageTokens[extractBearer(auth)]
				if !ok {
					t.Errorf("unexpected usage call with auth %q", auth)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(piPayload("user-cur", used, "")))
			}))
			t.Cleanup(usageServer.Close)

			authFile, piAuthFile, accountsFile := piSyncFixture(t)
			seedAuthFile(t, authFile, ocAuthBody(ocAccess, ocRefresh, farMs, "shared-acct"))
			seedAuthFile(t, piAuthFile, piAuthBody(piAccess, piRefresh, farMs, "shared-acct"))
			writeStore(t, accountsFile,
				map[string]any{"user_id": "user-cur", "access": ocAccess, "refresh": ocRefresh,
					"expires": int64(farMs), "type": "oauth", "accountId": "shared-acct"},
			)
			credMap := map[string]any{
				"type":      "oauth",
				"access":    piAccess,
				"refresh":   piRefresh,
				"expires":   int64(farMs),
				"accountId": "shared-acct",
			}
			piInfo := &piAuthInfo{Payload: map[string]any{"openai-codex": credMap}, CredMap: credMap}
			ocAccount := map[string]any{
				"access":    ocAccess,
				"refresh":   ocRefresh,
				"expires":   int64(farMs),
				"type":      "oauth",
				"accountId": "shared-acct",
			}

			var out strings.Builder
			if err := runDefaultCommandOpenCodePath(context.Background(), config{
				AuthFile:          authFile,
				PiAuthFile:        piAuthFile,
				AccountsFile:      accountsFile,
				UsageURL:          usageServer.URL,
				TokenURL:          refreshServer.URL,
				HTTPClient:        usageServer.Client(),
				Now:               func() time.Time { return fixedNow },
				FiveHourThreshold: 100,
				WeeklyThreshold:   100,
			}, &out, ocAccount, piInfo, true); err != nil {
				t.Fatalf("runDefaultCommandOpenCodePath: %v", err)
			}

			if v := refreshHits.Load(); v != tc.wantHits {
				t.Fatalf("refresh POSTs: got %d, want %d", v, tc.wantHits)
			}
			// After a reactive 401-driven refresh, both stores carry the
			// rotated tuple (access + refresh + rotated expires). Without a
			// refresh, both keep their original farMs expiry and their
			// original refresh tokens.
			wantOCExpires := farMs
			wantPiExpires := farMs
			wantOCRefresh := ocRefresh
			wantPiRefresh := piRefresh
			if tc.wantHits > 0 {
				wantOCExpires = rotatedMs
				wantPiExpires = rotatedMs
				wantOCRefresh = "rotated-shared-refresh"
				if !tc.shared {
					wantOCRefresh = "rotated-oc-refresh"
				}
				wantPiRefresh = wantOCRefresh
			}
			assertOCAuth(t, authFile, tc.wantOCAccess, wantOCRefresh, wantOCExpires, "shared-acct")
			if tc.wantPiAccess != "" {
				assertPiCodex(t, readJSONObject(t, piAuthFile), tc.wantPiAccess, wantPiRefresh, wantPiExpires, "shared-acct")
			}
			if out.String() != tc.wantStdout {
				t.Fatalf("stdout=%q, want %q", out.String(), tc.wantStdout)
			}
			// After a reactive rotation the in-memory piInfo must reflect the
			// rotated access so reconcilePiInboundFromPayload does not
			// reintroduce the stale credential.
			if tc.wantHits > 0 {
				if got, _ := piInfo.CredMap["access"].(string); got != tc.wantOCAccess {
					t.Fatalf("piInfo.CredMap access: got %q, want %q", got, tc.wantOCAccess)
				}
			}
		})
	}
}

// extractBearer returns the access token from a "Bearer <token>" header value.
// Empty input yields "".
func extractBearer(authHeader string) string {
	return strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
}

// farFuture returns a UnixMilli timestamp well past any sensible test "due"
// window. Used by tests that need a non-due baseline without baking magic
// numbers into every fixture.
func farFuture(now time.Time) int64 {
	return now.Add(2 * time.Hour).UnixMilli()
}

// TestSyncRefreshedCredentialToBothPiLockHeldExternallyLeavesBothUntouched
// is the regression for the dual-provider refresh path when Pi identity
// cannot be read: when Pi's `${auth.json}.lock` is held by an external
// actor BEFORE the refresh sync runs, the helper cannot verify the
// same-account identity (the identity read also goes through the lock)
// and must abort the refresh sync BEFORE mutating either auth file. The
// helper returns a clear error, both auth files remain byte-identical
// to their pre-call state, and the external lock is never deleted.
//
// A clean different-account refresh (no lock contention, Pi identity read
// succeeds, Pi's accountId/access does not match the rotated tuple) is
// the orthogonal contract and is pinned by
// TestSyncRefreshedCredentialToBothDifferentAccountSkipsPiWithoutTouchingLock:
// that path returns nil, writes only OC, and leaves Pi byte-identical
// to its pre-call state without acquiring the Pi lock. The error
// returned here is for the UNKNOWN-identity path only, where the safe
// default is to leave BOTH stores untouched so an unreadable Pi can
// never leak a rotated credential.
func TestSyncRefreshedCredentialToBothPiLockHeldExternallyLeavesBothUntouched(t *testing.T) {
	t.Parallel()

	d := t.TempDir()
	authFile := filepath.Join(d, "opencode.json")
	piAuthFile := filepath.Join(d, "pi.json")

	// Same accountId on both stores AND on the rotated credential so the
	// helper would normally enter the dual-write branch if Pi identity were
	// readable.
	const sharedAcct = "shared-acct"
	originalOC := fmt.Sprintf(`{"openai.access":"old-oc","openai.refresh":"old-oc-r","openai.expires":1700003600000,"openai.type":"oauth","openai.accountId":%q}`, sharedAcct)
	originalPi := fmt.Sprintf(`{"openai-codex":{"type":"oauth","access":"old-pi","refresh":"old-pi-r","expires":1700003600000,"accountId":%q}}`, sharedAcct)
	seedAuthFile(t, authFile, originalOC)
	seedAuthFile(t, piAuthFile, originalPi)

	// External actor (e.g. another CLI process) holds the Pi lock. Because
	// the identity read goes through the same lock, piAuthMatchesAccount
	// returns an UNKNOWN result; the helper must abort the refresh sync
	// before touching either auth file.
	lockPath := piAuthFile + ".lock"
	if err := os.Mkdir(lockPath, 0o700); err != nil {
		t.Fatalf("seed lock dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(lockPath) })

	refreshed := map[string]any{
		"access":    "rotated-access",
		"refresh":   "rotated-refresh",
		"expires":   int64(1700007200000),
		"type":      "oauth",
		"accountId": sharedAcct,
	}

	err := syncRefreshedCredentialToBoth(config{
		AuthFile:   authFile,
		PiAuthFile: piAuthFile,
	}, refreshed, false)
	if err == nil {
		t.Fatalf("identity read contention must surface as a clear error, not nil")
	}
	if !strings.Contains(err.Error(), "Pi auth identity check before refresh sync") {
		t.Fatalf("expected Pi-identity-check error prefix, got %v", err)
	}
	if !strings.Contains(err.Error(), "Pi auth lock") {
		t.Fatalf("expected error to name the underlying Pi lock contention, got %v", err)
	}

	// OC MUST remain byte-identical to its pre-call state: the helper
	// aborted before any file was touched.
	if got := string(mustReadFile(t, authFile)); got != originalOC {
		t.Fatalf("OC auth file was mutated despite identity-verification failure; got %q", got)
	}
	// Pi MUST remain byte-identical to its pre-call state.
	if got := string(mustReadFile(t, piAuthFile)); got != originalPi {
		t.Fatalf("Pi auth file was mutated despite identity-verification failure; got %q", got)
	}
	// The external lock must still be present and owned by the original
	// holder so the helper never deleted a lock it did not create.
	if _, statErr := os.Stat(lockPath); statErr != nil {
		t.Fatalf("external Pi lock was deleted by helper: %v", statErr)
	}
}

// TestSyncRefreshedCredentialToBothMalformedPiIdentityLeavesBothUntouched
// is the malformed-JSON counterpart to
// TestSyncRefreshedCredentialToBothPiLockHeldExternallyLeavesBothUntouched:
// when Pi's auth.json is present and the Pi directory exists but the
// identity read fails because the JSON cannot be parsed, the helper must
// return a clear error and leave BOTH auth files byte-identical to their
// pre-call state. Same-account dual-write MUST NOT run when the identity
// result is UNKNOWN.
func TestSyncRefreshedCredentialToBothMalformedPiIdentityLeavesBothUntouched(t *testing.T) {
	t.Parallel()

	d := t.TempDir()
	authFile := filepath.Join(d, "opencode.json")
	piAuthFile := filepath.Join(d, "pi.json")

	originalOC := `{"openai.access":"old-oc","openai.refresh":"old-oc-r","openai.expires":1700003600000,"openai.type":"oauth","openai.accountId":"shared-acct"}`
	originalPi := `{not valid json`
	seedAuthFile(t, authFile, originalOC)
	seedAuthFile(t, piAuthFile, originalPi)

	refreshed := map[string]any{
		"access":    "rotated-access",
		"refresh":   "rotated-refresh",
		"expires":   int64(1700007200000),
		"type":      "oauth",
		"accountId": "shared-acct",
	}

	err := syncRefreshedCredentialToBoth(config{
		AuthFile:   authFile,
		PiAuthFile: piAuthFile,
	}, refreshed, false)
	if err == nil {
		t.Fatalf("malformed Pi identity must surface as a clear error, not nil")
	}
	if !strings.Contains(err.Error(), "Pi auth identity check before refresh sync") {
		t.Fatalf("expected Pi-identity-check error prefix, got %v", err)
	}

	if got := string(mustReadFile(t, authFile)); got != originalOC {
		t.Fatalf("OC auth file was mutated despite identity-verification failure; got %q", got)
	}
	if got := string(mustReadFile(t, piAuthFile)); got != originalPi {
		t.Fatalf("Pi auth file was mutated despite identity-verification failure; got %q", got)
	}
}

// TestSyncRefreshedCredentialToBothRollsBackOCAfterPiFailure is the
// regression for blocker 2's post-mutation rollback: when the OC write
// succeeds and the Pi write fails (after the preflight passes), the helper
// must restore OC byte-for-byte from the pre-mutation snapshot and surface a
// joined partial-sync/rollback error. The "OC auth file restored" phrasing
// proves the rollback happened; the joined error format proves the rollback
// itself was reported.
func TestSyncRefreshedCredentialToBothRollsBackOCAfterPiFailure(t *testing.T) {
	t.Parallel()

	d := t.TempDir()
	authFile := filepath.Join(d, "opencode.json")
	piAuthFile := filepath.Join(d, "pi.json")

	originalOC := `{"openai.access":"old-oc","openai.refresh":"old-oc-r","openai.expires":1700003600000,"openai.type":"oauth"}`
	seedAuthFile(t, authFile, originalOC)
	// Both stores carry the SAME accountId so the account-identity guard
	// passes the Pi write through (the guard is verified separately; this
	// test focuses on the rollback mechanism when the Pi write FAILS).
	const sharedAcct = "shared-acct"
	seedAuthFile(t, piAuthFile, fmt.Sprintf(`{"openai-codex":{"type":"oauth","access":"old-pi","refresh":"r","expires":1700003600000,"accountId":%q}}`, sharedAcct))

	// Force the Pi write to fail by making the Pi file path unwritable AFTER
	// the preflight acquires and releases the lock. The OC write succeeds
	// first (OC owns its own file), then the Pi write fails on chmod/write.
	if err := os.Chmod(piAuthFile, 0o400); err != nil {
		t.Fatalf("seed chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(piAuthFile, 0o600) })

	refreshed := map[string]any{
		"access":    "rotated-access",
		"refresh":   "rotated-refresh",
		"expires":   int64(1700007200000),
		"type":      "oauth",
		"accountId": sharedAcct,
	}

	err := syncRefreshedCredentialToBoth(config{
		AuthFile:   authFile,
		PiAuthFile: piAuthFile,
	}, refreshed, false)
	if err == nil {
		t.Fatalf("expected Pi auth sync failure, got nil")
	}
	if !strings.Contains(err.Error(), "Pi auth sync failed") {
		t.Fatalf("expected Pi-sync-failed error phrasing, got %v", err)
	}
	if !strings.Contains(err.Error(), "OpenCode auth file restored") {
		t.Fatalf("expected rollback-from-snapshot phrasing in error, got %v", err)
	}
	// OC must be restored byte-for-byte to the pre-mutation snapshot.
	if got := string(mustReadFile(t, authFile)); got != originalOC {
		t.Fatalf("OC auth file was not rolled back; got %q", got)
	}
}

// TestRunDefaultCommandReactiveRefreshRewritesInMemoryPiInfo is the
// reactive regression for the in-memory piInfo update: when the OC usage
// call returns 401, the rotated credential must be mirrored into the
// in-memory piInfo snapshot BEFORE reconcilePiInboundFromPayload runs.
// Otherwise the inbound pass observes the stale pre-refresh access and
// either re-imports the stale value or triggers an unnecessary secondary
// fetch.
//
// Deterministic assertion: with the in-memory piInfo updated, the inbound
// reconciliation sees piInfo.piAccess == account["access"] == rotated
// access, so it MUST NOT call fetchUsageWindow with the stale access
// string. The test verifies this by returning 401 for the stale access
// (deterministic mismatch with the rotated access) and asserting that the
// helper is called with the rotated token and never the stale one.
func TestRunDefaultCommandReactiveRefreshRewritesInMemoryPiInfo(t *testing.T) {
	t.Parallel()

	fixedNow := time.Unix(1_700_000_000, 0)
	farMs := fixedNow.Add(2 * time.Hour).UnixMilli()
	rotatedMs := fixedNow.Add(time.Hour).UnixMilli()

	rotatedAccess := "rotated-shared-access"
	refreshServer, refreshHits := newRefreshServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"rotated-shared-refresh","expires_in":3600}`, rotatedAccess)
	})
	var usageHits atomic.Int32
	usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		usageHits.Add(1)
		auth := r.Header.Get("Authorization")
		if !strings.Contains(auth, rotatedAccess) {
			// Initial call with the stale access must return 401 so the
			// reactive helper triggers the refresh.
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(piPayload("user-cur", "11", "")))
	}))
	t.Cleanup(usageServer.Close)

	authFile, piAuthFile, accountsFile := piSyncFixture(t)
	staleAccess := "stale-shared-access"
	staleRefresh := "stale-shared-refresh"
	seedAuthFile(t, authFile, ocAuthBody(staleAccess, staleRefresh, farMs, "shared-acct"))
	seedAuthFile(t, piAuthFile, piAuthBody(staleAccess, staleRefresh, farMs, "shared-acct"))
	writeStore(t, accountsFile,
		map[string]any{
			"user_id": "user-cur", "access": staleAccess, "refresh": staleRefresh,
			"expires": int64(farMs), "type": "oauth", "accountId": "shared-acct",
		},
	)

	credMap := map[string]any{
		"type":      "oauth",
		"access":    staleAccess,
		"refresh":   staleRefresh,
		"expires":   int64(farMs),
		"accountId": "shared-acct",
	}
	piInfo := &piAuthInfo{Payload: map[string]any{"openai-codex": credMap}, CredMap: credMap}
	ocAccount := map[string]any{
		"access":    staleAccess,
		"refresh":   staleRefresh,
		"expires":   int64(farMs),
		"type":      "oauth",
		"accountId": "shared-acct",
	}

	var out strings.Builder
	if err := runDefaultCommandOpenCodePath(context.Background(), config{
		AuthFile:          authFile,
		PiAuthFile:        piAuthFile,
		AccountsFile:      accountsFile,
		UsageURL:          usageServer.URL,
		TokenURL:          refreshServer.URL,
		HTTPClient:        usageServer.Client(),
		Now:               func() time.Time { return fixedNow },
		FiveHourThreshold: 100,
		WeeklyThreshold:   100,
	}, &out, ocAccount, piInfo, true); err != nil {
		t.Fatalf("runDefaultCommandOpenCodePath: %v", err)
	}

	if v := refreshHits.Load(); v != 1 {
		t.Fatalf("refresh POSTs: got %d, want 1", v)
	}
	// usageHits must be exactly 2 (the initial 401 + the retry with the
	// rotated access). If the inbound reconciliation still saw the stale
	// access it would have made a THIRD call and the server would have
	// returned 401 again.
	if v := usageHits.Load(); v != 2 {
		t.Fatalf("usage POSTs: got %d, want exactly 2 (initial 401 + retry); inbound reconcile must NOT call with stale access", v)
	}

	assertOCAuth(t, authFile, rotatedAccess, "rotated-shared-refresh", rotatedMs, "shared-acct")
	assertPiCodex(t, readJSONObject(t, piAuthFile), rotatedAccess, "rotated-shared-refresh", rotatedMs, "shared-acct")

	if out.String() != "11" {
		t.Fatalf("stdout=%q, want %q", out.String(), "11")
	}

	// In-memory piInfo must reflect the rotated tuple. Without the fix this
	// would still carry the stale access string.
	if got, _ := piInfo.CredMap["access"].(string); got != rotatedAccess {
		t.Fatalf("piInfo.CredMap access: got %q, want %q (stale access would re-introduce the pre-refresh credential on the inbound pass)", got, rotatedAccess)
	}
}

// ----------------------------------------------------------------------------
// Guarded Pi-source recovery tests (selectRefreshSourceFromPi +
// fetchUsageWithReactiveRefresh's piInfo hook). These pin the contract for the
// 401-driven reactive refresh: when Pi holds a demonstrably same-account and
// newer/safer OAuth tuple, the OC path must consume Pi's refresh token in the
// single allowed refresh POST, and the rotated tuple must converge into both
// installed auth stores. Different accounts MUST NOT be substituted.
// ----------------------------------------------------------------------------

// TestSelectRefreshSourceFromPiRules is the unit-level pin for the selection
// rules. Each subtest seeds a distinct OC/Pi pair and asserts whether the
// helper substitutes Pi (returns a fresh merged copy) or leaves OC untouched
// (returns OC verbatim).
func TestSelectRefreshSourceFromPiRules(t *testing.T) {
	t.Parallel()

	nowMs := int64(1_700_000_000_000)
	later := nowMs + int64(time.Hour/time.Millisecond)
	earlier := nowMs - int64(time.Hour/time.Millisecond)

	mkOC := func(access, refresh, accountID string, expires int64) map[string]any {
		return map[string]any{
			"type":      "oauth",
			"access":    access,
			"refresh":   refresh,
			"expires":   expires,
			"accountId": accountID,
			"user_id":   "user-oc",
			"email":     "oc@example.com",
		}
	}

	t.Run("same_access_identical_refresh_no_swap", func(t *testing.T) {
		t.Parallel()
		oc := mkOC("shared", "oc-r", "shared-acct", later)
		credMap := map[string]any{
			"type":      "oauth",
			"access":    "shared",
			"refresh":   "oc-r",
			"expires":   later,
			"accountId": "shared-acct",
		}
		piInfo := &piAuthInfo{Payload: map[string]any{"openai-codex": credMap}, CredMap: credMap}
		got := selectRefreshSourceFromPi(oc, piInfo)
		// Same access + same refresh + same expires -> nothing to swap.
		if got["refresh"] != "oc-r" {
			t.Fatalf("refresh unexpectedly swapped: got %v", got["refresh"])
		}
		if got["access"] != "shared" {
			t.Fatalf("access mutated: got %v", got["access"])
		}
	})

	t.Run("same_access_different_refresh_swaps_to_pi", func(t *testing.T) {
		t.Parallel()
		oc := mkOC("shared", "oc-r-stale", "oc-acct", later)
		credMap := map[string]any{
			"type":      "oauth",
			"access":    "shared",
			"refresh":   "pi-r-fresh",
			"expires":   later,
			"accountId": "pi-acct",
		}
		piInfo := &piAuthInfo{Payload: map[string]any{"openai-codex": credMap}, CredMap: credMap}
		got := selectRefreshSourceFromPi(oc, piInfo)
		if got["refresh"] != "pi-r-fresh" {
			t.Fatalf("expected Pi refresh, got %v", got["refresh"])
		}
		if got["accountId"] != "pi-acct" {
			t.Fatalf("expected Pi accountId merged, got %v", got["accountId"])
		}
		// OC metadata preserved.
		if got["user_id"] != "user-oc" || got["email"] != "oc@example.com" {
			t.Fatalf("OC metadata lost: got %v", got)
		}
		// OC input was never mutated.
		if oc["refresh"] != "oc-r-stale" {
			t.Fatalf("OC input mutated: %v", oc)
		}
	})

	t.Run("same_accountId_pi_expires_strictly_greater_swaps_to_pi", func(t *testing.T) {
		t.Parallel()
		oc := mkOC("oc-a", "oc-r", "shared-acct", nowMs)
		credMap := map[string]any{
			"type":      "oauth",
			"access":    "pi-a-rotated",
			"refresh":   "pi-r",
			"expires":   later,
			"accountId": "shared-acct",
		}
		piInfo := &piAuthInfo{Payload: map[string]any{"openai-codex": credMap}, CredMap: credMap}
		got := selectRefreshSourceFromPi(oc, piInfo)
		if got["access"] != "pi-a-rotated" {
			t.Fatalf("expected Pi access merged, got %v", got["access"])
		}
		if got["refresh"] != "pi-r" {
			t.Fatalf("expected Pi refresh merged, got %v", got["refresh"])
		}
		if got["expires"] != later {
			t.Fatalf("expected Pi expires merged, got %v", got["expires"])
		}
	})

	t.Run("same_accountId_pi_expires_equal_no_swap", func(t *testing.T) {
		t.Parallel()
		oc := mkOC("oc-a", "oc-r", "shared-acct", later)
		credMap := map[string]any{
			"type":      "oauth",
			"access":    "pi-a",
			"refresh":   "pi-r",
			"expires":   later,
			"accountId": "shared-acct",
		}
		piInfo := &piAuthInfo{Payload: map[string]any{"openai-codex": credMap}, CredMap: credMap}
		got := selectRefreshSourceFromPi(oc, piInfo)
		if got["access"] != "oc-a" {
			t.Fatalf("expected OC access unchanged, got %v", got["access"])
		}
		if got["refresh"] != "oc-r" {
			t.Fatalf("expected OC refresh unchanged, got %v", got["refresh"])
		}
	})

	t.Run("same_accountId_pi_expires_older_no_swap", func(t *testing.T) {
		t.Parallel()
		oc := mkOC("oc-a", "oc-r", "shared-acct", later)
		credMap := map[string]any{
			"type":      "oauth",
			"access":    "pi-a",
			"refresh":   "pi-r",
			"expires":   earlier,
			"accountId": "shared-acct",
		}
		piInfo := &piAuthInfo{Payload: map[string]any{"openai-codex": credMap}, CredMap: credMap}
		got := selectRefreshSourceFromPi(oc, piInfo)
		if got["access"] != "oc-a" {
			t.Fatalf("expected OC access unchanged when Pi is older, got %v", got["access"])
		}
	})

	t.Run("different_accountId_and_different_access_no_swap", func(t *testing.T) {
		t.Parallel()
		oc := mkOC("oc-a", "oc-r", "oc-acct", earlier)
		credMap := map[string]any{
			"type":      "oauth",
			"access":    "pi-a",
			"refresh":   "pi-r",
			"expires":   later,
			"accountId": "pi-acct",
		}
		piInfo := &piAuthInfo{Payload: map[string]any{"openai-codex": credMap}, CredMap: credMap}
		got := selectRefreshSourceFromPi(oc, piInfo)
		if got["access"] != "oc-a" {
			t.Fatalf("expected OC unchanged for different accounts, got %v", got)
		}
		if got["refresh"] != "oc-r" {
			t.Fatalf("expected OC refresh unchanged, got %v", got)
		}
	})

	t.Run("nil_piInfo_returns_oc_unchanged", func(t *testing.T) {
		t.Parallel()
		oc := mkOC("oc-a", "oc-r", "oc-acct", nowMs)
		got := selectRefreshSourceFromPi(oc, nil)
		if got["access"] != "oc-a" || got["refresh"] != "oc-r" {
			t.Fatalf("nil piInfo must return OC unchanged, got %v", got)
		}
	})

	t.Run("nil_CredMap_returns_oc_unchanged", func(t *testing.T) {
		t.Parallel()
		oc := mkOC("oc-a", "oc-r", "oc-acct", nowMs)
		piInfo := &piAuthInfo{Payload: map[string]any{}, CredMap: nil}
		got := selectRefreshSourceFromPi(oc, piInfo)
		if got["access"] != "oc-a" || got["refresh"] != "oc-r" {
			t.Fatalf("nil CredMap must return OC unchanged, got %v", got)
		}
	})

	t.Run("malformed_Pi_returns_oc_unchanged", func(t *testing.T) {
		t.Parallel()
		oc := mkOC("oc-a", "oc-r", "oc-acct", nowMs)
		// Missing refresh makes Pi's parsePiOpenAICodexEntry fail.
		credMap := map[string]any{
			"type":    "oauth",
			"access":  "oc-a",
			"expires": nowMs,
		}
		piInfo := &piAuthInfo{Payload: map[string]any{"openai-codex": credMap}, CredMap: credMap}
		got := selectRefreshSourceFromPi(oc, piInfo)
		if got["refresh"] != "oc-r" {
			t.Fatalf("malformed Pi must not substitute, got %v", got["refresh"])
		}
	})

	t.Run("blank_accountId_does_not_match_blank_accountId", func(t *testing.T) {
		t.Parallel()
		oc := mkOC("oc-a", "oc-r", "", nowMs)
		credMap := map[string]any{
			"type":      "oauth",
			"access":    "pi-a",
			"refresh":   "pi-r",
			"expires":   later,
			"accountId": "",
		}
		piInfo := &piAuthInfo{Payload: map[string]any{"openai-codex": credMap}, CredMap: credMap}
		got := selectRefreshSourceFromPi(oc, piInfo)
		// Blank accountIds on both sides must NOT count as a match.
		if got["access"] != "oc-a" {
			t.Fatalf("blank accountIds must not match: got %v", got)
		}
	})

	t.Run("pi_blank_accountId_does_not_overwrite_oc_accountId", func(t *testing.T) {
		t.Parallel()
		oc := mkOC("oc-a", "oc-r", "shared-acct", nowMs)
		credMap := map[string]any{
			"type":      "oauth",
			"access":    "oc-a",
			"refresh":   "pi-r-fresh",
			"expires":   later,
			"accountId": "", // Pi has no accountId; OC has one.
		}
		piInfo := &piAuthInfo{Payload: map[string]any{"openai-codex": credMap}, CredMap: credMap}
		got := selectRefreshSourceFromPi(oc, piInfo)
		// Same access + different refresh -> Pi wins. Blank Pi accountId must
		// not clobber OC's accountId.
		if got["accountId"] != "shared-acct" {
			t.Fatalf("expected OC accountId preserved, got %v", got["accountId"])
		}
		if got["refresh"] != "pi-r-fresh" {
			t.Fatalf("expected Pi refresh merged, got %v", got["refresh"])
		}
	})
}

// TestRunDefaultCommandPiRotatedRefreshUsesPiRefreshToken pins the
// guarded-Pi-source recovery contract end-to-end for the OC default path:
//  1. OpenCode's refresh token is stale (R1) but its access token is still
//     the same string as Pi's (Pi rotated the refresh upstream).
//  2. The OC usage fetch with that access token returns 401 (simulating
//     OpenCode's session being invalidated).
//  3. The reactive helper sees Pi holds the same access + a newer refresh,
//     swaps the refresh source to Pi BEFORE the single refresh POST, and
//     sends R2 to the OAuth endpoint.
//  4. The refresh POST succeeds, the rotated tuple is persisted to both
//     installed auth stores via syncRefreshedCredentialToBoth, and the
//     usage retry succeeds with the rotated access.
//
// The contract: exactly ONE refresh POST, the POST sends R2 (not R1), and
// both stores converge on the rotated tuple.
func TestRunDefaultCommandPiRotatedRefreshUsesPiRefreshToken(t *testing.T) {
	t.Parallel()

	fixedNow := time.Unix(1_700_000_000, 0)
	staleOCExpiresMs := fixedNow.Add(2 * time.Hour).UnixMilli()
	piExpiresMs := fixedNow.Add(3 * time.Hour).UnixMilli()
	rotatedExpiresMs := fixedNow.Add(time.Hour).UnixMilli()

	const ocStaleRefresh = "OC-R1-STALE"
	const piFreshRefresh = "PI-R2-FRESH"
	const sharedAccess = "shared-access-xyz"

	var capturedRefresh atomic.Value
	refreshServer, refreshHits := newRefreshServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		capturedRefresh.Store(r.PostForm.Get("refresh_token"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}`)
	})

	var usageHits atomic.Int32
	usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		usageHits.Add(1)
		auth := r.Header.Get("Authorization")
		if !strings.Contains(auth, "rotated-access") {
			// Initial call with the stale OC access must return 401 so the
			// reactive helper triggers the refresh flow.
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(piPayload("user-cur", "77", "")))
	}))
	t.Cleanup(usageServer.Close)

	authFile, piAuthFile, accountsFile := piSyncFixture(t)
	seedAuthFile(t, authFile, ocAuthBody(sharedAccess, ocStaleRefresh, staleOCExpiresMs, "shared-acct"))
	seedAuthFile(t, piAuthFile, piAuthBody(sharedAccess, piFreshRefresh, piExpiresMs, "shared-acct"))
	writeStore(t, accountsFile,
		map[string]any{"user_id": "user-cur", "access": sharedAccess, "refresh": ocStaleRefresh,
			"expires": int64(staleOCExpiresMs), "type": "oauth", "accountId": "shared-acct"},
	)

	credMap := map[string]any{
		"type":      "oauth",
		"access":    sharedAccess,
		"refresh":   piFreshRefresh,
		"expires":   int64(piExpiresMs),
		"accountId": "shared-acct",
	}
	piInfo := &piAuthInfo{Payload: map[string]any{"openai-codex": credMap}, CredMap: credMap}
	ocAccount := map[string]any{
		"access":    sharedAccess,
		"refresh":   ocStaleRefresh,
		"expires":   int64(staleOCExpiresMs),
		"type":      "oauth",
		"accountId": "shared-acct",
	}

	var out strings.Builder
	if err := runDefaultCommandOpenCodePath(context.Background(), config{
		AuthFile:          authFile,
		PiAuthFile:        piAuthFile,
		AccountsFile:      accountsFile,
		UsageURL:          usageServer.URL,
		TokenURL:          refreshServer.URL,
		HTTPClient:        usageServer.Client(),
		Now:               func() time.Time { return fixedNow },
		FiveHourThreshold: 100,
		WeeklyThreshold:   100,
	}, &out, ocAccount, piInfo, true); err != nil {
		t.Fatalf("runDefaultCommandOpenCodePath: %v", err)
	}

	// Exactly ONE refresh POST: no recursion, no second attempt.
	if v := refreshHits.Load(); v != 1 {
		t.Fatalf("refresh POSTs: got %d, want 1", v)
	}
	// Usage calls: 1 initial 401 + 1 retry with rotated access = 2.
	if v := usageHits.Load(); v != 2 {
		t.Fatalf("usage POSTs: got %d, want 2 (initial 401 + retry)", v)
	}
	// The refresh POST must have used Pi's fresh refresh token R2 (NOT OC's
	// stale R1). This is the guarded Pi-source recovery: the OC refresh was
	// demonstrably stale, Pi's was demonstrably newer/safer (same access +
	// different refresh on the lock-protected rotating store).
	if got, _ := capturedRefresh.Load().(string); got != piFreshRefresh {
		t.Fatalf("refresh token sent to OAuth endpoint: got %q, want Pi fresh %q (stale OC %q would 401 the OAuth endpoint)", got, piFreshRefresh, ocStaleRefresh)
	}
	if out.String() != "77" {
		t.Fatalf("stdout=%q, want %q", out.String(), "77")
	}
	// Both stores converge on the rotated tuple.
	assertOCAuth(t, authFile, "rotated-access", "rotated-refresh", rotatedExpiresMs, "shared-acct")
	assertPiCodex(t, readJSONObject(t, piAuthFile), "rotated-access", "rotated-refresh", rotatedExpiresMs, "shared-acct")
	// The captured refresh token must NOT appear in any error/stdout surface
	// the test can read; sanity-check the rotated refresh is also absent.
	rendered := out.String()
	for _, secret := range []string{ocStaleRefresh, piFreshRefresh, "rotated-refresh"} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("stdout leaked secret %q: %q", secret, rendered)
		}
	}
}

// TestRunAccountsCommandCurrentRowPiRotatedUsesPiRefreshToken pins the
// guarded-Pi-source recovery contract for a CURRENT row in accounts/list:
// when OC's current token matches the row AND Pi holds a demonstrably
// same-account newer tuple, the refresh POST uses Pi's refresh token
// (R2), the rotated tuple is mirrored to both installed stores via
// syncRefreshedCredentialToBoth, and the CURRENT marker is preserved.
//
// Non-CURRENT rows continue using their own stored refresh so they stay
// isolated from any out-of-band Pi rotation unrelated to the active session.
func TestRunAccountsCommandCurrentRowPiRotatedUsesPiRefreshToken(t *testing.T) {
	t.Parallel()

	fixedNow := time.Unix(1_700_000_000, 0)
	staleOCExpiresMs := fixedNow.Add(2 * time.Hour).UnixMilli()
	piExpiresMs := fixedNow.Add(3 * time.Hour).UnixMilli()
	rotatedExpiresMs := fixedNow.Add(time.Hour).UnixMilli()

	const ocStaleRefresh = "OC-R1-STALE-CURRENT"
	const piFreshRefresh = "PI-R2-FRESH-CURRENT"
	const sharedAccess = "shared-access-current"

	var capturedRefresh atomic.Value
	refreshServer, refreshHits := newRefreshServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		capturedRefresh.Store(r.PostForm.Get("refresh_token"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"rotated-c-access","refresh_token":"rotated-c-refresh","expires_in":3600}`)
	})

	var usageHits atomic.Int32
	usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		usageHits.Add(1)
		auth := r.Header.Get("Authorization")
		switch {
		case strings.Contains(auth, "rotated-c-access"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_id":"user-cur","email":"cur@example.com","rate_limit":{"primary_window":{"used_percent":55,"reset_at":1777414800}}}`))
		case strings.Contains(auth, sharedAccess):
			// CURRENT row initial call: stale access must 401.
			w.WriteHeader(http.StatusUnauthorized)
		case strings.Contains(auth, "other-access"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_id":"user-other","email":"other@example.com","rate_limit":{"primary_window":{"used_percent":66,"reset_at":1777414800}}}`))
		default:
			t.Errorf("unexpected usage Authorization: %q", auth)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(usageServer.Close)

	authFile, piAuthFile, accountsFile := piSyncFixture(t)
	seedAuthFile(t, authFile, ocAuthBody(sharedAccess, ocStaleRefresh, staleOCExpiresMs, "shared-acct"))
	seedAuthFile(t, piAuthFile, piAuthBody(sharedAccess, piFreshRefresh, piExpiresMs, "shared-acct"))
	writeStore(t, accountsFile,
		map[string]any{"user_id": "user-cur", "access": sharedAccess, "refresh": ocStaleRefresh,
			"expires": int64(staleOCExpiresMs), "type": "oauth", "accountId": "shared-acct"},
		map[string]any{"user_id": "user-other", "access": "other-access", "refresh": "other-refresh",
			"expires": int64(staleOCExpiresMs), "type": "oauth", "accountId": "other-acct"},
	)

	var out strings.Builder
	if err := runAccountsCommand(context.Background(), config{
		AuthFile:     authFile,
		PiAuthFile:   piAuthFile,
		AccountsFile: accountsFile,
		UsageURL:     usageServer.URL,
		TokenURL:     refreshServer.URL,
		HTTPClient:   usageServer.Client(),
		Now:          func() time.Time { return fixedNow },
	}, &out); err != nil {
		t.Fatalf("runAccountsCommand: %v", err)
	}

	if v := refreshHits.Load(); v != 1 {
		t.Fatalf("refresh POSTs: got %d, want exactly 1 (single reactive POST)", v)
	}
	// The POST sent Pi's fresh refresh (R2), NOT OC's stale R1.
	if got, _ := capturedRefresh.Load().(string); got != piFreshRefresh {
		t.Fatalf("refresh token: got %q, want Pi fresh %q", got, piFreshRefresh)
	}
	// Both stores converge on the rotated tuple.
	assertOCAuth(t, authFile, "rotated-c-access", "rotated-c-refresh", rotatedExpiresMs, "shared-acct")
	assertPiCodex(t, readJSONObject(t, piAuthFile), "rotated-c-access", "rotated-c-refresh", rotatedExpiresMs, "shared-acct")
	if !strings.Contains(out.String(), "*") {
		t.Fatalf("CURRENT marker must be preserved across reactive refresh: %q", out.String())
	}
	// Non-current row must not be touched.
	store := mustReadStore(t, accountsFile)
	if v, _ := store["user-other"]["access"].(string); v != "other-access" {
		t.Fatalf("non-current row mutated: %v", store["user-other"])
	}
	// Sanity: no secret leaks through stdout.
	rendered := out.String()
	for _, secret := range []string{ocStaleRefresh, piFreshRefresh, "rotated-c-refresh"} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("stdout leaked secret %q: %q", secret, rendered)
		}
	}
}

// TestRunAccountsCommandCurrentRowDifferentAccountNeverUsesPi pins the
// "different account" guard: when OC's CURRENT row matches the OC auth file
// by access token but Pi holds a different accountId AND a different access
// token, the helper MUST NOT substitute Pi's refresh. Pi's credentials are
// the active session of a DIFFERENT account and must never bleed into the
// current row's refresh.
func TestRunAccountsCommandCurrentRowDifferentAccountNeverUsesPi(t *testing.T) {
	t.Parallel()

	fixedNow := time.Unix(1_700_000_000, 0)
	farMs := fixedNow.Add(2 * time.Hour).UnixMilli()

	var capturedRefresh atomic.Value
	refreshServer, refreshHits := newRefreshServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		capturedRefresh.Store(r.PostForm.Get("refresh_token"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"rotated-cur-access","refresh_token":"rotated-cur-refresh","expires_in":3600}`)
	})

	usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.Contains(auth, "rotated-cur-access") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"user-cur","email":"cur@example.com","rate_limit":{"primary_window":{"used_percent":11,"reset_at":1777414800}}}`))
	}))
	t.Cleanup(usageServer.Close)

	authFile, piAuthFile, accountsFile := piSyncFixture(t)
	const ocAccess = "oc-cur-access"
	const piAccess = "pi-other-access" // different access
	const ocAcct = "oc-acct"
	const piAcct = "pi-acct" // different accountId
	seedAuthFile(t, authFile, ocAuthBody(ocAccess, "oc-r-cur", farMs, ocAcct))
	seedAuthFile(t, piAuthFile, piAuthBody(piAccess, "pi-r-other", farMs, piAcct))
	writeStore(t, accountsFile,
		map[string]any{"user_id": "user-cur", "access": ocAccess, "refresh": "oc-r-cur",
			"expires": int64(farMs), "type": "oauth", "accountId": ocAcct},
	)

	var out strings.Builder
	if err := runAccountsCommand(context.Background(), config{
		AuthFile:     authFile,
		PiAuthFile:   piAuthFile,
		AccountsFile: accountsFile,
		UsageURL:     usageServer.URL,
		TokenURL:     refreshServer.URL,
		HTTPClient:   usageServer.Client(),
		Now:          func() time.Time { return fixedNow },
	}, &out); err != nil {
		t.Fatalf("runAccountsCommand: %v", err)
	}
	if v := refreshHits.Load(); v != 1 {
		t.Fatalf("refresh POSTs: got %d, want 1", v)
	}
	// The POST must have used OC's refresh, NOT Pi's. Different accounts
	// must NEVER be substituted: pi-r-other is a credential for a different
	// identity and using it would silently log the current row into Pi's
	// account.
	if got, _ := capturedRefresh.Load().(string); got != "oc-r-cur" {
		t.Fatalf("refresh token: got %q, want OC's %q (Pi's %q belongs to a different account and must NOT be substituted)", got, "oc-r-cur", "pi-r-other")
	}
	// OC auth file must carry the rotated tuple.
	assertOCAuth(t, authFile, "rotated-cur-access", "rotated-cur-refresh", fixedNow.Add(time.Hour).UnixMilli(), ocAcct)
	// Pi auth file must remain UNTOUCHED (different account, never
	// substituted).
	assertPiCodex(t, readJSONObject(t, piAuthFile), piAccess, "pi-r-other", farMs, piAcct)
}

// TestRefreshOAuthEndpointReturns401IsActionableNoLeak pins the actionable
// re-login contract: when the OAuth refresh endpoint itself returns 401
// (the supplied refresh token is rejected), the CLI must surface a clear
// "sign in again" error without echoing the response body, the refresh
// token, the access token, or any other credential material. Other
// non-200 statuses continue to use the generic "OAuth refresh endpoint
// returned N" phrasing.
func TestRefreshOAuthEndpointReturns401IsActionableNoLeak(t *testing.T) {
	t.Parallel()

	fixedNow := time.Unix(1_700_000_000, 0)
	rotatedExpiresMs := fixedNow.Add(time.Hour).UnixMilli()

	const ocAccess = "oc-cur-access"
	const ocRefresh = "OC-REFRESH-SECRET-XYZ"
	const ocAcct = "oc-acct"

	// OAuth endpoint echoes back the secret refresh token in the body to
	// prove the helper swallows it.
	refreshServer, refreshHits := newRefreshServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, `{"error":"invalid_grant","refresh_token":%q,"access_token":%q}`, ocRefresh, ocAccess)
	})

	usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(usageServer.Close)

	authFile, _, accountsFile := piSyncFixture(t)
	seedAuthFile(t, authFile, ocAuthBody(ocAccess, ocRefresh, rotatedExpiresMs, ocAcct))
	writeStore(t, accountsFile,
		map[string]any{"user_id": "user-cur", "access": ocAccess, "refresh": ocRefresh,
			"expires": int64(rotatedExpiresMs), "type": "oauth", "accountId": ocAcct},
	)

	var out strings.Builder
	err := runAccountsCommand(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		UsageURL:     usageServer.URL,
		TokenURL:     refreshServer.URL,
		HTTPClient:   usageServer.Client(),
		Now:          func() time.Time { return fixedNow },
	}, &out)
	if err == nil {
		t.Fatalf("expected overall error from refresh 401, got nil; stdout=%q", out.String())
	}
	// The user-visible surfaces (the rendered row error + overall error)
	// MUST carry the actionable re-login phrase and MUST NOT leak
	// credential material. The phrase appears in the row.Err rendered into
	// stdout via `[ERR: ...]`; the overall error is the generic
	// "all rows failed" diagnostic.
	const wantPhrase = "OAuth refresh token was rejected; sign in again"
	if !strings.Contains(out.String(), wantPhrase) {
		t.Fatalf("stdout must carry %q, got %q", wantPhrase, out.String())
	}
	if !strings.Contains(err.Error(), "failed to fetch usage for all saved accounts") {
		t.Fatalf("expected overall-failure diagnostic, got %q", err.Error())
	}
	for _, secret := range []string{ocAccess, ocRefresh, "invalid_grant"} {
		for _, surface := range []string{err.Error(), out.String()} {
			if strings.Contains(surface, secret) {
				t.Fatalf("surface leaked %q: %q", secret, surface)
			}
		}
	}
	// The OAuth endpoint was hit exactly once; no retry, no recursion.
	if v := refreshHits.Load(); v != 1 {
		t.Fatalf("refresh POSTs: got %d, want exactly 1", v)
	}
	// Auth bytes must remain unchanged on a refresh failure (persistFn was
	// never invoked).
	assertOCAuth(t, authFile, ocAccess, ocRefresh, rotatedExpiresMs, ocAcct)
}

// TestRefreshOAuthEndpointReturnsOtherStatusStaysStatusOnly pins the
// non-401 status phrasing: a 400 from the OAuth refresh endpoint must use
// the existing "OAuth refresh endpoint returned 400" phrasing and MUST
// NOT include the actionable re-login phrase (which is reserved for
// 401). This preserves the documented non-401 status semantics.
func TestRefreshOAuthEndpointReturnsOtherStatusStaysStatusOnly(t *testing.T) {
	t.Parallel()

	fixedNow := time.Unix(1_700_000_000, 0)
	farMs := fixedNow.Add(2 * time.Hour).UnixMilli()

	refreshServer, _ := newRefreshServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":"invalid_request"}`)
	})

	usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(usageServer.Close)

	authFile, _, accountsFile := piSyncFixture(t)
	seedAuthFile(t, authFile, ocAuthBody("oc-a", "oc-r", farMs, "oc-acct"))
	writeStore(t, accountsFile,
		map[string]any{"user_id": "user-cur", "access": "oc-a", "refresh": "oc-r",
			"expires": int64(farMs), "type": "oauth", "accountId": "oc-acct"},
	)

	var out strings.Builder
	err := runAccountsCommand(context.Background(), config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		UsageURL:     usageServer.URL,
		TokenURL:     refreshServer.URL,
		HTTPClient:   usageServer.Client(),
		Now:          func() time.Time { return fixedNow },
	}, &out)
	if err == nil {
		t.Fatalf("expected overall error from refresh 400, got nil")
	}
	// The row error carries the non-401 status phrasing and is rendered
	// into stdout via `[ERR: ...]`. The actionable re-login phrase is
	// reserved for the 401 case.
	if !strings.Contains(out.String(), "OAuth refresh endpoint returned 400") {
		t.Fatalf("non-401 must keep status phrasing in stdout, got %q", out.String())
	}
	if strings.Contains(out.String(), "sign in again") {
		t.Fatalf("non-401 must NOT carry the actionable re-login phrase, got %q", out.String())
	}
	if strings.Contains(err.Error(), "invalid_request") || strings.Contains(out.String(), "invalid_request") {
		t.Fatalf("non-401 error leaked response body: err=%q stdout=%q", err.Error(), out.String())
	}
}

// TestRefreshOAuthRequestExactPiParity pins the wire-level parity with
// pi-main's OAuth refresh request for the fields our code controls: the
// request body MUST be emitted in the documented insertion order
// `grant_type`, `refresh_token`, `client_id` with standard
// `application/x-www-form-urlencoded` percent-escaping, the Content-Type
// MUST be `application/x-www-form-urlencoded`, and the request MUST NOT
// carry an Accept header.
//
// Go's net/http transport adds `Accept-Encoding: gzip` by default; that
// is a runtime default outside the explicit request generated by our code
// and is documented as such in the README. This test deliberately avoids
// asserting against Accept-Encoding so a regression that re-introduces the
// Accept header or sorts the body fields alphabetically (as
// `url.Values.Encode` would) is caught by the body/header assertions
// below.
//
// The refresh token used here intentionally contains characters that
// `url.QueryEscape` rewrites (space, slash, ampersand, equals, plus) so a
// regression that swapped the primitive back to `fmt.Fprintf` with raw
// concatenation would produce visibly different bytes.
func TestRefreshOAuthRequestExactPiParity(t *testing.T) {
	t.Parallel()

	// refresh token deliberately encodes every special character that
	// `url.QueryEscape` rewrites, so a regression that loses the escape
	// primitive becomes visible in the captured body.
	const trickyRefresh = "rt+with space/and&special=chars"

	var capturedMethod atomic.Value
	var capturedPath atomic.Value
	var capturedRawBody atomic.Value
	var capturedHeaders http.Header

	server, hits := newRefreshServer(t, func(w http.ResponseWriter, r *http.Request) {
		capturedMethod.Store(r.Method)
		capturedPath.Store(r.URL.Path)
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		capturedRawBody.Store(string(rawBody))
		capturedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"x","refresh_token":"y","expires_in":3600}`)
	})

	_, _, _, err := performOAuthRefreshRequest(context.Background(), config{
		TokenURL:   server.URL,
		HTTPClient: server.Client(),
		Now:        func() time.Time { return time.Unix(1_700_000_000, 0) },
	}, trickyRefresh)
	if err != nil {
		t.Fatalf("performOAuthRefreshRequest: %v", err)
	}
	if v := hits.Load(); v != 1 {
		t.Fatalf("expected exactly 1 POST, got %d", v)
	}

	// Method + path: standard POST to the configured token URL.
	if got, _ := capturedMethod.Load().(string); got != http.MethodPost {
		t.Fatalf("method: got %q, want POST", got)
	}
	if got, _ := capturedPath.Load().(string); got != "/" {
		t.Fatalf("path: got %q, want /", got)
	}

	// Raw body MUST be the documented insertion order with url.QueryEscape
	// percent-encoding, byte-for-byte. `client_id` MUST come AFTER
	// `refresh_token` (not before, as a `url.Values.Encode` call would have
	// emitted), and the refresh_token MUST have its special characters
	// percent-escaped — `+` as `%2B`, space as `+`, `/` as `%2F`, `&` as
	// `%26`, `=` as `%3D`.
	wantRaw := "grant_type=refresh_token&refresh_token=" +
		url.QueryEscape(trickyRefresh) +
		"&client_id=" + url.QueryEscape(oauthRefreshClientID)
	if got, _ := capturedRawBody.Load().(string); got != wantRaw {
		t.Fatalf("raw body byte mismatch:\n got %q\nwant %q", got, wantRaw)
	}

	// Content-Type MUST be `application/x-www-form-urlencoded`.
	if got := capturedHeaders.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
		t.Fatalf("Content-Type: got %q, want application/x-www-form-urlencoded", got)
	}
	// No Accept header: the only Accept header our code used to add was
	// `Accept: application/json`; removing it is the explicit ask of the
	// parity fix.
	if _, present := capturedHeaders["Accept"]; present {
		t.Fatalf("Accept header must NOT be set on the refresh POST (got %q); pi-main parity", capturedHeaders.Get("Accept"))
	}
}

// TestSyncAccountToPiAuthWritesRotatedTupleWithoutAccountID is the
// regression for the Pi-only refresh contract: when the helper is invoked
// from the Pi-only refresh path and the rotated credential lacks an
// accountId (the new access token's JWT carries no chatgpt_account_id
// claim), it MUST still persist the rotated tuple to Pi's auth.json.
// Previously the broad identity-based skip in syncAccountToPiAuth would
// silently no-op a rotated access that no longer matched the on-disk Pi
// access (which is always the case after a rotation), leaking the rotated
// credential to memory but never to disk.
func TestSyncAccountToPiAuthWritesRotatedTupleWithoutAccountID(t *testing.T) {
	t.Parallel()

	d := t.TempDir()
	piAuthFile := filepath.Join(d, "pi.json")

	// Pi's pre-refresh credential: access+refresh+expires+accountId. The
	// rotated credential will have the same shape but a different access
	// (typical rotation) and NO accountId at all (simulating a JWT whose
	// chatgpt_account_id claim is missing/empty).
	originalPi := fmt.Sprintf(`{"openai-codex":{"type":"oauth","access":"old-pi","refresh":"old-pi-r","expires":1700003600000,"accountId":"pi-acct"}}`)
	seedAuthFile(t, piAuthFile, originalPi)

	rotated := map[string]any{
		"access":  "rotated-pi-access",
		"refresh": "rotated-pi-refresh",
		"expires": int64(1700007200000),
		"type":    "oauth",
		// accountId intentionally absent: Pi-only refresh must still
		// persist the rotated tuple to disk.
	}

	if err := syncAccountToPiAuth(config{PiAuthFile: piAuthFile}, rotated); err != nil {
		t.Fatalf("syncAccountToPiAuth must persist the rotated Pi tuple even when accountId is missing, got: %v", err)
	}

	// Pi MUST carry the rotated tuple. accountId is omitted from the
	// rotated payload so Pi's on-disk accountId key MUST also be absent
	// (Pi never invents identity metadata that isn't in the rotated tuple).
	assertPiCodex(t, readJSONObject(t, piAuthFile), "rotated-pi-access", "rotated-pi-refresh", 1700007200000, "")
}

// TestSyncAccountToPiAuthWritesRotatedTupleWithChangedAccountID is the
// companion regression for the same Pi-only contract: a rotation whose
// derived accountId differs from the on-disk one (e.g. the JWT carries a
// new chatgpt_account_id) MUST also be persisted. The on-disk accountId is
// the Pi session's identity; once Pi just rotated its own access token via
// the OAuth endpoint, the rotated accountId IS the Pi session's identity.
func TestSyncAccountToPiAuthWritesRotatedTupleWithChangedAccountID(t *testing.T) {
	t.Parallel()

	d := t.TempDir()
	piAuthFile := filepath.Join(d, "pi.json")

	originalPi := fmt.Sprintf(`{"openai-codex":{"type":"oauth","access":"old-pi","refresh":"old-pi-r","expires":1700003600000,"accountId":"old-pi-acct"}}`)
	seedAuthFile(t, piAuthFile, originalPi)

	rotated := map[string]any{
		"access":    "rotated-pi-access",
		"refresh":   "rotated-pi-refresh",
		"expires":   int64(1700007200000),
		"type":      "oauth",
		"accountId": "rotated-pi-acct",
	}

	if err := syncAccountToPiAuth(config{PiAuthFile: piAuthFile}, rotated); err != nil {
		t.Fatalf("syncAccountToPiAuth must persist the rotated Pi tuple even when accountId changed, got: %v", err)
	}

	assertPiCodex(t, readJSONObject(t, piAuthFile), "rotated-pi-access", "rotated-pi-refresh", 1700007200000, "rotated-pi-acct")
}

// TestSyncRefreshedCredentialToBothDifferentAccountSkipsPiWithoutTouchingLock
// is the dual-provider regression for the account-identity guard hoisted
// from syncAccountToPiAuth: when OC and Pi are both installed but Pi
// holds a different accountId AND a different access token, a successful
// OC refresh MUST propagate to OC only and MUST leave Pi's auth.json
// byte-identical to its pre-call state. The Pi lock MUST NOT be acquired
// by the helper — a DIFFERENT account's rotated OpenCode credential is
// never allowed to touch Pi's session.
func TestSyncRefreshedCredentialToBothDifferentAccountSkipsPiWithoutTouchingLock(t *testing.T) {
	t.Parallel()

	d := t.TempDir()
	authFile := filepath.Join(d, "opencode.json")
	piAuthFile := filepath.Join(d, "pi.json")

	const ocAcct = "oc-acct"
	const piAcct = "pi-acct"
	originalOC := fmt.Sprintf(`{"openai.access":"oc-a","openai.refresh":"oc-r","openai.expires":1700003600000,"openai.type":"oauth","openai.accountId":%q}`, ocAcct)
	originalPi := fmt.Sprintf(`{"openai-codex":{"type":"oauth","access":"pi-a","refresh":"pi-r","expires":1700003600000,"accountId":%q}}`, piAcct)
	seedAuthFile(t, authFile, originalOC)
	seedAuthFile(t, piAuthFile, originalPi)

	// Sanity: the lock must not pre-exist (we'll assert it is never created).
	lockPath := piAuthFile + ".lock"
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Pi lock must not pre-exist, stat err=%v", err)
	}

	// Rotated OC credential — a DIFFERENT account from Pi.
	refreshed := map[string]any{
		"access":    "rotated-oc-access",
		"refresh":   "rotated-oc-refresh",
		"expires":   int64(1700007200000),
		"type":      "oauth",
		"accountId": ocAcct,
	}

	if err := syncRefreshedCredentialToBoth(config{
		AuthFile:   authFile,
		PiAuthFile: piAuthFile,
	}, refreshed, false); err != nil {
		t.Fatalf("different-account refresh must NOT error (Pi is silently skipped): %v", err)
	}

	// OC MUST carry the rotated tuple.
	assertOCAuth(t, authFile, "rotated-oc-access", "rotated-oc-refresh", 1700007200000, ocAcct)
	// Pi MUST remain byte-identical to its pre-call state — the helper must
	// not have touched a DIFFERENT account's session.
	if got := string(mustReadFile(t, piAuthFile)); got != originalPi {
		t.Fatalf("Pi auth file was mutated despite the account-identity guard; got %q", got)
	}
	// The Pi lock MUST never have been created or acquired: a different
	// account's rotated credential is not allowed to coordinate with Pi's
	// lock protocol.
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Pi lock was acquired by the helper for a different-account refresh: stat err=%v", err)
	}
}

// TestUsedPercentAtOrAboveThreshold locks the >= boundary used by rotation.
func TestUsedPercentAtOrAboveThreshold(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		input     string
		threshold float64
		want      bool
	}{
		{name: "below integer", input: "79", threshold: 80, want: false},
		{name: "below decimal", input: "79.99", threshold: 80, want: false},
		{name: "exact integer", input: "80", threshold: 80, want: true},
		{name: "exact decimal", input: "80.00", threshold: 80, want: true},
		{name: "decimal above", input: "80.01", threshold: 80, want: true},
		{name: "integer above", input: "81", threshold: 80, want: true},
		{name: "maximum", input: "100", threshold: 80, want: true},
		{name: "whitespace exact", input: "  80  ", threshold: 80, want: true},
		{name: "whitespace above", input: "\t85\n", threshold: 80, want: true},
		{name: "empty", input: "", threshold: 80, want: false},
		{name: "non-numeric", input: "abc", threshold: 80, want: false},
		{name: "percent suffix", input: "80%", threshold: 80, want: false},
		{name: "malformed decimal", input: "80.0.0", threshold: 80, want: false},
		{name: "custom above", input: "50", threshold: 49.5, want: true},
		{name: "custom below", input: "50", threshold: 50.5, want: false},
		{name: "weekly exact", input: "98", threshold: 98, want: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := usedPercentAtOrAboveThreshold(tc.input, tc.threshold); got != tc.want {
				t.Fatalf("usedPercentAtOrAboveThreshold(%q, %v) = %v, want %v", tc.input, tc.threshold, got, tc.want)
			}
		})
	}
}
