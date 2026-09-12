package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const defaultUsageURL = "https://chatgpt.com/backend-api/wham/usage"

// defaultOAuthRefreshTokenURL is the production OAuth refresh endpoint. It is
// exposed as config.TokenURL so tests can substitute a httptest.Server URL.
const defaultOAuthRefreshTokenURL = "https://auth.openai.com/oauth/token"

// oauthRefreshRequestTimeout caps every refresh POST so a slow endpoint
// cannot block the CLI indefinitely. The caller's context is layered on top
// via context.WithTimeout, so cancellation still propagates.
const oauthRefreshRequestTimeout = 15 * time.Second

// oauthRefreshClientID is the public OpenAI client id used by the OAuth
// refresh flow. Rotating this literal would invalidate every existing
// refresh token, so it is intentionally hard-coded.
const oauthRefreshClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

// chatGPTAccountIDNamespace / chatGPTAccountIDField are the JWT keys Pi uses
// to carry the account identity inside its rotated access tokens (the value
// is a JSON object whose chatgpt_account_id field stores the id).
// chatGPTAccountIDClaim keeps the legacy flat claim
// (https://api.openai.com/auth.chatgpt_account_id) as a fallback so older
// issuers keep working. When any of these resolves to a non-empty string it
// replaces the stored accountId in the rotated tuple; when none parse, the
// prior accountId is preserved.
const (
	chatGPTAccountIDNamespace = "https://api.openai.com/auth"
	chatGPTAccountIDField     = "chatgpt_account_id"
	chatGPTAccountIDClaim     = "https://api.openai.com/auth.chatgpt_account_id"
)

// defaultWeeklyThreshold gates the WEEKLY usage window. It is always applied to
// `secondary_window` when 5h mode is active, and to `primary_window` when 5h
// mode is inactive (or `secondary_window` is missing/null). When the active
// account's weekly usage reaches (or exceeds) this percentage, the default
// command considers it exhausted and attempts to rotate to a saved alternate.
const defaultWeeklyThreshold = 98.0

// defaultFiveHourThreshold gates the 5-HOUR usage window. It is applied to
// `primary_window` only when 5h mode is active AND `secondary_window` is
// present.
const defaultFiveHourThreshold = 80.0

// rotationThresholds bundles the 5h and weekly cutoffs that drive rotation,
// eligibility, and cooldown. The runtime path loads these from the persisted
// config (with defaults applied when missing); helpers like
// `selectEligibleAlternateAccount` and `accountWithUsage` accept the struct
// instead of reading globals so tests can pin exact threshold values.
type rotationThresholds struct {
	FiveHour float64
	Weekly   float64
}

// defaultRotationThresholds returns the runtime defaults for the 5h and weekly
// thresholds. Both values are exposed via the `config <key>` subcommands and
// can be persisted into the config file; this helper is the single source of
// truth for the in-process defaults.
func defaultRotationThresholds() rotationThresholds {
	return rotationThresholds{FiveHour: defaultFiveHourThreshold, Weekly: defaultWeeklyThreshold}
}

type config struct {
	AuthFile          string
	PiAuthFile        string
	AccountsFile      string
	ConfigFile        string
	FiveHourEnabled   bool
	FiveHourThreshold float64
	WeeklyThreshold   float64
	UsageURL          string
	// TokenURL is the OAuth refresh endpoint. The default points at the
	// production OpenAI refresh endpoint; tests override it with a
	// httptest.Server URL to keep credentials off the network.
	TokenURL   string
	HTTPClient *http.Client
	Now        func() time.Time
}

type usageWindow struct {
	UsedPercent          string
	ResetAt              *int64
	SecondaryUsedPercent string
	SecondaryResetAt     *int64
	UserID               string
	Email                string
}

// fiveHourAvailable reports whether both windows are populated. It mirrors the
// "5h mode is active" rule: we need BOTH the toggle to be on AND the API to
// have provided a usable secondary_window.
func (w usageWindow) fiveHourAvailable(toggleEnabled bool) bool {
	return toggleEnabled && strings.TrimSpace(w.SecondaryUsedPercent) != ""
}

// normalizeUsageForToggle applies the 5h toggle to a raw usage window and
// returns the canonical usage every consumer should see plus the effective
// fiveHourMode (matches the README/SKILL contract).
func normalizeUsageForToggle(usage usageWindow, toggleEnabled bool) (usageWindow, bool) {
	if toggleEnabled {
		return usage, usage.fiveHourAvailable(toggleEnabled)
	}

	if strings.TrimSpace(usage.SecondaryUsedPercent) != "" {
		usage.UsedPercent = usage.SecondaryUsedPercent
		usage.ResetAt = usage.SecondaryResetAt
		usage.SecondaryUsedPercent = ""
		usage.SecondaryResetAt = nil
	}

	return usage, false
}

// evaluateActiveUsageExhaustion reports whether the active account's usage
// windows have crossed their rotation thresholds. Shared by the OC and
// Pi-only default-command paths; rotation uses OR semantics. In 5h mode the
// weekly (secondary) window is evaluated first, then the 5h (primary); when
// 5h mode is off, or no secondary window is available, only the weekly
// threshold applies to the primary window.
func evaluateActiveUsageExhaustion(usage usageWindow, fiveHourMode bool, thresholds rotationThresholds) (weeklyExhausted, fiveHourExhausted bool) {
	if fiveHourMode {
		return usedPercentAtOrAboveThreshold(usage.SecondaryUsedPercent, thresholds.Weekly),
			usedPercentAtOrAboveThreshold(usage.UsedPercent, thresholds.FiveHour)
	}
	return usedPercentAtOrAboveThreshold(usage.UsedPercent, thresholds.Weekly), false
}

func main() {
	// Hide a console window this process allocated for itself, so callers that
	// spawn the CLI without CREATE_NO_WINDOW do not flash one. No-op outside
	// Windows and when the console is shared with a terminal; see
	// console_windows.go.
	hideOwnConsoleWindow()

	if err := runWithArgs(context.Background(), defaultConfig(), os.Stdout, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func defaultConfig() config {
	home, err := os.UserHomeDir()
	authFile := filepath.Join(".local", "share", "opencode", "auth.json")
	accountsFile := filepath.Join(".local", "share", "codex-usage-cli", "openai-accounts.json")
	configFile := filepath.Join(".local", "share", "codex-usage-cli", "config.json")
	piAuthFile := filepath.Join(".pi", "agent", "auth.json")
	if err == nil {
		authFile = filepath.Join(home, ".local", "share", "opencode", "auth.json")
		accountsFile = filepath.Join(home, ".local", "share", "codex-usage-cli", "openai-accounts.json")
		configFile = filepath.Join(home, ".local", "share", "codex-usage-cli", "config.json")
		piAuthFile = filepath.Join(home, ".pi", "agent", "auth.json")
	}

	if envAuth := strings.TrimSpace(os.Getenv("OPENCODE_AUTH_FILE")); envAuth != "" {
		authFile = envAuth
	}
	if envAccounts := strings.TrimSpace(os.Getenv("CODEX_USAGE_ACCOUNTS_FILE")); envAccounts != "" {
		accountsFile = envAccounts
	}
	if envConfig := strings.TrimSpace(os.Getenv("CODEX_USAGE_CONFIG_FILE")); envConfig != "" {
		configFile = envConfig
	}
	if envPiDir := strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR")); envPiDir != "" {
		// PI_CODING_AGENT_DIR is a directory override; append "auth.json" to get
		// the canonical Pi auth file. The directory may use a leading "~" to
		// reference the user's home (matching Pi semantics); expand it before
		// joining so PiAuthFile is absolute when HOME is set.
		if expanded, expandErr := expandTildePath(envPiDir); expandErr == nil {
			piAuthFile = filepath.Join(expanded, "auth.json")
		}
	}

	return config{
		AuthFile:     authFile,
		PiAuthFile:   piAuthFile,
		AccountsFile: accountsFile,
		ConfigFile:   configFile,
		UsageURL:     defaultUsageURL,
		TokenURL:     defaultOAuthRefreshTokenURL,
		HTTPClient:   http.DefaultClient,
		Now:          time.Now,
	}
}

func run(ctx context.Context, cfg config, stdout io.Writer) error {
	return runWithArgs(ctx, cfg, stdout, nil)
}

func runWithArgs(ctx context.Context, cfg config, stdout io.Writer, args []string) error {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	if len(args) > 0 {
		command := strings.ToLower(strings.TrimSpace(args[0]))
		// Route `config` before any auth/accounts/usage validation and
		// before loading the toggle from disk: the config subcommand owns
		// the toggle file and must work even when no auth, accounts, or
		// usage URL is wired up (e.g. running `config 5h on` before any
		// account has been registered). The subcommand also reports its
		// own clear error when ConfigFile is empty.
		if command == "config" {
			return runConfigCommand(cfg, stdout, args[1:])
		}
	}

	if cfg.AuthFile == "" {
		return errors.New("auth file path is empty")
	}
	if cfg.UsageURL == "" {
		return errors.New("usage URL is empty")
	}
	if cfg.AccountsFile == "" {
		return errors.New("accounts file path is empty")
	}

	// Resolve the 5h toggle from the config file. loadFiveHourConfig is the
	// single source of truth for the default: a missing path, a missing file,
	// or a missing key all default to ON, matching the documented runtime
	// contract. The `config` subcommand is routed above and does not see this
	// load.
	enabled, err := loadFiveHourConfig(cfg.ConfigFile)
	if err != nil {
		return err
	}
	cfg.FiveHourEnabled = enabled

	// Resolve the rotation thresholds the same way. Missing config / missing
	// keys default to the documented runtime values; a malformed value or an
	// out-of-range stored value is a hard error so the CLI refuses to start
	// rather than silently ignoring a corrupt setting.
	fiveHourThreshold, err := loadFiveHourThresholdConfig(cfg.ConfigFile)
	if err != nil {
		return err
	}
	cfg.FiveHourThreshold = fiveHourThreshold

	weeklyThreshold, err := loadWeeklyThresholdConfig(cfg.ConfigFile)
	if err != nil {
		return err
	}
	cfg.WeeklyThreshold = weeklyThreshold

	if len(args) > 0 {
		command := strings.ToLower(strings.TrimSpace(args[0]))
		switch command {
		case "accounts", "list":
			return runAccountsCommand(ctx, cfg, stdout)
		case "use":
			if len(args) < 2 {
				return errors.New("use command requires an account identifier (index, user_id, or email)")
			}
			return runUseCommand(ctx, cfg, stdout, strings.TrimSpace(args[1]))
		default:
			return fmt.Errorf("unknown command %q (available: accounts, list, use <id>, config <feature> [value])", command)
		}
	}

	return runDefaultCommand(ctx, cfg, stdout)
}

// errNeitherProviderAvailable is returned when both OpenCode and Pi auth
// are unavailable. The wording is part of the CLI contract: users and tests
// assert this exact English phrase.
var errNeitherProviderAvailable = errors.New("OpenCode and Pi Agent are not installed or configured")

// ocInitialFetchError marks the failure of the FIRST OpenCode usage fetch in
// runDefaultCommandOpenCodePath; the dispatcher uses errors.As to fall back
// to Pi-only. Later stage errors return unwrapped so partial-sync states
// stay visible. The wrapper preserves the original fetchUsageWindow text.
type ocInitialFetchError struct{ Err error }

func (e *ocInitialFetchError) Error() string { return e.Err.Error() }
func (e *ocInitialFetchError) Unwrap() error { return e.Err }

func runDefaultCommand(ctx context.Context, cfg config, stdout io.Writer) error {
	// OC is permissive (no refresh/expires/type check); Pi uses strict
	// parsePiOpenAICodexEntry under the proper-lockfile protocol. Fallback
	// fires only when the FIRST OC usage fetch fails AND Pi is usable.
	ocAccount, ocAvailable := loadOpenCodeAccountIfAvailable(cfg.AuthFile)
	piInfo, piAvailable := loadPiAuthIfUsable(cfg.PiAuthFile)

	if !ocAvailable && !piAvailable {
		return errNeitherProviderAvailable
	}
	if !ocAvailable {
		return runDefaultCommandPiOnlyPath(ctx, cfg, stdout, piInfo)
	}

	err := runDefaultCommandOpenCodePath(ctx, cfg, stdout, ocAccount, piInfo, piAvailable)
	if err == nil {
		return nil
	}
	var initialErr *ocInitialFetchError
	if !piAvailable || !errors.As(err, &initialErr) {
		return err
	}
	if piErr := runDefaultCommandPiOnlyPath(ctx, cfg, stdout, piInfo); piErr != nil {
		// Join both diagnostics so the caller sees both reasons rather than
		// the misleading "not configured" error.
		return fmt.Errorf("OpenCode usage fetch failed (%v); Pi usage fetch also failed (%v)", initialErr.Err, piErr)
	}
	return nil
}

// runDefaultCommandOpenCodePath preserves the existing OpenCode pipeline,
// including the best-effort inbound Pi reconciliation. The first
// fetchUsageWindow is the ONLY fallback-eligible stage (wrapped in
// ocInitialFetchError); later stages return errors unwrapped so partial-
// sync states stay visible.
//
// Refresh: OAuth rotation is REACTIVE, not proactive. The current
// OpenCode credential is queried first; an HTTP 401 from the usage
// endpoint triggers a single OAuth refresh, the rotated credential is
// propagated to the installed auth stores (syncRefreshedCredentialToBoth
// preserves the existing Pi lock preflight + OC rollback semantics), and
// the usage call is retried exactly once with the new token. The helper
// never inspects `expires`, `cfg.Now`, or any time-based threshold; a
// 401 with missing/empty refresh metadata returns a value-free error and
// leaves auth/store bytes unchanged. Non-401 responses (400, 403, 429,
// 500, ...) bypass the token endpoint entirely.
//
// Guarded Pi-source recovery: BEFORE the single refresh POST, when Pi is
// installed the helper consults Pi's openai-codex tuple and swaps the
// refresh source to Pi's OAuth fields only when Pi is demonstrably the
// same account AND newer/safer (same non-empty access OR same non-empty
// accountId, AND Pi expires > OC expires OR identical access with a
// different refresh). Pi credentials from a different or unknown account
// are NEVER substituted; when the swap does not apply, OC's refresh
// token is reused verbatim. Only OAuth fields are projected into a copy
// of the OC account so user_id/email/custom metadata are preserved.
//
// When Pi is installed and shares the OpenCode pre-refresh access, the
// rotated tuple is mirrored into the in-memory piInfo so the later
// reconcilePiInboundFromPayload call observes the rotated access rather
// than the stale snapshot.
func runDefaultCommandOpenCodePath(ctx context.Context, cfg config, stdout io.Writer, account map[string]any, piInfo *piAuthInfo, piAvailable bool) error {
	token, _ := account["access"].(string)

	usage, rotatedAccount, err := fetchUsageWithReactiveRefresh(ctx, cfg, account, token, func(refreshed map[string]any) error {
		// syncRefreshedCredentialToBoth preserves the documented Pi lock
		// preflight and post-mutation OC rollback; a pre-mutation Pi lock
		// contention leaves both auth files untouched and a post-mutation Pi
		// failure restores OC byte-for-byte from the pre-mutation snapshot.
		return syncRefreshedCredentialToBoth(cfg, refreshed, false)
	}, piInfo)
	if err != nil {
		return &ocInitialFetchError{Err: err}
	}
	if rotatedAccount != nil {
		account = rotatedAccount
		// Mirror the rotated tuple into the in-memory piInfo so the later
		// reconcilePiInboundFromPayload call sees the rotated access rather
		// than the stale pre-refresh snapshot; otherwise the inbound pass
		// would still observe Pi's pre-refresh token and could re-import the
		// stale value. The mirror only touches OAuth fields and never invents
		// identity/usage metadata.
		overwritePiAuthInfoOAuth(piInfo, account)
	}

	usage, fiveHourMode := normalizeUsageForToggle(usage, cfg.FiveHourEnabled)

	// Best-effort inbound Pi reconciliation. ONLY runDefaultCommand does
	// this import; clean no-ops return nil; partial-sync write failures
	// surface as errors. Skipped when Pi was determined unavailable.
	if piAvailable {
		account, usage, _, err = reconcilePiInboundFromPayload(ctx, cfg, piInfo, account, usage, cfg.FiveHourEnabled)
		if err != nil {
			return err
		}
	}
	// Recompute after the inbound swap (idempotent on an already-normalized usage).
	usage, fiveHourMode = normalizeUsageForToggle(usage, cfg.FiveHourEnabled)

	if err := ensureCurrentAccountRegistered(cfg.AccountsFile, account, usage, fiveHourMode, rotationThresholds{FiveHour: cfg.FiveHourThreshold, Weekly: cfg.WeeklyThreshold}); err != nil {
		return err
	}

	thresholds := rotationThresholds{FiveHour: cfg.FiveHourThreshold, Weekly: cfg.WeeklyThreshold}
	weeklyExhausted, fiveHourExhausted := evaluateActiveUsageExhaustion(usage, fiveHourMode, thresholds)
	if weeklyExhausted || fiveHourExhausted {
		store, err := readAccountsStore(cfg.AccountsFile)
		if err != nil {
			return err
		}

		if nextAccount, ok := selectEligibleAlternateAccount(store, usage.UserID, cfg.Now().Unix(), fiveHourMode, cfg.FiveHourThreshold, cfg.WeeklyThreshold); ok {
			if err := activateAccount(cfg, nextAccount); err != nil {
				return err
			}
		}
	}

	_, err = io.WriteString(stdout, usage.UsedPercent)
	return err
}

// runDefaultCommandPiOnlyPath runs the default command when only Pi is
// available. MUST NOT touch OpenCode auth.json; the persisted shape matches
// the OC path.
//
// Refresh: OAuth rotation is REACTIVE. The first usage call is attempted
// with the stored Pi access token; an HTTP 401 triggers ONE OAuth refresh,
// the rotated credential is persisted via syncAccountToPiAuth (which keeps
// the Pi lock preflight + rollback semantics), and the usage call is
// retried exactly once. A 401 with missing/empty refresh metadata, a
// refresh POST failure, or a second 401 surfaces an error and leaves the
// Pi auth file unchanged. Non-401 responses bypass the token endpoint
// entirely.
func runDefaultCommandPiOnlyPath(ctx context.Context, cfg config, stdout io.Writer, piInfo *piAuthInfo) error {
	piAccount, piParseErr := parsePiOpenAICodexEntry(piInfo.CredMap)
	if piParseErr != nil {
		return piParseErr
	}

	piToken, _ := piAccount["access"].(string)
	if strings.TrimSpace(piToken) == "" {
		return errors.New("Pi openai-codex credential is missing a non-empty access token")
	}

	usage, rotatedAccount, err := fetchUsageWithReactiveRefresh(ctx, cfg, piAccount, piToken, func(refreshed map[string]any) error {
		return syncAccountToPiAuth(cfg, refreshed)
	}, piInfo)
	if err != nil {
		return err
	}
	if rotatedAccount != nil {
		piAccount = rotatedAccount
	}

	usage, fiveHourMode := normalizeUsageForToggle(usage, cfg.FiveHourEnabled)
	persisted := accountWithUsage(piAccount, usage, fiveHourMode, rotationThresholds{FiveHour: cfg.FiveHourThreshold, Weekly: cfg.WeeklyThreshold})

	// Persist ONLY into the store. OpenCode's auth.json is intentionally not
	// touched here: Pi-only mode must never create or rewrite it.
	if err := persistOpenAIAccount(cfg.AccountsFile, persisted); err != nil {
		return err
	}

	// Pi-only rotation: activateAccount recognizes the Pi-only install
	// (openCodeEnabled requires the file to exist on disk) and only writes
	// the Pi credential, so OpenCode auth.json is never created.
	thresholds := rotationThresholds{FiveHour: cfg.FiveHourThreshold, Weekly: cfg.WeeklyThreshold}
	weeklyExhausted, fiveHourExhausted := evaluateActiveUsageExhaustion(usage, fiveHourMode, thresholds)
	if weeklyExhausted || fiveHourExhausted {
		store, err := readAccountsStore(cfg.AccountsFile)
		if err != nil {
			return err
		}
		if nextAccount, ok := selectEligibleAlternateAccount(store, usage.UserID, cfg.Now().Unix(), fiveHourMode, cfg.FiveHourThreshold, cfg.WeeklyThreshold); ok {
			if err := activateAccount(cfg, nextAccount); err != nil {
				return err
			}
		}
	}

	_, err = io.WriteString(stdout, usage.UsedPercent)
	return err
}

// loadOpenCodeAccountIfAvailable returns the parsed OpenCode account when the
// auth file exists and yields a non-empty openai.access token. The check is
// intentionally permissive (no refresh/expires/type enforcement) so legacy
// auth files keep satisfying it; missing files, parse errors, and missing or
// empty access tokens count as unavailable.
func loadOpenCodeAccountIfAvailable(authFile string) (map[string]any, bool) {
	if strings.TrimSpace(authFile) == "" {
		return nil, false
	}
	account, err := readOpenAIAccount(authFile)
	if err != nil {
		return nil, false
	}
	token, _ := account["access"].(string)
	if strings.TrimSpace(token) == "" {
		return nil, false
	}
	return account, true
}

// piAuthInfo carries the already-validated Pi auth.json payload and the raw
// openai-codex credential map so callers reuse them without a second read.
type piAuthInfo struct {
	Payload map[string]any
	CredMap map[string]any
}

// loadPiAuthIfUsable reads Pi's auth.json under the proper-lockfile protocol
// and reports availability for routing. Missing files, lock failures,
// malformed JSON, missing openai-codex entries, or invalid OAuth entries
// all count as unavailable.
func loadPiAuthIfUsable(piAuthFile string) (*piAuthInfo, bool) {
	if strings.TrimSpace(piAuthFile) == "" {
		return nil, false
	}
	payload, err := readPiAuthUnderLock(piAuthFile)
	if err != nil {
		return nil, false
	}
	rawCred, present := payload["openai-codex"]
	if !present || rawCred == nil {
		return nil, false
	}
	credMap, ok := rawCred.(map[string]any)
	if !ok {
		return nil, false
	}
	if _, err := parsePiOpenAICodexEntry(credMap); err != nil {
		return nil, false
	}
	return &piAuthInfo{Payload: payload, CredMap: credMap}, true
}

type accountUsageRow struct {
	Current               bool
	Index                 int
	UserID                string
	Email                 string
	UsedPercent           string
	SecondaryUsedPercent  string
	ResetDisplay          string
	SecondaryResetDisplay string
	// IsDualRow is the canonical marker that this row carries secondary_window
	// data. Under dual mode it renders the row in the 4-column layout; under
	// weekly mode (or mixed dual/fallback) a row with IsDualRow=false is shown
	// in the WEEK% column instead of the USED% column so a fallback weekly row
	// is never mislabeled as a 5h value.
	IsDualRow bool
	Err       error
}

// currentAccountToken resolves the access token of the account currently in
// use, so `accounts`/`list` can mark it with `*`. OpenCode wins whenever its
// auth file is usable; only when it is absent or unusable (a Pi-only install,
// e.g. a VPS) does Pi's `openai-codex` credential decide.
//
// READ-ONLY: loadPiAuthIfUsable reads Pi's auth.json under the same
// proper-lockfile protocol as the default command and never creates or writes
// anything, so listing accounts stays free of side effects. It never calls
// reconcilePiInbound, which remains reserved for runDefaultCommand.
//
// The match key is exact access-token equality against the saved store — the
// same key `accounts` already used for OpenCode. It costs no extra usage
// request. The tradeoff is that a Pi credential refreshed out of band since
// the last store sync no longer matches, in which case no row is marked;
// running the default command re-syncs the store and restores the marker.
// Returns "" when neither provider yields a usable token.
func currentAccountToken(cfg config) string {
	if account, ok := loadOpenCodeAccountIfAvailable(cfg.AuthFile); ok {
		if token, _ := account["access"].(string); strings.TrimSpace(token) != "" {
			return token
		}
	}

	piInfo, piAvailable := loadPiAuthIfUsable(cfg.PiAuthFile)
	if !piAvailable {
		return ""
	}
	// loadPiAuthIfUsable already validated this entry, so the parse cannot
	// fail here; the error is still handled rather than discarded.
	piAccount, err := parsePiOpenAICodexEntry(piInfo.CredMap)
	if err != nil {
		return ""
	}
	token, _ := piAccount["access"].(string)
	return token
}

func runAccountsCommand(ctx context.Context, cfg config, stdout io.Writer) error {
	// OpenCode may not be installed on this machine. The saved store is
	// listed in full either way, so `accounts` stays useful on a Pi-only
	// (or store-only) install; currentAccountToken decides whether any row
	// can still be marked with `*`.
	//
	// piInfo is loaded once for the guarded CURRENT-row source selection
	// (selectRefreshSourceFromPi). On a Pi-only or store-only install the
	// load yields (nil, false) and the CURRENT row falls back to its own
	// stored refresh, matching the documented non-CURRENT behavior.
	piInfo, piAvailable := loadPiAuthIfUsable(cfg.PiAuthFile)
	currentToken := currentAccountToken(cfg)

	store, err := readAccountsStore(cfg.AccountsFile)
	if err != nil {
		return err
	}
	store = normalizeAccountsStoreByUserID(store)

	if len(store) == 0 {
		_, err := fmt.Fprintf(stdout, "No saved accounts found in %s\n", cfg.AccountsFile)
		return err
	}

	keys := make([]string, 0, len(store))
	for userID := range store {
		keys = append(keys, userID)
	}
	sort.Strings(keys)

	rows := make([]accountUsageRow, 0, len(keys))
	successCount := 0
	currentUserID := ""
	// currentCoherent tracks whether the CURRENT row's auth-synchronization
	// step (refresh + sync to installed auth stores) completed cleanly.
	// A failure here must not be hidden as "overall success" because the
	// rotated credential is now in memory but not necessarily on disk; we
	// skip the successCount++ for that row and surface the failure as an
	// explicit error after the table is rendered.
	currentCoherent := true
	for idx, userID := range keys {
		account := copyAccountData(store[userID])
		access, _ := account["access"].(string)
		current := strings.TrimSpace(currentToken) != "" && access == currentToken

		storedUserID, _ := account["user_id"].(string)
		if strings.TrimSpace(storedUserID) == "" {
			storedUserID = userID
		}

		row := accountUsageRow{
			Current: current,
			Index:   idx + 1,
			UserID:  storedUserID,
		}

		if strings.TrimSpace(access) == "" {
			row.Email = accountEmailFallback(account, row.UserID)
			row.UsedPercent = "ERR"
			row.ResetDisplay = "-"
			// Leave secondary fields EMPTY for failed rows. A sentinel "-"
			// here would falsely flip printAccountsTable into dual mode if
			// it is the only data-bearing column the renderer can inspect;
			// the renderer fills "-" for the missing secondary columns when
			// another successful row is dual.
			row.Err = errors.New("missing access token")
			rows = append(rows, row)
			continue
		}

		// Reactive OAuth refresh: the first usage call attempts with the
		// stored access token. An HTTP 401 triggers ONE OAuth refresh; on a
		// successful refresh of the CURRENT row the rotated credential is
		// mirrored to the installed auth stores via syncRefreshedCredentialToBoth
		// (Pi lock preflight + post-mutation OC rollback preserved). The usage
		// call is then retried exactly once with the new token. A second 401,
		// a refresh POST failure, a 401 with missing refresh metadata, or any
		// sync failure leaves auth/store bytes unchanged and renders the row as
		// ERR. Row-local failures stay row-local so other rows are still
		// rendered; only when the CURRENT row's auth sync fails do we flag the
		// entire command as incoherent so successCount is not inflated and the
		// caller sees a clear error.
		//
		// Non-401 responses (400, 403, 429, 500, ...) bypass the token endpoint
		// entirely and surface their existing errors. CURRENT was computed
		// against the pre-refresh token above, so a reactive rotation on the
		// current row does not silently un-mark it: the rotation only ever
		// runs when the pre-refresh token was rejected by the usage endpoint.
		//
		// Guarded Pi-source recovery for the CURRENT row: when OpenCode's
		// current token matches the row (the `current` flag above) and Pi
		// holds a demonstrably same-account newer tuple, the reactive helper
		// consults Pi before the single refresh POST and swaps the refresh
		// source to Pi's tuple when it is demonstrably newer/safer. The swap
		// only triggers when the usage endpoint returned 401; non-CURRENT
		// rows continue using their own stored refresh so they stay isolated
		// from any out-of-band Pi rotation that is unrelated to the active
		// session. We pass piInfo only for the CURRENT row; other rows pass
		// nil so the helper cannot pick up a Pi tuple unrelated to them.
		var rowPiInfo *piAuthInfo
		if current && piAvailable {
			rowPiInfo = piInfo
		}
		rowSyncFailed := false
		var persistFn func(map[string]any) error
		if current {
			persistFn = func(refreshed map[string]any) error {
				return syncRefreshedCredentialToBoth(cfg, refreshed, false)
			}
		}
		usage, rotatedAccount, fetchErr := fetchUsageWithReactiveRefresh(ctx, cfg, account, access, persistFn, rowPiInfo)
		if fetchErr != nil {
			row.Email = accountEmailFallback(account, row.UserID)
			row.UsedPercent = "ERR"
			row.ResetDisplay = "-"
			// See comment above: error rows must not determine table mode.
			row.Err = fetchErr
			// A reactive sync failure on the CURRENT row (a 401 followed by a
			// successful refresh POST whose auth-store sync failed) must mark
			// the whole command as incoherent so the caller never sees a
			// silent "success" while the current credential diverges from the
			// installed stores. Other failure modes (refresh POST error,
			// missing refresh metadata, second 401) stay row-local.
			var syncErr *refreshSyncError
			if current && errors.As(fetchErr, &syncErr) {
				rowSyncFailed = true
			}
			if rowSyncFailed {
				currentCoherent = false
			}
			rows = append(rows, row)
			continue
		}
		if rowSyncFailed {
			currentCoherent = false
		}
		if rotatedAccount != nil {
			account = rotatedAccount
			access, _ = account["access"].(string)
		}

		// Apply the 5h toggle once: with 5h off and a dual response this
		// promotes the weekly secondary into the primary fields and clears
		// the secondary fields so display and persistence see only weekly.
		usage, fiveHourMode := normalizeUsageForToggle(usage, cfg.FiveHourEnabled)

		if usage.UserID != "" {
			row.UserID = usage.UserID
		}
		row.Email = usageEmailOrFallback(usage.Email, row.UserID)
		row.UsedPercent = usage.UsedPercent
		row.ResetDisplay = formatResetDisplay(usage.ResetAt, cfg.Now())
		if fiveHourMode {
			row.SecondaryUsedPercent = usage.SecondaryUsedPercent
			row.SecondaryResetDisplay = formatResetDisplay(usage.SecondaryResetAt, cfg.Now())
			// IsDualRow marks rows that actually carry secondary_window data so
			// the renderer can keep fallback weekly rows in the WEEK% column
			// when another row in the same table is dual.
			row.IsDualRow = strings.TrimSpace(usage.SecondaryUsedPercent) != ""
		}
		// else: leave secondary fields empty so the row stays weekly-only
		// and never contributes to the dual-mode decision in the renderer.
		if current {
			currentUserID = row.UserID
		}

		// A failed current-row auth sync is NOT counted as a successful
		// persisted refresh: the rotated credential may not be on disk in
		// the installed auth stores, so claiming a coherent persistence
		// would be misleading. The row is still rendered (row.Err already
		// names the failing sync step) so the user can see exactly what
		// happened; we just skip both the persist and the successCount.
		if !rowSyncFailed {
			updated := accountWithUsage(account, usage, fiveHourMode, rotationThresholds{FiveHour: cfg.FiveHourThreshold, Weekly: cfg.WeeklyThreshold})
			if persistErr := persistOpenAIAccount(cfg.AccountsFile, updated); persistErr != nil {
				row.Err = persistErr
			} else {
				successCount++
			}
		}

		rows = append(rows, row)
	}

	if strings.TrimSpace(currentUserID) != "" {
		for i := range rows {
			rows[i].Current = rows[i].Current || rows[i].UserID == currentUserID
		}
	}

	// Rows are already in alphabetical user_id order; indices are stable for
	// callers that want to reference them from `use <id>`.
	if err := printAccountsTable(stdout, rows); err != nil {
		return err
	}

	// Current-account auth-synchronization failure takes priority over the
	// generic "all rows failed" diagnostic: it is more specific and actionable
	// for the caller, and it fires regardless of successCount so the user
	// never sees "success" while the current credential is incoherent across
	// the installed auth stores.
	if !currentCoherent {
		return errors.New("current account auth synchronization failed; one or more installed auth stores did not converge with the refreshed credential")
	}

	if successCount == 0 {
		return errors.New("failed to fetch usage for all saved accounts")
	}

	return nil
}

func printAccountsTable(stdout io.Writer, rows []accountUsageRow) error {
	// Table mode is data-driven: if any successful row carries secondary_window
	// data (i.e. 5h mode is active and the API provided both windows), render
	// the full dual table with both USED%/WEEK% and RESET/WEEK-RESET pairs.
	// Otherwise render the weekly-only table.
	//
	// IsDualRow is the canonical marker, but SecondaryUsedPercent is also
	// accepted so tests can construct rows directly without having to set the
	// marker explicitly. Error rows never carry secondary fields (they leave
	// IsDualRow false and SecondaryUsedPercent empty), so a table of error
	// rows never flips the renderer into dual mode.
	fiveHourMode := false
	for _, row := range rows {
		if row.IsDualRow || strings.TrimSpace(row.SecondaryUsedPercent) != "" {
			fiveHourMode = true
			break
		}
	}

	var headers []string
	if fiveHourMode {
		headers = []string{"ID", "CURRENT", "EMAIL", "USED%", "WEEK%", "RESET", "WEEK-RESET"}
	} else {
		// The endpoint now reports `rate_limit.primary_window` as the WEEKLY
		// usage window; `secondary_window` is null and unused. The single
		// pair of columns is therefore labeled `WEEK%` / `WEEK-RESET` to
		// keep the table unambiguous for humans.
		headers = []string{"ID", "CURRENT", "EMAIL", "WEEK%", "WEEK-RESET"}
	}

	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}

	prepared := make([][]string, len(rows))
	for i, row := range rows {
		marker := ""
		if row.Current {
			marker = "*"
		}

		isDual := row.IsDualRow || strings.TrimSpace(row.SecondaryUsedPercent) != ""
		isError := row.Err != nil

		var cells []string
		if fiveHourMode {
			if isError {
				// Error rows in dual mode: keep "ERR" in the primary (USED%)
				// column and "-" everywhere else, so the reader immediately
				// sees the failure in the same place a successful 5h value
				// would have been shown.
				cells = []string{
					strconv.Itoa(row.Index),
					marker,
					row.Email,
					"ERR",
					"-",
					"-",
					"-",
				}
			} else if isDual {
				cells = []string{
					strconv.Itoa(row.Index),
					marker,
					row.Email,
					row.UsedPercent,
					row.SecondaryUsedPercent,
					row.ResetDisplay,
					row.SecondaryResetDisplay,
				}
			} else {
				// Fallback weekly row in a dual table: keep the weekly value
				// in WEEK%/WEEK-RESET, and show "-" under USED%/RESET so the
				// row is never mislabeled as a 5h value.
				cells = []string{
					strconv.Itoa(row.Index),
					marker,
					row.Email,
					"-",
					row.UsedPercent,
					"-",
					row.ResetDisplay,
				}
			}
		} else {
			if isError {
				cells = []string{
					strconv.Itoa(row.Index),
					marker,
					row.Email,
					"ERR",
					"-",
				}
			} else {
				cells = []string{
					strconv.Itoa(row.Index),
					marker,
					row.Email,
					row.UsedPercent,
					row.ResetDisplay,
				}
			}
		}
		for c, value := range cells {
			if n := len(value); n > widths[c] {
				widths[c] = n
			}
		}
		prepared[i] = cells
	}

	writeSeparator := func() error {
		parts := make([]string, len(widths))
		for i, w := range widths {
			parts[i] = strings.Repeat("-", w)
		}
		_, err := fmt.Fprintf(stdout, "| %s |\n", strings.Join(parts, " | "))
		return err
	}

	writeRow := func(cells []string) error {
		parts := make([]string, len(widths))
		for i, w := range widths {
			parts[i] = fmt.Sprintf("%-*s", w, cells[i])
		}
		_, err := fmt.Fprintf(stdout, "| %s |", strings.Join(parts, " | "))
		return err
	}

	if err := writeRow(headers); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(stdout); err != nil {
		return err
	}
	if err := writeSeparator(); err != nil {
		return err
	}
	for i, cells := range prepared {
		if err := writeRow(cells); err != nil {
			return err
		}
		if rowErr := rows[i].Err; rowErr != nil {
			if _, err := fmt.Fprintf(stdout, "  [ERR: %s]", rowErr.Error()); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(stdout); err != nil {
			return err
		}
	}

	return nil
}

func runUseCommand(ctx context.Context, cfg config, stdout io.Writer, identifier string) error {
	// `use` does NOT query the usage endpoint, so OAuth refresh is out of
	// scope: the reactive 401-driven refresh flow lives in the default and
	// `accounts`/`list` commands only. Activation propagates the stored
	// OAuth tuple to OpenCode + Pi auth.json via activateAccount (which
	// preserves its documented validate-first / partial-sync-error contract).
	if strings.TrimSpace(identifier) == "" {
		return errors.New("use command requires an account identifier (index, user_id, or email)")
	}

	store, err := readAccountsStore(cfg.AccountsFile)
	if err != nil {
		return err
	}
	store = normalizeAccountsStoreByUserID(store)

	if len(store) == 0 {
		return errors.New("no saved accounts found; run the CLI once to register the active account")
	}

	keys := sortedUserIDs(store)

	target, targetUserID, err := resolveAccountIdentifier(store, keys, identifier)
	if err != nil {
		return err
	}

	if err := activateAccount(cfg, target); err != nil {
		return err
	}

	email := accountEmailFallback(target, targetUserID)
	_, err = fmt.Fprintf(stdout, "Switched active account to %s (%s)\n", email, targetUserID)
	return err
}

func sortedUserIDs(store map[string]map[string]any) []string {
	keys := make([]string, 0, len(store))
	for userID := range store {
		keys = append(keys, userID)
	}
	sort.Strings(keys)
	return keys
}

func resolveAccountIdentifier(store map[string]map[string]any, sortedKeys []string, identifier string) (map[string]any, string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return nil, "", errors.New("identifier is empty")
	}

	// `#<n>` opts into row-index resolution; this prefix cannot collide with
	// an OpenAI `user_id`, so a bare numeric identifier is always treated as
	// an exact user_id match (preventing `use 2` from selecting user_id "10"
	// when the store contains user_ids like "1", "2", "10").
	if strings.HasPrefix(identifier, "#") {
		idx, err := strconv.Atoi(strings.TrimPrefix(identifier, "#"))
		if err != nil || idx < 1 || idx > len(sortedKeys) {
			return nil, "", fmt.Errorf("row index %q out of range (1..%d)", identifier, len(sortedKeys))
		}
		userID := sortedKeys[idx-1]
		return copyAccountData(store[userID]), userID, nil
	}

	for _, userID := range sortedKeys {
		if userID == identifier {
			return copyAccountData(store[userID]), userID, nil
		}
	}

	normalized := strings.ToLower(identifier)
	for _, userID := range sortedKeys {
		account := store[userID]
		email, _ := account["email"].(string)
		if strings.ToLower(strings.TrimSpace(email)) == normalized {
			return copyAccountData(account), userID, nil
		}
	}

	return nil, "", fmt.Errorf("no saved account matched identifier %q (use exact user_id, email, or #<row-index> from `accounts`/`list`)", identifier)
}

func formatResetDisplay(resetAt *int64, now time.Time) string {
	if resetAt == nil {
		return "-"
	}

	remaining := time.Unix(*resetAt, 0).Sub(now)
	if remaining <= 0 {
		return "expired"
	}

	seconds := int64(remaining / time.Second)
	if seconds < 60 {
		return "now"
	}

	days := seconds / (24 * 60 * 60)
	hours := (seconds % (24 * 60 * 60)) / (60 * 60)
	minutes := (seconds % (60 * 60)) / 60

	if days > 0 {
		if hours > 0 {
			return fmt.Sprintf("%dd %dh", days, hours)
		}
		return fmt.Sprintf("%dd", days)
	}

	if hours > 0 {
		if minutes > 0 {
			return fmt.Sprintf("%dh %dm", hours, minutes)
		}
		return fmt.Sprintf("%dh", hours)
	}

	return fmt.Sprintf("%dm", minutes)
}

func usageEmailOrFallback(email, userID string) string {
	if strings.TrimSpace(email) != "" {
		return email
	}
	if strings.TrimSpace(userID) != "" {
		return userID
	}
	return "(sin email)"
}

func accountEmailFallback(account map[string]any, userID string) string {
	if storedEmail, _ := account["email"].(string); strings.TrimSpace(storedEmail) != "" {
		return storedEmail
	}
	if strings.TrimSpace(userID) != "" {
		return userID
	}
	return "(sin email)"
}

// loadFiveHourConfig reads the dedicated 5h toggle from the config file. The
// toggle is enabled by default (missing file or missing key) and stored as a
// string ("on"/"off"); boolean values are accepted for convenience. Any other
// value or malformed JSON returns an error so the CLI can refuse to start
// rather than silently defaulting.
func loadFiveHourConfig(path string) (bool, error) {
	if strings.TrimSpace(path) == "" {
		return true, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, fmt.Errorf("reading config file: %w", err)
	}

	if len(bytes.TrimSpace(data)) == 0 {
		return true, nil
	}

	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return false, fmt.Errorf("parsing config file JSON: %w", err)
	}

	raw, ok := payload["5h"]
	if !ok {
		return true, nil
	}

	switch v := raw.(type) {
	case bool:
		return v, nil
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "on", "true":
			return true, nil
		case "off", "false":
			return false, nil
		default:
			return false, fmt.Errorf("invalid 5h value %q (expected: on, off, true, or false)", v)
		}
	default:
		return false, fmt.Errorf("invalid 5h value of type %T (expected boolean or \"on\"/\"off\" string)", raw)
	}
}

// saveFiveHourConfig writes the 5h toggle to the config file with restrictive
// permissions (parent dir 0700, file 0600). Unknown keys already present in
// the file are preserved so users can keep their own settings.
func saveFiveHourConfig(path string, enabled bool) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("config file path is empty")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	payload := make(map[string]any)
	if data, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &payload); err != nil {
			return fmt.Errorf("parsing existing config file: %w", err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading existing config file: %w", err)
	}

	if enabled {
		payload["5h"] = "on"
	} else {
		payload["5h"] = "off"
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding config file: %w", err)
	}

	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return fmt.Errorf("writing config file: %w", err)
	}

	// Tighten permissions in case the file already existed with looser modes.
	// os.WriteFile only applies 0o600 on initial creation; if the file was
	// pre-existing with 0o644 (or worse), the truncate-and-write path leaves
	// the mode untouched. chmod is a no-op on Windows so the loop just keeps
	// the file restricted on every other platform.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("tightening config file permissions: %w", err)
		}
	}

	return nil
}

// runConfigCommand handles the `config` subcommand. It never touches the auth
// or accounts files, so it can be invoked without an active OpenCode account.
func runConfigCommand(cfg config, stdout io.Writer, args []string) error {
	if len(args) < 1 {
		return errors.New("config command requires a feature name (currently supported: 5h, 5h-threshold, weekly-threshold)")
	}

	feature := strings.ToLower(strings.TrimSpace(args[0]))
	switch feature {
	case "5h":
		return runConfigFiveHour(cfg, stdout, args[1:])
	case "5h-threshold":
		return runConfigThreshold(cfg, stdout, args[1:], loadFiveHourThresholdConfig, saveFiveHourThresholdConfig, "5h_threshold")
	case "weekly-threshold":
		return runConfigThreshold(cfg, stdout, args[1:], loadWeeklyThresholdConfig, saveWeeklyThresholdConfig, "weekly_threshold")
	default:
		return fmt.Errorf("unknown config feature %q (currently supported: 5h, 5h-threshold, weekly-threshold)", feature)
	}
}

func runConfigFiveHour(cfg config, stdout io.Writer, args []string) error {
	if len(args) == 0 {
		// Report effective state.
		enabled, err := loadFiveHourConfig(cfg.ConfigFile)
		if err != nil {
			return err
		}
		return writeConfigState(stdout, enabled)
	}

	if len(args) != 1 {
		return errors.New("config 5h takes at most one value (on or off)")
	}

	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case "on":
		if err := saveFiveHourConfig(cfg.ConfigFile, true); err != nil {
			return err
		}
		return writeConfigState(stdout, true)
	case "off":
		if err := saveFiveHourConfig(cfg.ConfigFile, false); err != nil {
			return err
		}
		return writeConfigState(stdout, false)
	default:
		return fmt.Errorf("invalid 5h value %q (expected: on or off)", args[0])
	}
}

func writeConfigState(stdout io.Writer, enabled bool) error {
	state := "off"
	if enabled {
		state = "on"
	}
	_, err := io.WriteString(stdout, state+"\n")
	return err
}

// runConfigThreshold handles `config <key> [value]` for the two threshold
// subcommands. Without a value it prints the effective threshold as a plain
// parseable number (so scripts can capture it directly); with a value it
// validates and persists the threshold, refusing to touch the file on a
// validation failure. The behavior is identical for both keys; the loader,
// writer, and on-disk field name are passed in so we can share the contract
// without duplicating parsing/save logic.
func runConfigThreshold(
	cfg config,
	stdout io.Writer,
	args []string,
	load func(path string) (float64, error),
	save func(path string, value float64) error,
	fieldName string,
) error {
	if len(args) > 1 {
		return fmt.Errorf("config %s takes at most one value (a number in (0, 100])", strings.TrimSuffix(fieldName, "_threshold"))
	}

	if len(args) == 0 {
		value, err := load(cfg.ConfigFile)
		if err != nil {
			return err
		}
		return writeConfigThreshold(stdout, value)
	}

	parsed, err := validateThresholdPercent(args[0])
	if err != nil {
		return fmt.Errorf("invalid %s value %q: %w", fieldName, args[0], err)
	}

	if err := save(cfg.ConfigFile, parsed); err != nil {
		return err
	}
	return writeConfigThreshold(stdout, parsed)
}

// writeConfigThreshold prints the threshold as a plain parseable number with
// no surrounding markup so scripts can read the value directly. strconv 'g'
// picks the shortest representation that round-trips exactly.
func writeConfigThreshold(stdout io.Writer, value float64) error {
	_, err := io.WriteString(stdout, strconv.FormatFloat(value, 'g', -1, 64)+"\n")
	return err
}

// validateThresholdPercent parses raw as a float64 and rejects anything that
// is not a finite number inside the half-open interval (0, 100]. 100 is the
// inclusive upper bound (a threshold of 100 means "rotate when fully used");
// 0 and negative values are rejected because they would mark an account as
// exhausted at zero usage. NaN and ±Inf are rejected to keep the persisted
// value JSON-safe. Errors do not echo the offending value because it has
// already been quoted in the caller; the caller wraps the value into the
// final user-facing message.
func validateThresholdPercent(raw string) (float64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, errors.New("value is empty")
	}

	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, fmt.Errorf("not a finite number: %w", err)
	}
	if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, errors.New("value must be finite")
	}
	if parsed <= 0 {
		return 0, errors.New("value must be greater than 0")
	}
	if parsed > 100 {
		return 0, errors.New("value must be at most 100")
	}
	return parsed, nil
}

// loadFiveHourThresholdConfig reads the 5h rotation threshold from the
// config file. Missing path, missing file, or missing key default to the
// documented runtime value (80). An explicit value outside (0, 100] or any
// malformed JSON returns an error so the CLI can refuse to start rather than
// silently defaulting.
func loadFiveHourThresholdConfig(path string) (float64, error) {
	value, err := loadThresholdFromConfig(path, "5h_threshold", defaultFiveHourThreshold)
	if err != nil {
		return 0, err
	}
	if err := validateThresholdValue(value); err != nil {
		return 0, err
	}
	return value, nil
}

// loadWeeklyThresholdConfig reads the weekly rotation threshold from the
// config file. Same contract as loadFiveHourThresholdConfig with the
// documented runtime default (98).
func loadWeeklyThresholdConfig(path string) (float64, error) {
	value, err := loadThresholdFromConfig(path, "weekly_threshold", defaultWeeklyThreshold)
	if err != nil {
		return 0, err
	}
	if err := validateThresholdValue(value); err != nil {
		return 0, err
	}
	return value, nil
}

// saveFiveHourThresholdConfig persists the 5h rotation threshold, preserving
// every other key in the config file and tightening permissions to 0o600 (the
// same hardening saveFiveHourConfig applies). The value is validated before
// writing: an out-of-range value returns an error WITHOUT touching the file
// so callers never see a half-updated config.
func saveFiveHourThresholdConfig(path string, value float64) error {
	if err := validateThresholdValue(value); err != nil {
		return err
	}
	return persistThresholdConfig(path, "5h_threshold", value)
}

// saveWeeklyThresholdConfig is the weekly counterpart to
// saveFiveHourThresholdConfig. Same validation/permission guarantees.
func saveWeeklyThresholdConfig(path string, value float64) error {
	if err := validateThresholdValue(value); err != nil {
		return err
	}
	return persistThresholdConfig(path, "weekly_threshold", value)
}

// loadThresholdFromConfig is the shared loader. It mirrors loadFiveHourConfig:
// empty path / missing file / missing key all fall back to the documented
// default. Malformed JSON returns an error so the CLI can refuse to start.
// A present-but-wrong-typed value is also an error: silently coercing a
// string to 0 (or any other default) would let users think they had
// configured a threshold when they had not.
func loadThresholdFromConfig(path, key string, defaultValue float64) (float64, error) {
	if strings.TrimSpace(path) == "" {
		return defaultValue, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return defaultValue, nil
		}
		return 0, fmt.Errorf("reading config file: %w", err)
	}

	if len(bytes.TrimSpace(data)) == 0 {
		return defaultValue, nil
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return 0, fmt.Errorf("parsing config file JSON: %w", err)
	}

	raw, ok := payload[key]
	if !ok {
		return defaultValue, nil
	}

	switch v := raw.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, fmt.Errorf("invalid %s value: must be finite", key)
		}
		return v, nil
	case json.Number:
		parsed, err := v.Float64()
		if err != nil {
			return 0, fmt.Errorf("invalid %s value %q: %w", key, v.String(), err)
		}
		if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return 0, fmt.Errorf("invalid %s value: must be finite", key)
		}
		return parsed, nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	default:
		return 0, fmt.Errorf("invalid %s value of type %T (expected a number)", key, raw)
	}
}

// persistThresholdConfig writes the threshold under fieldName, preserving
// every other key already in the file and tightening permissions to 0o600
// on non-Windows platforms. Mirrors saveFiveHourConfig so the two writers
// stay symmetric for tests and operators.
func persistThresholdConfig(path, fieldName string, value float64) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("config file path is empty")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	payload := make(map[string]any)
	if data, err := os.ReadFile(path); err == nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &payload); err != nil {
			return fmt.Errorf("parsing existing config file: %w", err)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading existing config file: %w", err)
	}

	payload[fieldName] = value

	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding config file: %w", err)
	}

	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return fmt.Errorf("writing config file: %w", err)
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("tightening config file permissions: %w", err)
		}
	}

	return nil
}

// validateThresholdValue is the in-process half of validateThresholdPercent.
// It is reused by the loaders to refuse corrupted-but-readable JSON (e.g. an
// explicit value of 0 or 200 left on disk by an older or buggy version of the
// CLI) so the runtime path never operates with a nonsensical threshold.
func validateThresholdValue(value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return errors.New("threshold must be finite")
	}
	if value <= 0 {
		return errors.New("threshold must be greater than 0")
	}
	if value > 100 {
		return errors.New("threshold must be at most 100")
	}
	return nil
}

func ensureCurrentAccountRegistered(accountsFile string, account map[string]any, usage usageWindow, fiveHourMode bool, thresholds rotationThresholds) error {
	return persistOpenAIAccount(accountsFile, accountWithUsage(account, usage, fiveHourMode, thresholds))
}

func accountWithUsage(account map[string]any, usage usageWindow, fiveHourMode bool, thresholds rotationThresholds) map[string]any {
	accountWithUsage := copyAccountData(account)
	accountWithUsage["user_id"] = usage.UserID
	accountWithUsage["usedPercent"] = usage.UsedPercent
	accountWithUsage["email"] = usageEmailOrFallback(usage.Email, usage.UserID)
	if usage.ResetAt != nil {
		accountWithUsage["resetAt"] = *usage.ResetAt
	} else {
		delete(accountWithUsage, "resetAt")
	}

	if fiveHourMode {
		// Persist the secondary (weekly) window when 5h mode is on so the
		// account store mirrors the API contract for that mode.
		if strings.TrimSpace(usage.SecondaryUsedPercent) != "" {
			accountWithUsage["secondaryUsedPercent"] = usage.SecondaryUsedPercent
		} else {
			delete(accountWithUsage, "secondaryUsedPercent")
		}
		if usage.SecondaryResetAt != nil {
			accountWithUsage["secondaryResetAt"] = *usage.SecondaryResetAt
		} else {
			delete(accountWithUsage, "secondaryResetAt")
		}
	} else {
		// Strip any stale secondary fields persisted by older runs so the store
		// file matches the current 5h-off contract.
		delete(accountWithUsage, "secondaryUsedPercent")
		delete(accountWithUsage, "secondaryResetAt")
	}

	// Cooldown rules:
	//   - 5h OFF: only the primary (weekly) window matters. Threshold 98%.
	//   - 5h ON:  primary (5h) at 80% AND/OR secondary (weekly) at 98%.
	//     When both windows are exhausted, take the later reset so the account
	//     stays out of rotation until BOTH windows have recovered. When only
	//     one is exhausted, take that window's reset.
	var cooldownReset *int64
	if fiveHourMode {
		primaryExhausted := usedPercentAtOrAboveThreshold(usage.UsedPercent, thresholds.FiveHour) && usage.ResetAt != nil
		secondaryExhausted := usedPercentAtOrAboveThreshold(usage.SecondaryUsedPercent, thresholds.Weekly) && usage.SecondaryResetAt != nil
		switch {
		case primaryExhausted && secondaryExhausted:
			if *usage.ResetAt >= *usage.SecondaryResetAt {
				cooldownReset = usage.ResetAt
			} else {
				cooldownReset = usage.SecondaryResetAt
			}
		case primaryExhausted:
			cooldownReset = usage.ResetAt
		case secondaryExhausted:
			cooldownReset = usage.SecondaryResetAt
		}
	} else {
		if usedPercentAtOrAboveThreshold(usage.UsedPercent, thresholds.Weekly) && usage.ResetAt != nil {
			cooldownReset = usage.ResetAt
		}
	}

	if cooldownReset != nil {
		accountWithUsage["cooldownUntil"] = *cooldownReset
	} else {
		delete(accountWithUsage, "cooldownUntil")
	}

	return accountWithUsage
}

func readOpenAIAccount(authFile string) (map[string]any, error) {
	data, err := os.ReadFile(authFile)
	if err != nil {
		return nil, fmt.Errorf("reading auth file: %w", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("parsing auth file JSON: %w", err)
	}

	account, err := extractOpenAIAccount(payload)
	if err != nil {
		return nil, err
	}

	return account, nil
}

func extractOpenAIAccount(payload map[string]any) (map[string]any, error) {
	token, err := extractToken(payload)
	if err != nil {
		return nil, err
	}

	account := map[string]any{
		"access": token,
	}

	if accountID, ok := extractAccountID(payload); ok {
		account["accountId"] = accountID
	}

	for key, value := range payload {
		if !strings.HasPrefix(key, "openai.") {
			continue
		}

		shortKey := strings.TrimPrefix(key, "openai.")
		if shortKey == "access" || shortKey == "accountId" || strings.TrimSpace(shortKey) == "" {
			continue
		}
		account[shortKey] = value
	}

	if rawOpenAI, ok := payload["openai"]; ok {
		if openAIMap, ok := rawOpenAI.(map[string]any); ok {
			for key, value := range openAIMap {
				if key == "access" || key == "accountId" || strings.TrimSpace(key) == "" {
					continue
				}
				if _, exists := account[key]; !exists {
					account[key] = value
				}
			}
		}
	}

	return account, nil
}

func extractToken(payload map[string]any) (string, error) {
	if raw, ok := payload["openai.access"]; ok {
		token, ok := raw.(string)
		if !ok || strings.TrimSpace(token) == "" {
			return "", errors.New(`auth JSON key "openai.access" must be a non-empty string`)
		}
		return token, nil
	}

	rawOpenAI, ok := payload["openai"]
	if !ok {
		return "", errors.New(`auth JSON missing key "openai.access"`)
	}

	openAIMap, ok := rawOpenAI.(map[string]any)
	if !ok {
		return "", errors.New(`auth JSON key "openai" is not an object`)
	}

	rawAccess, ok := openAIMap["access"]
	if !ok {
		return "", errors.New(`auth JSON missing key "openai.access"`)
	}

	token, ok := rawAccess.(string)
	if !ok || strings.TrimSpace(token) == "" {
		return "", errors.New(`auth JSON key "openai.access" must be a non-empty string`)
	}

	return token, nil
}

func extractAccountID(payload map[string]any) (string, bool) {
	if raw, ok := payload["openai.accountId"]; ok {
		accountID, ok := raw.(string)
		if !ok || strings.TrimSpace(accountID) == "" {
			return "", false
		}
		return accountID, true
	}

	rawOpenAI, ok := payload["openai"]
	if !ok {
		return "", false
	}

	openAIMap, ok := rawOpenAI.(map[string]any)
	if !ok {
		return "", false
	}

	rawAccountID, ok := openAIMap["accountId"]
	if !ok {
		return "", false
	}

	accountID, ok := rawAccountID.(string)
	if !ok || strings.TrimSpace(accountID) == "" {
		return "", false
	}

	return accountID, true
}

// refreshOAuthAccount rotates account's OAuth tuple by POSTing the stored
// refresh token to the OpenAI token endpoint. The helper is the reactive
// refresh primitive: callers invoke it ONLY after the usage endpoint has
// returned 401 to signal that the current access token is expired or
// invalid. It never inspects `expires`, `cfg.Now`, or any time-based
// threshold; that policy lives entirely at the call sites.
//
// The returned map is a fresh copy on success; the input map is NEVER
// mutated. On any failure (missing refresh token, non-200 response,
// transport error, malformed JSON, missing fields, positive
// expires_in) the helper returns (nil, err) and the caller MUST treat the
// original account as unchanged. Errors never echo credential values, JWT
// payload, or response bodies.
//
// In-memory only: the helper neither reads nor writes any auth or accounts
// file. Callers that need the rotated tuple persisted to disk must invoke
// the existing store/auth writers themselves after a nil error.
func refreshOAuthAccount(ctx context.Context, cfg config, account map[string]any) (map[string]any, error) {
	if account == nil {
		return nil, errors.New("refresh target is nil")
	}

	refreshToken, _ := account["refresh"].(string)
	if strings.TrimSpace(refreshToken) == "" {
		return nil, errors.New("refresh required but refresh token is missing")
	}

	newAccess, newRefresh, expiresIn, err := performOAuthRefreshRequest(ctx, cfg, refreshToken)
	if err != nil {
		return nil, err
	}

	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	nowMs := cfg.Now().UnixMilli()

	rotated := copyAccountData(account)
	rotated["access"] = newAccess
	rotated["refresh"] = newRefresh
	rotated["expires"] = nowMs + int64(expiresIn)*1000

	// accountId is derived from the JWT claim when valid; otherwise the
	// existing accountId (preserved by copyAccountData) is kept verbatim.
	if accountID, ok := extractChatGPTAccountIDFromJWT(newAccess); ok {
		rotated["accountId"] = accountID
	}

	return rotated, nil
}

// overwritePiAuthInfoOAuth mirrors a successfully-rotated Pi credential into
// the in-memory piInfo snapshot so the subsequent reconcilePiInboundFromPayload
// call reads the rotated tuple instead of the stale pre-refresh snapshot. The
// helper keeps piInfo.Payload["openai-codex"] pointing at the same underlying
// map as piInfo.CredMap so callers that read either view stay consistent. It
// only touches OAuth fields (access, refresh, expires, optional accountId, type)
// and never invents identity/usage metadata.
func overwritePiAuthInfoOAuth(piInfo *piAuthInfo, refreshed map[string]any) {
	if piInfo == nil || refreshed == nil || piInfo.CredMap == nil {
		return
	}
	if v, ok := refreshed["access"]; ok {
		piInfo.CredMap["access"] = v
	}
	if v, ok := refreshed["refresh"]; ok {
		piInfo.CredMap["refresh"] = v
	}
	if v, ok := refreshed["expires"]; ok {
		piInfo.CredMap["expires"] = v
	}
	if aid, ok := refreshed["accountId"].(string); ok && strings.TrimSpace(aid) != "" {
		piInfo.CredMap["accountId"] = aid
	} else {
		delete(piInfo.CredMap, "accountId")
	}
	if rawType, ok := refreshed["type"].(string); ok && strings.TrimSpace(rawType) != "" {
		piInfo.CredMap["type"] = rawType
	}
	if piInfo.Payload != nil {
		piInfo.Payload["openai-codex"] = piInfo.CredMap
	}
}

// selectRefreshSourceFromPi decides whether Pi's openai-codex credential is a
// demonstrably safer/newer refresh source for the same account as ocAccount,
// and when so returns a copy of ocAccount with Pi's OAuth fields merged in
// (user_id/email/custom metadata preserved). Otherwise it returns ocAccount
// unchanged so callers fall back to the OpenCode refresh. It NEVER substitutes
// Pi credentials from a different or unknown account.
//
// Selection rules (reactive refresh only — invoked after the usage endpoint
// already returned 401, so the OC credential is provably stale from the
// endpoint's perspective):
//
//  1. Same-account match: same non-empty access token OR same non-empty
//     accountId. Without that, Pi is treated as a different/unknown account
//     and the OC account is returned untouched.
//  2. Prefer Pi when it is demonstrably newer/safer: its exact-integer
//     `expires` is strictly greater than OC's, OR the access tokens are
//     identical but the refresh tokens differ (Pi is the lock-protected
//     rotating store and the canonical refresh source when access has not
//     changed).
//
// Pi's openai-codex tuple is parsed through parsePiOpenAICodexEntry, so a
// malformed/missing Pi entry returns ocAccount unchanged. When OC's expires
// is missing or fails exact-int64 parsing, Pi wins whenever the "Pi is
// newer" condition can be demonstrated (refresh differs on identical access,
// or Pi's parsed expires is valid) — an OC expires that is unparseable is
// not a signal that Pi is stale.
//
// The returned map is a fresh copy on substitution; ocAccount is never
// mutated. No-merge paths return ocAccount unchanged so the caller's
// original input is reused verbatim.
func selectRefreshSourceFromPi(ocAccount map[string]any, piInfo *piAuthInfo) map[string]any {
	if ocAccount == nil || piInfo == nil || piInfo.CredMap == nil {
		return ocAccount
	}

	piAccount, err := parsePiOpenAICodexEntry(piInfo.CredMap)
	if err != nil {
		return ocAccount
	}

	ocAccess, _ := ocAccount["access"].(string)
	piAccess, _ := piAccount["access"].(string)
	ocAccountID, _ := ocAccount["accountId"].(string)
	piAccountID, _ := piAccount["accountId"].(string)

	// Same-account match guard. Empty/blank values on either side disqualify
	// the equality (so Pi is never substituted when OC carries no token
	// metadata at all, even if Pi happens to match a blank string).
	sameAccess := strings.TrimSpace(ocAccess) != "" && strings.TrimSpace(ocAccess) == strings.TrimSpace(piAccess)
	sameAccountID := strings.TrimSpace(ocAccountID) != "" && strings.TrimSpace(ocAccountID) == strings.TrimSpace(piAccountID)
	if !sameAccess && !sameAccountID {
		return ocAccount
	}

	ocRefresh, _ := ocAccount["refresh"].(string)
	piRefresh, _ := piAccount["refresh"].(string)

	// Lock-protected rotating store: identical access + different refresh is
	// enough to prefer Pi even when expires match. Pi only mutates the refresh
	// through its ${auth.json}.lock-protected writer, so any divergence on an
	// identical access string is authoritative.
	if sameAccess && ocRefresh != piRefresh {
		return mergePiOAuthFieldsIntoOC(ocAccount, piAccount)
	}

	// Newer expires (strictly greater, parsed as exact int64) is the other
	// demonstration of "Pi has been refreshed since OC was last synced". A
	// missing or non-int64 OC expires does not block the substitution: Pi's
	// parsed expires is authoritative.
	piExpires, piErr := parseExactInt64(piAccount["expires"])
	if piErr != nil {
		return ocAccount
	}
	ocExpires, ocErr := parseExactInt64(ocAccount["expires"])
	if ocErr != nil || piExpires > ocExpires {
		return mergePiOAuthFieldsIntoOC(ocAccount, piAccount)
	}

	return ocAccount
}

// mergePiOAuthFieldsIntoOC projects only the OAuth fields from piAccount into
// a copy of ocAccount, preserving OC's user_id/email/custom metadata. Fields
// projected: type, access, refresh, expires, and accountId WHEN Pi has a
// non-empty value (a blank Pi accountId leaves OC's existing accountId
// untouched so we never erase a known identity during a pre-refresh merge).
// The merged map is always a fresh allocation; ocAccount is never mutated.
func mergePiOAuthFieldsIntoOC(ocAccount, piAccount map[string]any) map[string]any {
	merged := copyAccountData(ocAccount)
	if v, ok := piAccount["type"]; ok {
		merged["type"] = v
	}
	if v, ok := piAccount["access"]; ok {
		merged["access"] = v
	}
	if v, ok := piAccount["refresh"]; ok {
		merged["refresh"] = v
	}
	if v, ok := piAccount["expires"]; ok {
		merged["expires"] = v
	}
	if aid, ok := piAccount["accountId"].(string); ok && strings.TrimSpace(aid) != "" {
		merged["accountId"] = aid
	}
	return merged
}

// oauthRefreshRejectedError is returned when the OAuth refresh endpoint
// itself responds with HTTP 401 Unauthorized. The actionable message tells
// the user the refresh token is no longer accepted without echoing the
// token, the response body, or any other credential material. Callers
// detect it via errors.As to surface the re-login instruction; non-401
// statuses continue to use the generic "OAuth refresh endpoint returned N"
// phrasing.
type oauthRefreshRejectedError struct{}

func (e *oauthRefreshRejectedError) Error() string {
	return "OAuth refresh token was rejected; sign in again"
}

// performOAuthRefreshRequest POSTs to the OAuth refresh endpoint with the
// required form fields and validates the response shape. The access and
// refresh tokens are NEVER included in the returned error so a hostile
// endpoint cannot exfiltrate them through the CLI's user-visible output.
//
// The wire format is pinned to byte-level parity with pi-main: the body is
// emitted in the documented insertion order
// `grant_type`, `refresh_token`, `client_id` using
// `application/x-www-form-urlencoded` percent-escaping via url.QueryEscape
// (the same primitive url.Values.Encode uses internally, so each value is
// encoded exactly as the canonical form would encode it). url.Values.Encode
// is NOT used directly because it sorts keys alphabetically and would emit
// the body in the order `client_id`, `grant_type`, `refresh_token` — that
// byte-level divergence from pi-main is what an independent reviewer would
// catch. The Content-Type is `application/x-www-form-urlencoded` and the
// request intentionally does NOT carry an Accept header; Go's net/http
// package would otherwise add its own `Accept-Encoding` defaults, which
// would also diverge from pi-main.
func performOAuthRefreshRequest(ctx context.Context, cfg config, refreshToken string) (string, string, int64, error) {
	tokenURL := strings.TrimSpace(cfg.TokenURL)
	if tokenURL == "" {
		tokenURL = defaultOAuthRefreshTokenURL
	}

	var body strings.Builder
	body.WriteString("grant_type=refresh_token&refresh_token=")
	body.WriteString(url.QueryEscape(refreshToken))
	body.WriteString("&client_id=")
	body.WriteString(url.QueryEscape(oauthRefreshClientID))

	reqCtx, cancel := context.WithTimeout(ctx, oauthRefreshRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, tokenURL, strings.NewReader(body.String()))
	if err != nil {
		return "", "", 0, fmt.Errorf("building OAuth refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Intentionally no Accept header: stay byte-equivalent to pi-main.

	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, fmt.Errorf("OAuth refresh request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Drain (but never echo) the response body so the bearer token cannot
		// be smuggled back through a 4xx/5xx message.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusUnauthorized {
			// 401 from the OAuth refresh endpoint means the supplied refresh
			// token is no longer accepted (rotated or revoked upstream). Surface
			// the actionable re-login instruction instead of the raw status so
			// the user is told to sign in again without echoing the token,
			// response body, or any other credential material.
			return "", "", 0, &oauthRefreshRejectedError{}
		}
		return "", "", 0, fmt.Errorf("OAuth refresh endpoint returned %d", resp.StatusCode)
	}

	decoder := json.NewDecoder(resp.Body)
	decoder.UseNumber()

	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return "", "", 0, fmt.Errorf("parsing OAuth refresh response JSON: %w", err)
	}

	newAccess, ok := payload["access_token"].(string)
	if !ok || strings.TrimSpace(newAccess) == "" {
		return "", "", 0, errors.New("OAuth refresh response missing non-empty access_token")
	}
	newRefresh, ok := payload["refresh_token"].(string)
	if !ok || strings.TrimSpace(newRefresh) == "" {
		return "", "", 0, errors.New("OAuth refresh response missing non-empty refresh_token")
	}

	rawExpiresIn, ok := payload["expires_in"]
	if !ok || rawExpiresIn == nil {
		return "", "", 0, errors.New("OAuth refresh response missing expires_in")
	}
	expiresIn, err := parseExactInt64(rawExpiresIn)
	if err != nil {
		return "", "", 0, fmt.Errorf("parsing OAuth refresh expires_in: %w", err)
	}
	if expiresIn <= 0 {
		return "", "", 0, errors.New("OAuth refresh response expires_in must be positive")
	}

	return newAccess, newRefresh, expiresIn, nil
}

// extractChatGPTAccountIDFromJWT decodes the JWT payload and returns the
// chatgpt_account_id when it is a non-empty string. Pi's real rotated access
// tokens nest the identity under https://api.openai.com/auth as a JSON object
// whose chatgpt_account_id field carries the id. A legacy flat claim under
// https://api.openai.com/auth.chatgpt_account_id is still accepted as a
// fallback so older issuers keep working. Returns ("", false) for any
// malformed token, malformed payload, missing/empty/wrong-typed claim, or a
// claim nested under a non-object value. Errors are intentionally swallowed:
// the helper is best-effort and never surfaces credential material; callers
// must preserve the existing accountId when this returns false.
func extractChatGPTAccountIDFromJWT(token string) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return "", false
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", false
	}
	if raw, ok := claims[chatGPTAccountIDNamespace]; ok && raw != nil {
		// Pi's real shape: a JSON object with a chatgpt_account_id field. We
		// only honor the object form here; a string at the namespace key is
		// not the documented shape and falls through to the legacy flat claim.
		if nested, ok := raw.(map[string]any); ok {
			if v, ok := nested[chatGPTAccountIDField]; ok && v != nil {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return s, true
				}
			}
		}
	}
	if raw, ok := claims[chatGPTAccountIDClaim]; ok && raw != nil {
		if s, ok := raw.(string); ok && strings.TrimSpace(s) != "" {
			return s, true
		}
	}
	return "", false
}

// syncAccountToOpenCodeAuth writes account's OAuth tuple to OpenCode's
// auth.json when the provider is installed (file exists on disk). Returns
// nil when the provider is absent; callers must not invent an auth file on
// a host where OpenCode has never been installed.
func syncAccountToOpenCodeAuth(cfg config, account map[string]any) error {
	if strings.TrimSpace(cfg.AuthFile) == "" || !regularFileExists(cfg.AuthFile) {
		return nil
	}
	return updateOpenAIAuthFile(cfg.AuthFile, account)
}

// syncAccountToPiAuth writes account's OAuth tuple to Pi's auth.json when
// the Pi directory is installed, reusing the existing ${auth.json}.lock
// update machinery under updatePiAuthFile. Returns nil when Pi is absent.
//
// This helper is the Pi-only persistence primitive: the caller has already
// authenticated against the Pi session and is propagating its own rotated
// tuple back to disk. It MUST always persist the rotated credential —
// including credentials without an accountId — so a Pi-only refresh that
// loses its accountId during rotation still converges back into Pi's
// auth.json. Account-identity guards belong at the OpenCode/default/
// accounts refresh call sites (see syncRefreshedCredentialToBoth) where a
// DIFFERENT account's rotated OC tuple must NEVER bleed into Pi's session;
// the Pi-only refresh path never has that ambiguity because there is no
// "other account" to protect Pi from — the rotated tuple IS the active Pi
// session.
//
// Concurrency note: this writes through updatePiAuthFile's existing lock
// protocol. The reactive refresh path within a single process is naturally
// single-flight: a 401-driven refresh runs once and the rotated tuple is
// reused by the same caller, but two concurrent CLI processes racing the
// same Pi file can both observe a 401 from the usage endpoint and POST a
// refresh. The lock only serializes the file write; the per-process
// refresh decision is racy by design and surfaces as a "second CLI run
// picked up the rotated tuple" symptom, not a credential corruption.
func syncAccountToPiAuth(cfg config, account map[string]any) error {
	if strings.TrimSpace(cfg.PiAuthFile) == "" || !directoryExists(filepath.Dir(cfg.PiAuthFile)) {
		return nil
	}
	cred, err := buildPiCredential(account)
	if err != nil {
		return err
	}
	return updatePiAuthFile(cfg.PiAuthFile, cred)
}

// piAuthMatchesAccount reports whether Pi's currently-installed
// openai-codex tuple is the same account as account. The match mirrors
// selectRefreshSourceFromPi: same non-empty accountId OR same non-empty
// access token. A blank/empty value on either side disqualifies the
// equality.
//
// The return contract distinguishes a CLEAN "Pi is not the same account"
// answer from an UNKNOWN outcome so the dual-provider refresh path never
// collapses an unreadable Pi identity into a silent Pi-skip:
//   - (false, nil): Pi is not installed (errPiAuthMissing from the read
//     helper), OR Pi is installed but carries no openai-codex entry, OR
//     Pi's openai-codex entry parsed cleanly and demonstrably does NOT
//     match account. The caller may safely treat Pi as absent for this
//     refresh and write only the eligible single store.
//   - (true, nil): Pi is installed and its openai-codex is demonstrably
//     the same account as account. The caller may proceed with the
//     dual-write protocol.
//   - (false, err): Pi identity could not be read (lock contention,
//     malformed JSON, an entry that is not a JSON object, or a parse
//     failure). The caller MUST surface the error BEFORE mutating any
//     auth file; both stores must remain byte-identical to their pre-call
//     state so an UNKNOWN Pi identity can never bleed a rotated
//     credential into Pi.
func piAuthMatchesAccount(cfg config, account map[string]any) (bool, error) {
	if account == nil {
		return false, nil
	}
	payload, err := readPiAuthUnderLock(cfg.PiAuthFile)
	if err != nil {
		if errors.Is(err, errPiAuthMissing) {
			// Pi is not installed. Clean signal: caller may skip Pi.
			return false, nil
		}
		// Unknown: lock contention, malformed IO, parse error, ...
		// The caller MUST surface this before touching either auth file.
		return false, fmt.Errorf("reading Pi auth identity: %w", err)
	}
	rawCred, present := payload["openai-codex"]
	if !present || rawCred == nil {
		// Pi installed but no openai-codex credential. Clean signal:
		// caller may skip Pi.
		return false, nil
	}
	credMap, ok := rawCred.(map[string]any)
	if !ok {
		return false, fmt.Errorf("Pi openai-codex entry is not a JSON object (got %T)", rawCred)
	}
	piAccount, err := parsePiOpenAICodexEntry(credMap)
	if err != nil {
		return false, fmt.Errorf("parsing Pi openai-codex credential: %w", err)
	}

	ocAccountID, _ := account["accountId"].(string)
	piAccountID, _ := piAccount["accountId"].(string)
	if strings.TrimSpace(ocAccountID) != "" && strings.TrimSpace(ocAccountID) == strings.TrimSpace(piAccountID) {
		return true, nil
	}

	ocAccess, _ := account["access"].(string)
	piAccess, _ := piAccount["access"].(string)
	if strings.TrimSpace(ocAccess) != "" && strings.TrimSpace(ocAccess) == strings.TrimSpace(piAccess) {
		return true, nil
	}
	return false, nil
}

// syncRefreshedCredentialToBoth propagates a refreshed OAuth credential to
// the installed auth stores (OpenCode, Pi Agent) under the safest available
// cross-file coherence. It is reserved for the OAuth-refresh path so the
// rotated credential never leaves the on-disk stores out of sync.
// Account-identity guard for Pi: when Pi is installed AND its currently-
// stored openai-codex credential is NOT demonstrably the same account as
// the rotated tuple, the helper treats Pi as absent for THIS refresh: it
// writes only to OpenCode and leaves Pi's auth.json byte-identical to its
// pre-call state, without ever acquiring the Pi lock. The identity check
// mirrors selectRefreshSourceFromPi / piAuthMatchesAccount (same
// non-empty accountId, OR same non-empty access token). This is the only
// place the dual-provider refresh path protects Pi from a DIFFERENT
// account's rotated OpenCode credential — the underlying
// syncAccountToPiAuth helper is the Pi-only persistence primitive and
// intentionally always persists its own rotated tuple.
//
// Unknown Pi identity aborts the refresh sync with both files untouched:
// piAuthMatchesAccount distinguishes (false, nil) "Pi is a clean
// different account" from (false, err) "Pi identity could not be read"
// (lock contention, malformed JSON, parse error, ...). When the result
// is the unknown case the helper returns the error BEFORE writing to
// either auth store so a locked or malformed Pi can never cause the OC
// write to leak a rotated credential while Pi stays out of sync.
//
// When BOTH eligible stores (OC and Pi-installed-and-same-account) the
// helper:
//   - Snapshots the file written FIRST (so a SECOND-store failure can
//     restore it byte-for-byte).
//   - Takes the Pi ${auth.json}.lock as a preflight; if contention is
//     detected BEFORE any mutation it returns a clear error and leaves
//     both files untouched. (The lock probe is acquired and immediately
//     released so the real updatePiAuthFile call can re-acquire cleanly.)
//   - Writes the FIRST store, then the SECOND. If the SECOND write fails
//     after the FIRST succeeded, the FIRST is rolled back from the
//     snapshot. The returned error names both the failing store and
//     whether the rollback succeeded; when the rollback itself fails,
//     both diagnostics are joined so the operator can see the cross-file
//     inconsistency instead of having it masked.
//
// When ONLY ONE eligible store exists the helper delegates to the matching
// single-store helper so the documented "absent provider is a no-op"
// semantics for the refresh path are preserved. When NEITHER store is
// installed (or both are filtered out by the identity guard) it returns
// nil without touching any file.
//
// ActivateAccount (used by `use <selector>` and rotation) is intentionally
// untouched: activation already validates the OAuth tuple before mutating
// either file and is documented to surface partial-sync errors without
// rolling back, so the broader activation contract is preserved.
func syncRefreshedCredentialToBoth(cfg config, account map[string]any, piFirst bool) error {
	ocFile := strings.TrimSpace(cfg.AuthFile)
	piFile := strings.TrimSpace(cfg.PiAuthFile)
	ocEnabled := ocFile != "" && regularFileExists(ocFile)
	piInstalled := piFile != "" && directoryExists(filepath.Dir(piFile))

	// Pi is eligible for this sync only when (a) it is installed and (b)
	// its currently-stored openai-codex credential is demonstrably the same
	// account as the rotated tuple. The Pi-only refresh path uses
	// syncAccountToPiAuth directly and is unaffected by this guard.
	//
	// The identity check is performed BEFORE any file is touched. A CLEAN
	// "different account" answer is collapsed into the OC-only path below;
	// an UNKNOWN answer (lock contention, malformed JSON, parse error,
	// non-object entry) aborts the refresh sync with both auth files left
	// byte-identical to their pre-call state — see piAuthMatchesAccount.
	piEligible := false
	if piInstalled {
		match, err := piAuthMatchesAccount(cfg, account)
		if err != nil {
			return fmt.Errorf("Pi auth identity check before refresh sync: %w", err)
		}
		piEligible = match
	}

	if !ocEnabled && !piEligible {
		return nil
	}
	if ocEnabled != piEligible {
		if ocEnabled {
			return syncAccountToOpenCodeAuth(cfg, account)
		}
		return syncAccountToPiAuth(cfg, account)
	}

	firstFile := ocFile
	if piFirst {
		firstFile = piFile
	}
	otherLabel := "Pi"
	if piFirst {
		otherLabel = "OpenCode"
	}
	firstLabel := "OpenCode"
	if piFirst {
		firstLabel = "Pi"
	}

	// Snapshot the FIRST file so a SECOND-store failure can restore it.
	firstSnapshot, snapshotErr := os.ReadFile(firstFile)
	snapshotOK := snapshotErr == nil

	// Preflight the Pi lock so contention is detected BEFORE any mutation.
	// Probing acquire+release is the cleanest signal: updatePiAuthFile can
	// re-acquire the lock cleanly during the real write below.
	lockPath := piFile + ".lock"
	if err := acquirePiLock(lockPath); err != nil {
		return fmt.Errorf("Pi auth lock contention before refresh sync: %w", err)
	}
	if relErr := releasePiLock(lockPath); relErr != nil {
		return fmt.Errorf("Pi auth lock probe release failed before refresh sync: %w", relErr)
	}

	var firstSyncErr error
	if piFirst {
		firstSyncErr = syncAccountToPiAuth(cfg, account)
	} else {
		firstSyncErr = syncAccountToOpenCodeAuth(cfg, account)
	}
	if firstSyncErr != nil {
		// SECOND file was never targeted; no rollback needed.
		return firstSyncErr
	}

	var secondSyncErr error
	if piFirst {
		secondSyncErr = syncAccountToOpenCodeAuth(cfg, account)
	} else {
		secondSyncErr = syncAccountToPiAuth(cfg, account)
	}
	if secondSyncErr == nil {
		return nil
	}

	if !snapshotOK {
		return fmt.Errorf("%s auth sync failed: %w (%s auth file could not be snapshotted for rollback; the %s auth file was written before the failure)",
			otherLabel, secondSyncErr, firstLabel, firstLabel)
	}
	if restoreErr := os.WriteFile(firstFile, firstSnapshot, 0o600); restoreErr != nil {
		return fmt.Errorf("%s auth sync failed: %w (rollback of %s auth file also failed: %v; cross-file auth state may be inconsistent)",
			otherLabel, secondSyncErr, firstLabel, restoreErr)
	}
	return fmt.Errorf("%s auth sync failed: %w (%s auth file restored from pre-mutation snapshot)",
		otherLabel, secondSyncErr, firstLabel)
}

// accountDerivedFieldKeys lists every account key whose value reflects only
// the latest usage refresh and is owned by accountWithUsage. Persisted records
// must NOT carry these forward when an incoming refresh omits them: a toggle
// from 5h ON to OFF must clear the secondary window, a usage drop below the
// threshold must clear cooldownUntil, and a missing API field must not leave a
// stale value behind. accountWithUsage already deletes the keys from the
// incoming copy; persistOpenAIAccount must apply the same deletion policy to
// the previously-persisted record before merging so the deletion is not
// silently undone by the merge.
//
// Auth and account metadata (access, refresh, accountId, email, user_id) and
// any caller-defined custom fields are intentionally excluded — they must
// always be preserved across refreshes.
var accountDerivedFieldKeys = []string{
	"usedPercent",
	"resetAt",
	"secondaryUsedPercent",
	"secondaryResetAt",
	"cooldownUntil",
}

// clearAccountDerivedFields strips every usage-derived field from account so
// the next refresh can repopulate only the keys it actually owns. Tombstone
// or null artifacts are never written: keys that should no longer exist are
// deleted, not blanked.
func clearAccountDerivedFields(account map[string]any) {
	for _, key := range accountDerivedFieldKeys {
		delete(account, key)
	}
}

func persistOpenAIAccount(accountsFile string, account map[string]any) error {
	userID, ok := account["user_id"].(string)
	if !ok || strings.TrimSpace(userID) == "" {
		return errors.New("account data missing non-empty user_id")
	}

	if err := os.MkdirAll(filepath.Dir(accountsFile), 0o700); err != nil {
		return fmt.Errorf("creating accounts directory: %w", err)
	}

	store, err := readAccountsStore(accountsFile)
	if err != nil {
		return err
	}

	store = normalizeAccountsStoreByUserID(store)

	copyAccount := copyAccountData(account)
	if existing, ok := store[userID]; ok {
		merged := copyAccountData(existing)
		// The merge below only assigns keys present in copyAccount, so any
		// derived key that the latest refresh legitimately dropped (e.g.
		// secondaryUsedPercent after a toggle OFF, or cooldownUntil after a
		// drop below threshold) would otherwise survive from the previously
		// persisted record. Clear derived fields first; the refresh then
		// repopulates only the ones it owns. Auth and custom metadata are
		// untouched and remain available for the subsequent copy.
		clearAccountDerivedFields(merged)
		for key, value := range copyAccount {
			merged[key] = value
		}
		copyAccount = merged
	}

	store[userID] = copyAccount

	if err := writeAccountsStore(accountsFile, store); err != nil {
		return err
	}

	return nil
}

func normalizeAccountsStoreByUserID(store map[string]map[string]any) map[string]map[string]any {
	normalized := make(map[string]map[string]any, len(store))

	mergeEntry := func(key string, entry map[string]any) {
		if existing, ok := normalized[key]; ok {
			merged := copyAccountData(existing)
			for field, value := range entry {
				merged[field] = value
			}
			normalized[key] = merged
			return
		}
		normalized[key] = entry
	}

	for key, entry := range store {
		entryCopy := copyAccountData(entry)
		if userID, ok := entryCopy["user_id"].(string); ok && strings.TrimSpace(userID) != "" {
			entryCopy["user_id"] = userID
			mergeEntry(userID, entryCopy)
			continue
		}

		mergeEntry(key, entryCopy)
	}

	return normalized
}

func copyAccountData(account map[string]any) map[string]any {
	copyAccount := make(map[string]any, len(account))
	for key, value := range account {
		copyAccount[key] = value
	}
	return copyAccount
}

func readAccountsStore(accountsFile string) (map[string]map[string]any, error) {
	store := make(map[string]map[string]any)
	data, err := os.ReadFile(accountsFile)
	if err == nil {
		if len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, &store); err != nil {
				return nil, fmt.Errorf("parsing accounts file JSON: %w", err)
			}
		}
		return store, nil
	}

	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}

	return nil, fmt.Errorf("reading accounts file: %w", err)
}

func writeAccountsStore(accountsFile string, store map[string]map[string]any) error {

	encoded, err := json.Marshal(store)
	if err != nil {
		return fmt.Errorf("encoding accounts JSON: %w", err)
	}

	if err := os.WriteFile(accountsFile, encoded, 0o600); err != nil {
		return fmt.Errorf("writing accounts file: %w", err)
	}

	return nil
}

// redactBearerToken replaces any exact occurrence of token within msg with a
// fixed placeholder so non-200 error messages from hostile or buggy endpoints
// never echo the bearer access token back to logs or callers. Non-secret text
// is preserved.
func redactBearerToken(msg, token string) string {
	if token == "" || msg == "" {
		return msg
	}
	return strings.ReplaceAll(msg, token, "[REDACTED]")
}

// usageEndpointError carries the HTTP status the usage endpoint returned so
// callers can branch on it (e.g. trigger a reactive OAuth refresh on 401).
// The Error() string preserves the historical `usage endpoint returned N`
// phrasing so user-visible output and existing substring assertions stay
// stable.
type usageEndpointError struct {
	StatusCode int
	Body       string
}

func (e *usageEndpointError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("usage endpoint returned %d: %s", e.StatusCode, e.Body)
	}
	return fmt.Sprintf("usage endpoint returned %d", e.StatusCode)
}

func fetchUsageWindow(ctx context.Context, client *http.Client, usageURL, token string) (usageWindow, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return usageWindow{}, fmt.Errorf("building usage request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return usageWindow{}, fmt.Errorf("requesting usage endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(message))
		if token != "" {
			msg = redactBearerToken(msg, token)
		}
		return usageWindow{}, &usageEndpointError{StatusCode: resp.StatusCode, Body: msg}
	}

	decoder := json.NewDecoder(resp.Body)
	decoder.UseNumber()

	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return usageWindow{}, fmt.Errorf("parsing usage JSON: %w", err)
	}

	window, err := extractUsageWindow(payload)
	if err != nil {
		return usageWindow{}, err
	}

	return window, nil
}

// refreshSyncError wraps an error returned by the caller's persistFn in
// fetchUsageWithReactiveRefresh. Callers detect it via errors.As to tell
// the post-refresh auth-store sync apart from a refresh POST failure or a
// retry that still returned 401. Only the auth-sync path attaches this
// marker, so a wrap implies the refresh POST itself succeeded and the
// rotated credential is in memory but may not be on disk.
type refreshSyncError struct{ Err error }

func (e *refreshSyncError) Error() string { return e.Err.Error() }
func (e *refreshSyncError) Unwrap() error { return e.Err }

// fetchUsageWithReactiveRefresh queries the usage endpoint with the supplied
// access token. When the first call returns HTTP 401 it issues ONE OAuth
// refresh, persists the rotated credential via persistFn (when non-nil),
// and retries the usage call exactly once with the new token. A second
// 401, any other non-200, or a refresh POST failure aborts without further
// retries; the auth/store bytes are left unchanged on a refresh failure
// because persistFn was never invoked.
//
// Non-401 responses (400, 403, 429, 500, ...) are returned without invoking
// the token endpoint, preserving their existing error semantics.
//
// A 401 with missing or empty `refresh` metadata returns a clear value-free
// error; the token endpoint is NOT contacted and auth/store bytes stay
// unchanged. Errors from refreshOAuthAccount are returned as-is. Errors
// from persistFn are wrapped in *refreshSyncError so callers can branch
// on the auth-sync failure mode (e.g. `accounts`/`list` row-local
// coherence).
//
// persistFn is the caller's chosen persistence strategy. Dual-provider
// callers pass syncRefreshedCredentialToBoth; Pi-only callers pass
// syncAccountToPiAuth; in-memory-only callers (and tests) pass nil.
//
// piInfo is an OPTIONAL hook for guarded Pi-source recovery: when non-nil
// and Pi is installed, the helper consults Pi's openai-codex tuple AFTER
// the 401 is detected and BEFORE the single refresh POST, swapping the
// refresh source to Pi's tuple when it is demonstrably the same account
// and newer/safer (see selectRefreshSourceFromPi). When piInfo is nil or
// the swap does not apply, account is reused unchanged. The swap only
// triggers when the usage endpoint already returned 401, so the OC refresh
// token is never replaced with Pi's when OC's credential is still valid.
//
// The returned account is the rotated copy ONLY when a refresh actually
// occurred; on a non-refresh success path the returned account is nil so
// callers can use the presence-of-refresh as a signal to mirror the
// rotated tuple into in-memory snapshots.
func fetchUsageWithReactiveRefresh(
	ctx context.Context,
	cfg config,
	account map[string]any,
	token string,
	persistFn func(map[string]any) error,
	piInfo *piAuthInfo,
) (usageWindow, map[string]any, error) {
	usage, err := fetchUsageWindow(ctx, cfg.HTTPClient, cfg.UsageURL, token)
	if err == nil {
		return usage, nil, nil
	}
	var usageErr *usageEndpointError
	if !errors.As(err, &usageErr) || usageErr.StatusCode != http.StatusUnauthorized {
		return usageWindow{}, nil, err
	}

	// Guarded Pi-source recovery: swap the refresh source to Pi's tuple
	// only AFTER the 401 is detected, only when the helper has a piInfo,
	// and only when Pi is demonstrably the same account and newer/safer.
	// When the swap does not apply, account is reused unchanged so the OC
	// refresh token is the only refresh attempted.
	refreshSource := account
	if piInfo != nil {
		refreshSource = selectRefreshSourceFromPi(account, piInfo)
	}

	rotated, refreshErr := refreshOAuthAccount(ctx, cfg, refreshSource)
	if refreshErr != nil {
		return usageWindow{}, nil, refreshErr
	}
	if persistFn != nil {
		if persistErr := persistFn(rotated); persistErr != nil {
			return usageWindow{}, rotated, &refreshSyncError{Err: persistErr}
		}
	}

	newToken, _ := rotated["access"].(string)
	if strings.TrimSpace(newToken) == "" {
		return usageWindow{}, nil, errors.New("refresh succeeded but rotated access token is empty")
	}
	usage, err = fetchUsageWindow(ctx, cfg.HTTPClient, cfg.UsageURL, newToken)
	if err != nil {
		return usageWindow{}, rotated, err
	}
	return usage, rotated, nil
}

func extractUsageWindow(payload map[string]any) (usageWindow, error) {
	window := usageWindow{}

	rateLimit, ok := payload["rate_limit"].(map[string]any)
	if !ok {
		return usageWindow{}, errors.New(`usage JSON missing object "rate_limit"`)
	}

	primaryWindow, ok := rateLimit["primary_window"].(map[string]any)
	if !ok {
		return usageWindow{}, errors.New(`usage JSON missing object "rate_limit.primary_window"`)
	}

	value, ok := primaryWindow["used_percent"]
	if !ok {
		return usageWindow{}, errors.New(`usage JSON missing key "rate_limit.primary_window.used_percent"`)
	}

	usedPercent, err := valueToString(value)
	if err != nil {
		return usageWindow{}, err
	}
	window.UsedPercent = usedPercent

	rawUserID, ok := payload["user_id"]
	if !ok {
		return usageWindow{}, errors.New(`usage JSON missing key "user_id"`)
	}

	userID, ok := rawUserID.(string)
	if !ok || strings.TrimSpace(userID) == "" {
		return usageWindow{}, errors.New(`usage JSON key "user_id" must be a non-empty string`)
	}
	window.UserID = userID

	if rawEmail, ok := payload["email"]; ok && rawEmail != nil {
		if email, ok := rawEmail.(string); ok {
			window.Email = strings.TrimSpace(email)
		}
	}

	resetAt, err := extractResetAt(primaryWindow, "rate_limit.primary_window.reset_at")
	if err != nil {
		return usageWindow{}, err
	}
	window.ResetAt = resetAt

	// `secondary_window` may be missing or null (valid fallback: primary is
	// treated as the weekly window). When the key is present and non-null it
	// MUST be an object that exposes a usable `used_percent` (must exist, be
	// non-null, and parse to a non-empty supported value); `reset_at` remains
	// optional, but a malformed present `reset_at` is a hard error. Tolerating
	// an empty object or a missing/null `used_percent` would risk the
	// persistence/rotation/cooldown pipeline silently interpreting the primary
	// 5h value as the weekly value.
	if secondary, present := rateLimit["secondary_window"]; present && secondary != nil {
		secondaryMap, ok := secondary.(map[string]any)
		if !ok {
			return usageWindow{}, fmt.Errorf(`usage JSON key "rate_limit.secondary_window" must be an object when present, got %T`, secondary)
		}
		used, hasUsed := secondaryMap["used_percent"]
		if !hasUsed {
			return usageWindow{}, errors.New(`usage JSON missing key "rate_limit.secondary_window.used_percent"`)
		}
		if used == nil {
			return usageWindow{}, errors.New(`usage JSON key "rate_limit.secondary_window.used_percent" must not be null`)
		}
		secondaryUsed, sErr := valueToString(used)
		if sErr != nil {
			return usageWindow{}, fmt.Errorf(`parsing usage JSON key "rate_limit.secondary_window.used_percent": %w`, sErr)
		}
		window.SecondaryUsedPercent = secondaryUsed

		secondaryReset, sErr := extractResetAt(secondaryMap, "rate_limit.secondary_window.reset_at")
		if sErr != nil {
			return usageWindow{}, sErr
		}
		if secondaryReset != nil {
			window.SecondaryResetAt = secondaryReset
		}
	}

	return window, nil
}

func extractResetAt(window map[string]any, fieldName string) (*int64, error) {
	rawResetAt, ok := window["reset_at"]
	if !ok || rawResetAt == nil {
		return nil, nil
	}

	parsed, ok := valueToInt64(rawResetAt)
	if !ok {
		return nil, fmt.Errorf("usage JSON key %q has unsupported type %T", fieldName, rawResetAt)
	}

	return &parsed, nil
}

func valueToInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case json.Number:
		parsed, err := v.Int64()
		if err == nil {
			return parsed, true
		}
		floatParsed, err := strconv.ParseFloat(v.String(), 64)
		if err != nil {
			return 0, false
		}
		return int64(floatParsed), true
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	case int:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		return int64(v), true
	case float32:
		return int64(v), true
	default:
		return 0, false
	}
}

func usedPercentAtOrAboveThreshold(usedPercent string, threshold float64) bool {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(usedPercent), 64)
	if err != nil {
		return false
	}

	return parsed >= threshold
}

func selectEligibleAlternateAccount(store map[string]map[string]any, currentUserID string, nowUnix int64, fiveHourMode bool, fiveHourThreshold, weeklyThreshold float64) (map[string]any, bool) {
	store = normalizeAccountsStoreByUserID(store)

	keys := make([]string, 0, len(store))
	for userID := range store {
		keys = append(keys, userID)
	}
	sort.Strings(keys)

	for _, userID := range keys {
		if userID == currentUserID {
			continue
		}

		account := store[userID]
		storedUserID, _ := account["user_id"].(string)
		if strings.TrimSpace(storedUserID) == "" {
			storedUserID = userID
		}
		if strings.TrimSpace(storedUserID) == "" || storedUserID == currentUserID {
			continue
		}

		if token, ok := account["access"].(string); !ok || strings.TrimSpace(token) == "" {
			continue
		}

		// Eligibility is per-candidate. The toggle decides the EVALUATION mode;
		// the persisted shape (dual vs weekly-only) decides which fields apply.
		// See candidateEligible for the full rule table.
		if !candidateEligible(account, nowUnix, fiveHourMode, fiveHourThreshold, weeklyThreshold) {
			continue
		}

		selected := copyAccountData(account)
		selected["user_id"] = storedUserID
		return selected, true
	}

	return nil, false
}

// candidateEligible applies per-candidate rotation eligibility based on the
// current 5h toggle and the candidate's persisted shape (dual vs weekly-only).
// Detecting a persisted dual entry uses both secondaryUsedPercent AND
// secondaryResetAt so legacy entries written before new metadata was added
// still classify correctly.
//
// Rule table:
//
//	toggle | shape        | primary blocked?      | secondary blocked?     | cooldown?
//	------ | ------------ | --------------------- | ---------------------- | -----------
//	ON     | dual         | usedPercent >= 80%    | secondaryUsed >= 98%   | honored
//	ON     | weekly-only  | usedPercent >= 98%    | n/a                    | honored
//	OFF    | stale dual   | ignored (and cooldown | secondaryUsed >= 98%   | ignored
//	      |              | derived from primary  |                        |
//	      |              | is unreliable)        |                        |
//	OFF    | weekly-only  | usedPercent >= 98%    | n/a                    | honored
//
// "Missing weekly data remains conservative/eligible": when the secondary
// window is missing entirely (no secondaryUsedPercent and no usable
// secondaryResetAt) a stale dual candidate is treated as eligible — we never
// block on the absence of data.
func candidateEligible(account map[string]any, nowUnix int64, fiveHourMode bool, fiveHourThreshold, weeklyThreshold float64) bool {
	hasSecondary := candidateHasSecondaryFields(account)

	switch {
	case fiveHourMode && hasSecondary:
		if exceedsThresholdWithFutureReset(account, "usedPercent", "resetAt", fiveHourThreshold, nowUnix) {
			return false
		}
		if exceedsThresholdWithFutureReset(account, "secondaryUsedPercent", "secondaryResetAt", weeklyThreshold, nowUnix) {
			return false
		}
		return !cooldownInFuture(account, nowUnix)

	case fiveHourMode && !hasSecondary:
		// Weekly-only persisted entry under toggle ON: treat primary as weekly.
		if exceedsThresholdWithFutureReset(account, "usedPercent", "resetAt", weeklyThreshold, nowUnix) {
			return false
		}
		return !cooldownInFuture(account, nowUnix)

	case !fiveHourMode && hasSecondary:
		// Stale dual entry under toggle OFF. The stored `cooldownUntil` may have
		// been derived partly from the now-ignored 5h primary, so it is treated
		// as unreliable: ignore it. Only the persisted secondary weekly data
		// blocks. Missing secondary fields are conservative/eligible.
		return !exceedsThresholdWithFutureReset(account, "secondaryUsedPercent", "secondaryResetAt", weeklyThreshold, nowUnix)

	default: // !fiveHourMode && !hasSecondary
		if exceedsThresholdWithFutureReset(account, "usedPercent", "resetAt", weeklyThreshold, nowUnix) {
			return false
		}
		return !cooldownInFuture(account, nowUnix)
	}
}

// candidateHasSecondaryFields reports whether the persisted account carries a
// secondary window. It accepts legacy entries whose only secondary metadata
// is `secondaryResetAt` (no `secondaryUsedPercent`) so per-candidate evaluation
// can still recognize them as dual when the toggle is on.
func candidateHasSecondaryFields(account map[string]any) bool {
	if used, ok := account["secondaryUsedPercent"].(string); ok && strings.TrimSpace(used) != "" {
		return true
	}
	if _, hasReset := extractStoredResetAt(account, "secondaryResetAt"); hasReset {
		return true
	}
	return false
}

// exceedsThresholdWithFutureReset reports whether the persisted
// (usedPercent, resetAt) pair indicates FRESH exhaustion. Stale or missing
// resets are treated as "the account has had time to recover" and do not
// block, matching the conservative "never skip on missing data" rule used
// elsewhere in the rotation pipeline.
func exceedsThresholdWithFutureReset(account map[string]any, usedField, resetField string, threshold float64, nowUnix int64) bool {
	used, ok := account[usedField].(string)
	if !ok || strings.TrimSpace(used) == "" {
		return false
	}
	if !usedPercentAtOrAboveThreshold(used, threshold) {
		return false
	}
	resetAt, hasReset := extractStoredResetAt(account, resetField)
	if !hasReset || resetAt <= nowUnix {
		return false
	}
	return true
}

func cooldownInFuture(account map[string]any, nowUnix int64) bool {
	cooldownUntil, hasCooldown := extractCooldownUntil(account)
	return hasCooldown && cooldownUntil > nowUnix
}

// extractStoredResetAt returns the int64 stored under fieldName in account.
// Used both to read the persisted cooldownUntil marker and to validate
// candidate eligibility (threshold usage only blocks while the recorded
// reset window is still in the future).
func extractStoredResetAt(account map[string]any, fieldName string) (int64, bool) {
	raw, ok := account[fieldName]
	if !ok || raw == nil {
		return 0, false
	}

	parsed, ok := valueToInt64(raw)
	if !ok {
		return 0, false
	}

	return parsed, true
}

func extractCooldownUntil(account map[string]any) (int64, bool) {
	return extractStoredResetAt(account, "cooldownUntil")
}

// updateOpenAIAuthFile keeps OpenCode's `auth.json` OAuth tuple in sync with the
// selected account. Every source field present in the account is propagated to
// BOTH the supported flat `openai.*` representation AND the nested `openai`
// object when those representations already exist; fields absent from the
// source are removed from both representations so a previous active account's
// stale values never survive a switch. Auth providers other than `openai` and
// any non-OAuth fields under `openai` (such as custom workspace metadata) are
// preserved untouched.
//
// The OpenCode auth.json is intentionally written WITHOUT the proper-lockfile
// protocol Pi uses: OpenCode is the source of truth the CLI reads on every
// invocation, and the activation flow is the only writer here. Co-activation
// with Pi is handled by activateAccount, which validates the full OAuth
// metadata before either file is modified so a successful run produces one
// coherent selected account across both stores.
func updateOpenAIAuthFile(authFile string, account map[string]any) error {
	access, ok := account["access"].(string)
	if !ok || strings.TrimSpace(access) == "" {
		return errors.New("account data missing non-empty access token")
	}

	accountID, hasAccountID := stringField(account, "accountId")
	refresh, hasRefresh := stringField(account, "refresh")
	typ, hasType := stringField(account, "type")
	hasType = hasType && strings.TrimSpace(typ) != ""

	expires, hasExpires, expiresErr := int64FieldExact(account, "expires")
	if expiresErr != nil {
		return fmt.Errorf("account expires: %w", expiresErr)
	}

	data, err := os.ReadFile(authFile)
	if err != nil {
		return fmt.Errorf("reading auth file: %w", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return fmt.Errorf("parsing auth file JSON: %w", err)
	}

	// Flat representation: openai.access / openai.accountId / openai.refresh /
	// openai.expires / openai.type. Each is assigned when the source has the
	// field; deleted otherwise so stale values from a previous switch are
	// removed.
	payload["openai.access"] = access
	writeOptionalString(payload, "openai.accountId", accountID, hasAccountID)
	writeOptionalString(payload, "openai.refresh", refresh, hasRefresh)
	if hasExpires {
		// Preserve the integer literal by writing through json.Number; this
		// keeps `expires` as a JSON number of milliseconds with no truncation.
		payload["openai.expires"] = json.Number(strconv.FormatInt(expires, 10))
	} else {
		delete(payload, "openai.expires")
	}
	writeOptionalString(payload, "openai.type", typ, hasType)

	// Nested representation: openai.{access,accountId,refresh,expires,type}.
	// Only updated when the nested object already exists; creating a new
	// nested object from scratch would silently invent a second representation
	// the rest of the codebase does not expect.
	if rawOpenAI, hasNested := payload["openai"]; hasNested {
		if openAIMap, ok := rawOpenAI.(map[string]any); ok {
			openAIMap["access"] = access
			writeOptionalString(openAIMap, "accountId", accountID, hasAccountID)
			writeOptionalString(openAIMap, "refresh", refresh, hasRefresh)
			if hasExpires {
				openAIMap["expires"] = json.Number(strconv.FormatInt(expires, 10))
			} else {
				delete(openAIMap, "expires")
			}
			writeOptionalString(openAIMap, "type", typ, hasType)
			payload["openai"] = openAIMap
		}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding auth JSON: %w", err)
	}

	if err := os.WriteFile(authFile, encoded, 0o600); err != nil {
		return fmt.Errorf("writing auth file: %w", err)
	}

	return nil
}

// stringField returns account[key] coerced to a string, reporting presence
// independently of trim semantics so the caller can decide whether an empty
// value still counts as "the source claims this field". Returns "", false when
// the field is missing or not a string.
func stringField(account map[string]any, key string) (string, bool) {
	raw, ok := account[key]
	if !ok || raw == nil {
		return "", false
	}
	s, ok := raw.(string)
	if !ok {
		return "", false
	}
	return s, true
}

// int64FieldExact returns account[key] coerced to an int64 via parseExactInt64.
// It reports presence when the key exists AND parses cleanly; an unparseable
// value (fraction, overflow, wrong type) is surfaced as an error so the caller
// can refuse the activation rather than silently dropping the field.
func int64FieldExact(account map[string]any, key string) (int64, bool, error) {
	raw, ok := account[key]
	if !ok || raw == nil {
		return 0, false, nil
	}
	n, err := parseExactInt64(raw)
	if err != nil {
		return 0, false, err
	}
	return n, true, nil
}

// writeOptionalString assigns target[key] = value when hasValue is true; when
// false, the key is deleted so a stale value from a previous switch does not
// survive into the next account. Centralized so the flat and nested
// representations stay symmetric.
func writeOptionalString(target map[string]any, key, value string, hasValue bool) {
	if hasValue {
		target[key] = value
		return
	}
	delete(target, key)
}

// -----------------------------------------------------------------------------
// Pi auth synchronization.
// -----------------------------------------------------------------------------

// expandTildePath expands a leading "~" or "~/" segment to the user's home
// directory so directory overrides like PI_CODING_AGENT_DIR=~/my-pi match Pi's
// own path semantics. Returns the input unchanged when it does not start with
// "~" or when the user home directory cannot be resolved (callers fall back to
// their home-based default in that case).
func expandTildePath(path string) (string, error) {
	if path == "" || path[0] != '~' {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return home, nil
	}
	if path[1] == '/' || path[1] == filepath.Separator {
		return filepath.Join(home, path[2:]), nil
	}
	// "~user/..." is not supported by Pi; return the input unchanged.
	return path, nil
}

// PiAuthEntry is the openai-codex credential shape stored in Pi's auth.json.
// It contains ONLY the OAuth fields Pi cares about: usage metadata
// (usedPercent, resetAt, secondary*, cooldownUntil), account identity
// (user_id, email), and unrelated account fields must never appear here.
//
// `type` is always serialized (no `omitempty`) so the JSON shape is
// unambiguous: a legacy source account that omits `type` is normalized to
// `"oauth"` by buildPiCredential rather than being written as a JSON object
// without a `type` field. Pi treats a missing `type` as an authentication
// error, so the projection must never emit one.
type PiAuthEntry struct {
	Type      string `json:"type"`
	Access    string `json:"access"`
	Refresh   string `json:"refresh"`
	Expires   int64  `json:"expires"`
	AccountID string `json:"accountId,omitempty"`
}

// parseExactInt64 parses value as a finite integer exactly representable as
// int64. It is the entry point used by the Pi projection to interpret the
// source account's `expires` value, which Pi treats as an integer number of
// milliseconds since epoch and refuses as fractional or truncated.
//
// Accepted inputs (all preserving the integer value exactly):
//
//   - json.Number with an integral decimal literal, including scientific
//     notation (`1e3`) and `.0` forms (`100.0`). Decoded via math/big so the
//     literal's full precision is checked before any int64 conversion.
//   - float64 / float32 values that are mathematically integral and within
//     int64 range; NaN and ±Inf are rejected.
//   - int, int8/16/32/64 and uint, uint8/16/32/64 within int64 range.
//
// Rejected (no truncation, no rounding) with a value-free error:
//
//   - Fractions (1.5, 0.1, "100.5").
//   - NaN, +Inf, -Inf.
//   - Values outside [math.MinInt64, math.MaxInt64].
//   - Nonnumeric types (strings, booleans, nil, maps, slices, structs).
//
// Errors do not include the offending value: callers surface them to the
// user and must not echo credential material.
func parseExactInt64(value any) (int64, error) {
	switch v := value.(type) {
	case json.Number:
		literal := v.String()
		if !json.Valid([]byte(literal)) {
			return 0, errors.New("invalid number literal")
		}
		rational, ok := new(big.Rat).SetString(literal)
		if !ok {
			return 0, errors.New("invalid number literal")
		}
		if !rational.IsInt() {
			return 0, errors.New("number literal is not an integer")
		}
		integer := rational.Num()
		if !integer.IsInt64() {
			return 0, errors.New("number literal out of int64 range")
		}
		return integer.Int64(), nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, errors.New("integer value must be finite")
		}
		// float64 cannot represent every int64 exactly. The round-trip check
		// (convert to int64 and back) catches fractions (truncation differs),
		// NaN/Inf (already filtered above), and values past the int64 range
		// (where int64() returns an implementation-defined sentinel whose
		// float64 back-conversion does not match the source).
		n := int64(v)
		if float64(n) != v {
			return 0, errors.New("integer value not representable as int64 (fractional or out of range)")
		}
		return n, nil
	case float32:
		return parseExactInt64(float64(v))
	case int:
		return int64(v), nil
	case int8:
		return int64(v), nil
	case int16:
		return int64(v), nil
	case int32:
		return int64(v), nil
	case int64:
		return v, nil
	case uint:
		if uint64(v) > math.MaxInt64 {
			return 0, errors.New("unsigned integer out of int64 range")
		}
		return int64(v), nil
	case uint8:
		return int64(v), nil
	case uint16:
		return int64(v), nil
	case uint32:
		return int64(v), nil
	case uint64:
		if v > uint64(math.MaxInt64) {
			return 0, errors.New("unsigned integer out of int64 range")
		}
		return int64(v), nil
	default:
		return 0, fmt.Errorf("unsupported type %T", value)
	}
}

// validatePiAccountFields reports the Pi OAuth fields missing from account.
// The error names field NAMES but NEVER the values: callers surface it directly
// to the user and must not include credential material in summaries. Used as a
// pre-flight check before either auth file is modified so a manual/automatic
// account activation that lacks Pi OAuth metadata leaves both files unchanged.
func validatePiAccountFields(account map[string]any) error {
	if account == nil {
		return errors.New("selected account is empty")
	}

	var missing []string

	if access, _ := account["access"].(string); strings.TrimSpace(access) == "" {
		missing = append(missing, "access")
	}
	if refresh, _ := account["refresh"].(string); strings.TrimSpace(refresh) == "" {
		missing = append(missing, "refresh")
	}

	if _, ok := account["expires"]; !ok {
		missing = append(missing, "expires")
	} else if _, err := parseExactInt64(account["expires"]); err != nil {
		return fmt.Errorf("invalid expires field (must be an integer number of milliseconds since epoch): %w", err)
	}

	if len(missing) > 0 {
		return fmt.Errorf("selected account missing required Pi OAuth fields: %s", strings.Join(missing, ", "))
	}

	// Type, when present, must be a string and OAuth-compatible. A missing or
	// empty type is tolerated so buildPiCredential can normalize it to "oauth".
	// Errors identify the field without echoing credential values.
	if rawType, ok := account["type"]; ok && rawType != nil {
		t, isString := rawType.(string)
		if !isString || (strings.TrimSpace(t) != "" && t != "oauth") {
			return errors.New("unsupported account type field (Pi requires \"oauth\")")
		}
	}

	return nil
}

// buildPiCredential projects a selected account into the openai-codex
// credential shape Pi expects. It runs validatePiAccountFields first so any
// error here means the caller must NOT touch either auth file. The returned
// struct intentionally exposes no Pi-irrelevant fields: usage metadata
// (usedPercent, resetAt, secondary*, cooldownUntil), identity metadata
// (user_id, email), and arbitrary custom fields on the source account are
// never projected.
//
// `type` is normalized to "oauth" when the source account omits it (legacy
// records written before Pi required an explicit type). validatePiAccountFields
// guarantees that any present non-empty value is already "oauth", so the
// override below only fills in the legacy default.
func buildPiCredential(account map[string]any) (PiAuthEntry, error) {
	if err := validatePiAccountFields(account); err != nil {
		return PiAuthEntry{}, err
	}

	access, _ := account["access"].(string)
	refresh, _ := account["refresh"].(string)
	// parseExactInt64 is guaranteed to succeed here: validatePiAccountFields
	// already rejected fractional/out-of-range/typed-wrong values.
	expiresMs, _ := parseExactInt64(account["expires"])

	cred := PiAuthEntry{
		Type:    "oauth",
		Access:  access,
		Refresh: refresh,
		Expires: expiresMs,
	}

	if rawType, ok := account["type"]; ok {
		if t, _ := rawType.(string); strings.TrimSpace(t) != "" {
			cred.Type = t
		}
	}

	if rawAccountID, ok := account["accountId"]; ok {
		if id, _ := rawAccountID.(string); strings.TrimSpace(id) != "" {
			cred.AccountID = id
		}
	}

	return cred, nil
}

// Pi auth synchronization lock semantics.
//
// The lock is `${auth.json}.lock` created as an ATOMIC DIRECTORY (matching
// Pi's proper-lockfile-based sync path). Acquiring the lock means `os.Mkdir`
// succeeded; releasing it means `os.Remove` ran. The directory approach is
// intentional: POSIX mkdir is atomic, so concurrent writers cannot both
// observe "lock acquired".
//
// We retry a bounded number of times (10 attempts × 20ms = 200ms total) and
// then give up with a clear error. We NEVER delete a stale lock we did not
// create: a stale directory is treated as a concurrent writer that owns the
// file, and the activation surfaces an error rather than racing ahead.
//
// Release failures are NOT silently swallowed: a stuck lock directory will
// block subsequent activations, so the error is propagated and surfaced.
const (
	piLockMaxAttempts = 10
	piLockRetryDelay  = 20 * time.Millisecond
	piAuthDirPerm     = 0o700
	piAuthFilePerm    = 0o600
	piAuthLockDirPerm = 0o700
)

// acquirePiLock atomically creates lockPath as a directory matching Pi's
// proper-lockfile sync path. It returns nil on success; on failure it returns
// an error that names the lock path and the number of attempts so the caller
// can surface a clear "Pi auth is busy" message without touching the auth
// file. The lock is NEVER deleted speculatively: an existing lock directory
// means another writer owns the file, and that ownership is respected.
func acquirePiLock(lockPath string) error {
	for attempt := 1; attempt <= piLockMaxAttempts; attempt++ {
		err := os.Mkdir(lockPath, piAuthLockDirPerm)
		if err == nil {
			return nil
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("acquiring Pi auth lock at %s: %w", lockPath, err)
		}
		time.Sleep(piLockRetryDelay)
	}
	return fmt.Errorf("could not acquire Pi auth lock at %s after %d attempts (%dms each)",
		lockPath, piLockMaxAttempts, piLockRetryDelay/time.Millisecond)
}

// releasePiLock removes the lock directory created by acquirePiLock. The
// caller decides how to surface the error (typically by returning it from
// updatePiAuthFile): a leftover lock directory will block subsequent
// activations, so the failure is reported rather than hidden.
func releasePiLock(lockPath string) error {
	if err := os.Remove(lockPath); err != nil {
		return fmt.Errorf("releasing Pi auth lock at %s: %w", lockPath, err)
	}
	return nil
}

func combinePiAuthWriteAndReleaseErrors(writeErr, releaseErr error) error {
	return errors.Join(writeErr, releaseErr)
}

// updatePiAuthFile replaces ONLY the top-level "openai-codex" entry in Pi's
// auth.json, preserving every other provider and their nested values. The
// file is created with restrictive permissions (parent dir 0700 when freshly
// created, file 0600); on POSIX an existing file is tightened to 0600 BEFORE
// the write so a permission failure aborts the update without mutating
// credentials.
//
// Concurrency is handled with the proper-lockfile protocol (see acquirePiLock):
// the parent directory and a missing `{}` file are prepared BEFORE the lock
// attempt, then the lock is held across read/modify/write and released on
// return. Write and release failures are joined so neither credential-write
// failures nor compromised lock ownership are hidden.
func updatePiAuthFile(piAuthFile string, cred PiAuthEntry) error {
	if err := ensurePiAuthParent(filepath.Dir(piAuthFile)); err != nil {
		return err
	}
	if err := ensurePiAuthFile(piAuthFile); err != nil {
		return err
	}

	lockPath := piAuthFile + ".lock"
	if err := acquirePiLock(lockPath); err != nil {
		return err
	}

	writeErr := writePiAuthFileUnderLock(piAuthFile, cred)
	releaseErr := releasePiLock(lockPath)
	return combinePiAuthWriteAndReleaseErrors(writeErr, releaseErr)
}

// writePiAuthFileUnderLock performs the read/modify/write cycle while the
// proper-lockfile lock is held. Splitting this from updatePiAuthFile keeps the
// lock acquisition/release and the I/O boundaries easy to read at a glance.
func writePiAuthFileUnderLock(piAuthFile string, cred PiAuthEntry) error {
	payload, originalBytes, err := loadPiAuthFile(piAuthFile)
	if err != nil {
		return err
	}

	payload["openai-codex"] = cred

	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding Pi auth JSON: %w", err)
	}

	// Tighten permissions on POSIX BEFORE writing. A chmod failure (e.g. an
	// immutable file, a path we cannot stat) aborts the update so the
	// credential content is never written under looser permissions than the
	// documented 0600 contract.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(piAuthFile, piAuthFilePerm); err != nil {
			return fmt.Errorf("tightening Pi auth file permissions: %w", err)
		}
	}

	if err := os.WriteFile(piAuthFile, encoded, piAuthFilePerm); err != nil {
		// Best-effort restore of the original content so a write failure does
		// not leave the file truncated or empty.
		if originalBytes != nil {
			_ = os.WriteFile(piAuthFile, originalBytes, piAuthFilePerm)
		}
		return fmt.Errorf("writing Pi auth file: %w", err)
	}

	return nil
}

// ensurePiAuthParent creates the Pi auth parent directory with 0700 if it
// does not already exist. An EXISTING parent directory is left untouched: we
// do not claim to tighten a directory we did not create, both to avoid
// surprise-mode changes on shared paths and because tightening a pre-existing
// directory's permissions could break unrelated tools sharing it.
func ensurePiAuthParent(parentDir string) error {
	if parentDir == "" {
		return errors.New("Pi auth file path has no parent directory")
	}
	info, err := os.Stat(parentDir)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("Pi auth parent path is not a directory: %s", parentDir)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking Pi auth parent directory: %w", err)
	}
	if err := os.MkdirAll(parentDir, piAuthDirPerm); err != nil {
		return fmt.Errorf("creating Pi auth directory: %w", err)
	}
	return nil
}

// ensurePiAuthFile ensures piAuthFile exists as a regular file before the lock
// is acquired. A missing file is initialized to `{}` with 0600 via
// O_CREATE|O_EXCL so a concurrent writer cannot clobber an existing file with
// an empty body. An EEXIST race against another creator is treated as success:
// whichever writer created the file owns the contents and the lock held later
// in the update cycle still guarantees read/modify/write consistency. An
// existing file is left as-is (the chmod-before-write tightening happens
// under the lock).
func ensurePiAuthFile(piAuthFile string) error {
	info, err := os.Stat(piAuthFile)
	if err == nil {
		if info.IsDir() {
			return fmt.Errorf("Pi auth path is a directory: %s", piAuthFile)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking Pi auth file: %w", err)
	}
	f, err := os.OpenFile(piAuthFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, piAuthFilePerm)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return fmt.Errorf("initializing Pi auth file: %w", err)
	}
	if _, err := f.Write([]byte("{}")); err != nil {
		_ = f.Close()
		return fmt.Errorf("initializing Pi auth file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing Pi auth file: %w", err)
	}
	return nil
}

// loadPiAuthFile reads piAuthFile as a JSON object. A missing file yields an
// empty object (the caller is expected to have called ensurePiAuthFile first,
// but we still tolerate the race because the lock guarantees correctness on
// the read/modify/write cycle). An empty file is rejected as malformed
// (matches Pi's JSON.parse behavior). Trailing JSON values or content after
// the first top-level object are also rejected (e.g. "{} {}", "{\"a\":1} junk")
// to keep the file unambiguously a single JSON object. The original bytes are
// returned so the caller can restore them on a later write failure.
//
// The file is decoded with json.Decoder.UseNumber so unrelated providers can
// carry large integers beyond float64's exact integer range (~2^53) and still
// be remarshaled with their original numeric value preserved.
func loadPiAuthFile(piAuthFile string) (map[string]any, []byte, error) {
	data, err := os.ReadFile(piAuthFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, nil, nil
		}
		return nil, nil, fmt.Errorf("reading Pi auth file: %w", err)
	}

	if len(bytes.TrimSpace(data)) == 0 {
		return nil, data, errors.New("Pi auth file is empty (malformed JSON)")
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, data, fmt.Errorf("parsing Pi auth JSON: %w", err)
	}
	if payload == nil {
		return nil, data, errors.New("Pi auth file must be a JSON object")
	}

	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, data, errors.New("Pi auth file must contain a single JSON object")
	}
	return payload, data, nil
}

// activateAccount centralizes the account-activation side effects: it updates
// OpenCode's auth.json and Pi's auth.json (when Pi sync is enabled) for the
// selected account. Registration/listing paths MUST NOT call this; it is only
// invoked from the rotation pipeline and from `use`.
//
// Validation runs before either file is modified so a manual/automatic account
// activation that lacks Pi OAuth metadata leaves both auth files unchanged. A
// successful OpenCode update followed by a failed Pi write surfaces as a clear
// partial-sync error that names both stores but never the credential values.
//
// Pi sync is opt-in via cfg.PiAuthFile: an explicitly empty string skips Pi
// sync so existing unit tests that pre-date Pi sync stay isolated from the
// real ~/.pi/agent/auth.json.
func activateAccount(cfg config, account map[string]any) error {
	// A provider that is not installed is skipped instead of failing the whole
	// activation. OpenCode counts as installed when its auth file exists, since
	// updateOpenAIAuthFile only ever rewrites an existing file. Pi counts as
	// installed when its agent directory exists: updatePiAuthFile may create
	// auth.json inside it (Pi does the same on login), but creating the whole
	// tree would fabricate a Pi install that is not there.
	openCodeEnabled := strings.TrimSpace(cfg.AuthFile) != "" && regularFileExists(cfg.AuthFile)
	piSyncEnabled := strings.TrimSpace(cfg.PiAuthFile) != "" && directoryExists(filepath.Dir(cfg.PiAuthFile))

	if !openCodeEnabled && !piSyncEnabled {
		return errNeitherProviderAvailable
	}

	if piSyncEnabled {
		if _, err := buildPiCredential(account); err != nil {
			return err
		}
	}

	if openCodeEnabled {
		if err := updateOpenAIAuthFile(cfg.AuthFile, account); err != nil {
			return err
		}
	}

	if !piSyncEnabled {
		return nil
	}

	cred, _ := buildPiCredential(account) // already validated above
	if err := updatePiAuthFile(cfg.PiAuthFile, cred); err != nil {
		if !openCodeEnabled {
			return fmt.Errorf("Pi auth sync failed: %w", err)
		}
		return fmt.Errorf("OpenCode auth updated but Pi auth sync failed: %w", err)
	}

	return nil
}

// regularFileExists reports whether path names an existing non-directory file.
// Any stat error (missing, permission denied, broken symlink) counts as absent
// so callers treat an unusable provider path exactly like an uninstalled one.
func regularFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// directoryExists reports whether path names an existing directory, with the
// same "any error means absent" rule as regularFileExists.
func directoryExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// -----------------------------------------------------------------------------
// Inbound Pi reconciliation (best-effort, default command only).
// -----------------------------------------------------------------------------

// errPiAuthMissing is the sentinel for a missing Pi auth file. Callers
// treat it as a clean skip without surfacing an error.
var errPiAuthMissing = errors.New("Pi auth file is missing")

// readPiAuthUnderLock reads Pi's auth.json under the proper-lockfile protocol
// used by the activation path. READ-ONLY: never creates the parent dir, the
// auth file, or writes to it. A missing file returns errPiAuthMissing.
//
// The lock is released explicitly; read or release failures are joined so
// neither is hidden. When the combined error is non-nil the payload is NOT
// returned: a compromised release means another writer could be racing.
func readPiAuthUnderLock(piAuthFile string) (map[string]any, error) {
	if strings.TrimSpace(piAuthFile) == "" {
		return nil, errPiAuthMissing
	}

	info, err := os.Stat(piAuthFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errPiAuthMissing
		}
		return nil, fmt.Errorf("checking Pi auth file: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("Pi auth path is a directory: %s", piAuthFile)
	}

	lockPath := piAuthFile + ".lock"
	if err := acquirePiLock(lockPath); err != nil {
		return nil, fmt.Errorf("acquiring Pi auth lock for read: %w", err)
	}

	payload, _, readErr := loadPiAuthFile(piAuthFile)
	releaseErr := releasePiLock(lockPath)
	if combined := combinePiAuthWriteAndReleaseErrors(readErr, releaseErr); combined != nil {
		return nil, combined
	}
	return payload, nil
}

// parsePiOpenAICodexEntry validates the Pi openai-codex credential with the
// same strict OAuth parsing as buildPiCredential (access/refresh/expires/
// type/accountId, exact-int64 expires) and projects it to map[string]any for
// accountWithUsage/persistOpenAIAccount. Errors never echo credential values.
func parsePiOpenAICodexEntry(credMap map[string]any) (map[string]any, error) {
	cred, err := buildPiCredential(credMap)
	if err != nil {
		return nil, err
	}

	account := map[string]any{
		"type":    cred.Type,
		"access":  cred.Access,
		"refresh": cred.Refresh,
		"expires": cred.Expires,
	}
	if strings.TrimSpace(cred.AccountID) != "" {
		account["accountId"] = cred.AccountID
	}
	return account, nil
}

// reconcilePiInbound best-effort reconciles a newer Pi openai-codex
// credential into the saved store and (when matched is current) into
// OpenCode's auth.json. Invoked ONLY from runDefaultCommand; `accounts`/
// `list`/`use`/`config` MUST NOT call it.
//
// Returns (account, usage, changed, error). account/usage are returned only
// on an active match; changed = true iff a store or OpenCode mutation
// occurred. error is non-nil ONLY for persistence/OpenCode write failures
// AFTER a valid newer match; clean no-ops return nil. Match key is user_id
// (accountId is metadata, never a match key); skip when Pi access == current
// OC access (reuse usage); saved expires must be a valid exact-int64.
//
// Rules: non-active match imports ONLY if Pi expires > saved (strictly).
// Active match (Pi user_id == current OC user_id): no-op when current OC
// expires is missing/invalid; never downgrade; self-heal (Pi == saved but >
// current OC: update ONLY active OC); otherwise persist store FIRST then
// active OC (a store failure surfaces an error; the next run self-heals
// via the equal-saved/newer-current branch). Merge ONLY Pi OAuth fields
// (type=oauth, access, refresh, expires, optional accountId); user_id,
// email, custom metadata preserved.
func reconcilePiInbound(
	ctx context.Context,
	cfg config,
	account map[string]any,
	usage usageWindow,
	fiveHourToggle bool,
) (map[string]any, usageWindow, bool, error) {
	if strings.TrimSpace(cfg.PiAuthFile) == "" {
		return account, usage, false, nil
	}

	piPayload, err := readPiAuthUnderLock(cfg.PiAuthFile)
	if err != nil {
		return account, usage, false, nil
	}

	rawCred, present := piPayload["openai-codex"]
	if !present || rawCred == nil {
		return account, usage, false, nil
	}
	credMap, ok := rawCred.(map[string]any)
	if !ok {
		return account, usage, false, nil
	}

	piInfo := &piAuthInfo{Payload: piPayload, CredMap: credMap}
	return reconcilePiInboundFromPayload(ctx, cfg, piInfo, account, usage, fiveHourToggle)
}

// reconcilePiInboundFromPayload accepts an already-loaded Pi auth payload
// (typically returned by loadPiAuthIfUsable during the dispatcher) so Pi's
// auth.json is read under the lock exactly once per run. Semantics match
// reconcilePiInbound exactly.
func reconcilePiInboundFromPayload(
	ctx context.Context,
	cfg config,
	piInfo *piAuthInfo,
	account map[string]any,
	usage usageWindow,
	fiveHourToggle bool,
) (map[string]any, usageWindow, bool, error) {
	if piInfo == nil {
		return account, usage, false, nil
	}

	piAccount, err := parsePiOpenAICodexEntry(piInfo.CredMap)
	if err != nil {
		return account, usage, false, nil
	}

	piAccess, _ := piAccount["access"].(string)
	piExpires, _ := piAccount["expires"].(int64)

	// Validate Pi access. Reuse the current usage when tokens match so we
	// never make a second API call for the same token (the API response is
	// deterministic per-token).
	piUsage := usage
	currentAccess, _ := account["access"].(string)
	if strings.TrimSpace(piAccess) != strings.TrimSpace(currentAccess) {
		fetched, fetchErr := fetchUsageWindow(ctx, cfg.HTTPClient, cfg.UsageURL, piAccess)
		if fetchErr != nil {
			return account, usage, false, nil
		}
		piUsage = fetched
	}

	piUserID := strings.TrimSpace(piUsage.UserID)
	if piUserID == "" {
		return account, usage, false, nil
	}

	// Match Pi user_id into the normalized saved accounts store.
	store, err := readAccountsStore(cfg.AccountsFile)
	if err != nil {
		return account, usage, false, nil
	}
	store = normalizeAccountsStoreByUserID(store)

	saved, ok := store[piUserID]
	if !ok {
		return account, usage, false, nil
	}

	// Validate saved expires is an exact integer. Missing/invalid -> no-op.
	savedExpiresRaw, hasExpires := saved["expires"]
	if !hasExpires || savedExpiresRaw == nil {
		return account, usage, false, nil
	}
	savedExpires, err := parseExactInt64(savedExpiresRaw)
	if err != nil {
		return account, usage, false, nil
	}

	// Active match: Pi API user_id == current OpenCode API user_id.
	currentUserID := strings.TrimSpace(usage.UserID)
	isActiveMatch := currentUserID != "" && currentUserID == piUserID

	// Active-match extra check: inspect current OC expires to enforce the
	// never-downgrade rule and to detect self-heal.
	var currentOpenCodeExpires int64
	if isActiveMatch {
		rawCurrent, has := account["expires"]
		if !has || rawCurrent == nil {
			return account, usage, false, nil // cannot compare
		}
		n, perr := parseExactInt64(rawCurrent)
		if perr != nil {
			return account, usage, false, nil
		}
		currentOpenCodeExpires = n
		// Never downgrade either source.
		if currentOpenCodeExpires >= piExpires || savedExpires > piExpires {
			return account, usage, false, nil
		}
	} else if piExpires <= savedExpires {
		// Non-active: import only when Pi expires is STRICTLY greater.
		return account, usage, false, nil
	}

	// Build the merged saved entry: preserve identity/custom metadata,
	// replace OAuth fields with the newer Pi values.
	merged := copyAccountData(saved)
	merged["type"] = "oauth"
	merged["access"] = piAccess
	merged["refresh"] = piAccount["refresh"]
	merged["expires"] = piExpires
	if aid, ok := piAccount["accountId"].(string); ok && strings.TrimSpace(aid) != "" {
		merged["accountId"] = aid
	} else {
		delete(merged, "accountId")
	}

	piUsageNorm, piFiveHourMode := normalizeUsageForToggle(piUsage, fiveHourToggle)
	if savedEmail, ok := saved["email"].(string); ok && strings.TrimSpace(savedEmail) != "" {
		// The default pipeline persists the returned usage once more; carry
		// the saved email through that final accountWithUsage call as well.
		piUsageNorm.Email = savedEmail
	}
	mergedWithUsage := accountWithUsage(merged, piUsageNorm, piFiveHourMode, rotationThresholds{FiveHour: cfg.FiveHourThreshold, Weekly: cfg.WeeklyThreshold})

	if isActiveMatch {
		// Self-heal: Pi expires == saved expires (already in store) but Pi >
		// current OpenCode expires; update ONLY active OpenCode.
		if piExpires == savedExpires {
			if err := updateOpenAIAuthFile(cfg.AuthFile, mergedWithUsage); err != nil {
				return mergedWithUsage, piUsageNorm, true, fmt.Errorf("self-healing active OpenCode from Pi: %w", err)
			}
			return mergedWithUsage, piUsageNorm, true, nil
		}
		// Full forward path: persist store FIRST so a store write failure
		// leaves OpenCode unchanged; the next run self-heals.
		if err := persistOpenAIAccount(cfg.AccountsFile, mergedWithUsage); err != nil {
			return account, usage, false, fmt.Errorf("reconciling Pi into saved store: %w", err)
		}
		if err := updateOpenAIAuthFile(cfg.AuthFile, mergedWithUsage); err != nil {
			// Store updated, OpenCode not: surface the partial sync so the
			// next run's self-heal retries OpenCode.
			return mergedWithUsage, piUsageNorm, true, fmt.Errorf("reconciling active OpenCode from Pi after store update: %w", err)
		}
		return mergedWithUsage, piUsageNorm, true, nil
	}

	// Non-active (store-only) match: keep the original OpenCode account +
	// usage; stdout pipeline stays untouched.
	if err := persistOpenAIAccount(cfg.AccountsFile, mergedWithUsage); err != nil {
		return account, usage, false, fmt.Errorf("reconciling Pi into saved store: %w", err)
	}
	return account, usage, true, nil
}

func valueToString(value any) (string, error) {
	switch v := value.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return "", errors.New("used_percent value is empty")
		}
		return v, nil
	case json.Number:
		if strings.TrimSpace(v.String()) == "" {
			return "", errors.New("used_percent value is empty")
		}
		return v.String(), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32), nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case uint64:
		return strconv.FormatUint(v, 10), nil
	default:
		return "", fmt.Errorf("used_percent has unsupported type %T", value)
	}
}
