package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const defaultUsageURL = "https://chatgpt.com/backend-api/wham/usage"

// usageWeeklyThreshold gates the WEEKLY usage window. It is always applied to
// `secondary_window` when 5h mode is active, and to `primary_window` when 5h
// mode is inactive (or `secondary_window` is missing/null). When the active
// account's weekly usage reaches (or exceeds) this percentage, the default
// command considers it exhausted and attempts to rotate to a saved alternate.
const usageWeeklyThreshold = 98.0

// usageFiveHourThreshold gates the 5-HOUR usage window. It is applied to
// `primary_window` only when 5h mode is active AND `secondary_window` is
// present.
const usageFiveHourThreshold = 80.0

type config struct {
	AuthFile        string
	AccountsFile    string
	ConfigFile      string
	FiveHourEnabled bool
	UsageURL        string
	HTTPClient      *http.Client
	Now             func() time.Time
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
// returns both the canonical usage every consumer (persistence, rotation,
// cooldown, display, stdout) should see and the effective fiveHourMode.
//
// Contract (matches the README/SKILL map):
//
//   - 5h ON + secondary present: dual mode is preserved. Primary keeps the
//     5h value (rate_limit.primary_window) and secondary keeps the weekly
//     value (rate_limit.secondary_window). fiveHourMode is true.
//   - 5h ON + secondary missing/null: primary is retained as the weekly
//     fallback. Secondary fields stay empty and fiveHourMode is false.
//   - 5h OFF + secondary present: the 5h primary data is fully ignored. The
//     weekly secondary value is PROMOTED into the canonical primary fields
//     (UsedPercent / ResetAt) and the secondary fields are CLEARED so the
//     downstream pipeline only ever sees the weekly contract. fiveHourMode
//     is false.
//   - 5h OFF + secondary missing/null: primary is retained as the weekly
//     fallback. fiveHourMode is false.
//
// Centralizing this here means callers in runDefaultCommand and
// runAccountsCommand no longer carry the OFF-mode promotion logic inline.
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

func main() {
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
	if err == nil {
		authFile = filepath.Join(home, ".local", "share", "opencode", "auth.json")
		accountsFile = filepath.Join(home, ".local", "share", "codex-usage-cli", "openai-accounts.json")
		configFile = filepath.Join(home, ".local", "share", "codex-usage-cli", "config.json")
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

	return config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		ConfigFile:   configFile,
		UsageURL:     defaultUsageURL,
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

	if len(args) > 0 {
		command := strings.ToLower(strings.TrimSpace(args[0]))
		switch command {
		case "accounts", "list":
			return runAccountsCommand(ctx, cfg, stdout)
		case "use":
			if len(args) < 2 {
				return errors.New("use command requires an account identifier (index, user_id, or email)")
			}
			return runUseCommand(cfg, stdout, strings.TrimSpace(args[1]))
		default:
			return fmt.Errorf("unknown command %q (available: accounts, list, use <id>, config <feature> [on|off])", command)
		}
	}

	return runDefaultCommand(ctx, cfg, stdout)
}

func runDefaultCommand(ctx context.Context, cfg config, stdout io.Writer) error {

	account, err := readOpenAIAccount(cfg.AuthFile)
	if err != nil {
		return err
	}

	token, _ := account["access"].(string)
	usage, err := fetchUsageWindow(ctx, cfg.HTTPClient, cfg.UsageURL, token)
	if err != nil {
		return err
	}

	// Apply the 5h toggle ONCE here so persistence, rotation, cooldown and
	// stdout all observe the canonical contract. With 5h off and a dual
	// response this promotes the weekly secondary into the primary fields
	// and clears the secondary fields.
	usage, fiveHourMode := normalizeUsageForToggle(usage, cfg.FiveHourEnabled)

	if err := ensureCurrentAccountRegistered(cfg.AccountsFile, account, usage, fiveHourMode); err != nil {
		return err
	}

	primaryExhausted := false
	secondaryExhausted := false
	if fiveHourMode {
		primaryExhausted = usedPercentAtOrAboveThreshold(usage.UsedPercent, usageFiveHourThreshold)
		secondaryExhausted = usedPercentAtOrAboveThreshold(usage.SecondaryUsedPercent, usageWeeklyThreshold)
	} else {
		primaryExhausted = usedPercentAtOrAboveThreshold(usage.UsedPercent, usageWeeklyThreshold)
	}

	if primaryExhausted || secondaryExhausted {
		store, err := readAccountsStore(cfg.AccountsFile)
		if err != nil {
			return err
		}

		if nextAccount, ok := selectEligibleAlternateAccount(store, usage.UserID, cfg.Now().Unix(), fiveHourMode); ok {
			if err := updateOpenAIAuthFile(cfg.AuthFile, nextAccount); err != nil {
				return err
			}
		}
	}

	_, err = io.WriteString(stdout, usage.UsedPercent)
	return err
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

func runAccountsCommand(ctx context.Context, cfg config, stdout io.Writer) error {
	currentAccount, err := readOpenAIAccount(cfg.AuthFile)
	if err != nil {
		return err
	}

	currentToken, _ := currentAccount["access"].(string)

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

		usage, fetchErr := fetchUsageWindow(ctx, cfg.HTTPClient, cfg.UsageURL, access)
		if fetchErr != nil {
			row.Email = accountEmailFallback(account, row.UserID)
			row.UsedPercent = "ERR"
			row.ResetDisplay = "-"
			// See comment above: error rows must not determine table mode.
			row.Err = fetchErr
			rows = append(rows, row)
			continue
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

		updated := accountWithUsage(account, usage, fiveHourMode)
		if persistErr := persistOpenAIAccount(cfg.AccountsFile, updated); persistErr != nil {
			row.Err = persistErr
		} else {
			successCount++
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

func runUseCommand(cfg config, stdout io.Writer, identifier string) error {
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

	if err := updateOpenAIAuthFile(cfg.AuthFile, target); err != nil {
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
		return errors.New("config command requires a feature name (currently supported: 5h)")
	}

	feature := strings.ToLower(strings.TrimSpace(args[0]))
	switch feature {
	case "5h":
		return runConfigFiveHour(cfg, stdout, args[1:])
	default:
		return fmt.Errorf("unknown config feature %q (currently supported: 5h)", feature)
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

func ensureCurrentAccountRegistered(accountsFile string, account map[string]any, usage usageWindow, fiveHourMode bool) error {
	return persistOpenAIAccount(accountsFile, accountWithUsage(account, usage, fiveHourMode))
}

func accountWithUsage(account map[string]any, usage usageWindow, fiveHourMode bool) map[string]any {
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
		primaryExhausted := usedPercentAtOrAboveThreshold(usage.UsedPercent, usageFiveHourThreshold) && usage.ResetAt != nil
		secondaryExhausted := usedPercentAtOrAboveThreshold(usage.SecondaryUsedPercent, usageWeeklyThreshold) && usage.SecondaryResetAt != nil
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
		if usedPercentAtOrAboveThreshold(usage.UsedPercent, usageWeeklyThreshold) && usage.ResetAt != nil {
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
		if msg != "" {
			return usageWindow{}, fmt.Errorf("usage endpoint returned %d: %s", resp.StatusCode, msg)
		}
		return usageWindow{}, fmt.Errorf("usage endpoint returned %d", resp.StatusCode)
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

func selectEligibleAlternateAccount(store map[string]map[string]any, currentUserID string, nowUnix int64, fiveHourMode bool) (map[string]any, bool) {
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
		if !candidateEligible(account, nowUnix, fiveHourMode) {
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
func candidateEligible(account map[string]any, nowUnix int64, fiveHourMode bool) bool {
	hasSecondary := candidateHasSecondaryFields(account)

	switch {
	case fiveHourMode && hasSecondary:
		if exceedsThresholdWithFutureReset(account, "usedPercent", "resetAt", usageFiveHourThreshold, nowUnix) {
			return false
		}
		if exceedsThresholdWithFutureReset(account, "secondaryUsedPercent", "secondaryResetAt", usageWeeklyThreshold, nowUnix) {
			return false
		}
		return !cooldownInFuture(account, nowUnix)

	case fiveHourMode && !hasSecondary:
		// Weekly-only persisted entry under toggle ON: treat primary as weekly.
		if exceedsThresholdWithFutureReset(account, "usedPercent", "resetAt", usageWeeklyThreshold, nowUnix) {
			return false
		}
		return !cooldownInFuture(account, nowUnix)

	case !fiveHourMode && hasSecondary:
		// Stale dual entry under toggle OFF. The stored `cooldownUntil` may have
		// been derived partly from the now-ignored 5h primary, so it is treated
		// as unreliable: ignore it. Only the persisted secondary weekly data
		// blocks. Missing secondary fields are conservative/eligible.
		return !exceedsThresholdWithFutureReset(account, "secondaryUsedPercent", "secondaryResetAt", usageWeeklyThreshold, nowUnix)

	default: // !fiveHourMode && !hasSecondary
		if exceedsThresholdWithFutureReset(account, "usedPercent", "resetAt", usageWeeklyThreshold, nowUnix) {
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

func updateOpenAIAuthFile(authFile string, account map[string]any) error {
	access, ok := account["access"].(string)
	if !ok || strings.TrimSpace(access) == "" {
		return errors.New("account data missing non-empty access token")
	}

	accountID, hasAccountID := account["accountId"].(string)
	hasAccountID = hasAccountID && strings.TrimSpace(accountID) != ""

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

	payload["openai.access"] = access
	if hasAccountID {
		payload["openai.accountId"] = accountID
	} else {
		// Selected account lacks an accountId: clear any stale value from the
		// previous active account so downstream readers do not see a leftover
		// workspace id.
		delete(payload, "openai.accountId")
	}

	rawOpenAI, hasNested := payload["openai"]
	if hasNested {
		if openAIMap, ok := rawOpenAI.(map[string]any); ok {
			openAIMap["access"] = access
			if hasAccountID {
				openAIMap["accountId"] = accountID
			} else {
				delete(openAIMap, "accountId")
			}
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
