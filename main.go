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
	"sort"
	"strconv"
	"strings"
	"time"
)

const defaultUsageURL = "https://chatgpt.com/backend-api/wham/usage"

// usageRotationThreshold gates the only window the ChatGPT usage endpoint
// exposes now: `rate_limit.primary_window`, which is the WEEKLY usage window.
// The old 5-hour window is gone and `rate_limit.secondary_window` is reported
// as `null`, so there is no second window to track. When the active account's
// weekly usage reaches (or exceeds) this percentage, the default command
// considers it exhausted and attempts to rotate to a saved alternate.
const usageRotationThreshold = 98.0

type config struct {
	AuthFile     string
	AccountsFile string
	UsageURL     string
	HTTPClient   *http.Client
	Now          func() time.Time
}

type usageWindow struct {
	UsedPercent string
	ResetAt     *int64
	UserID      string
	Email       string
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
	if err == nil {
		authFile = filepath.Join(home, ".local", "share", "opencode", "auth.json")
		accountsFile = filepath.Join(home, ".local", "share", "codex-usage-cli", "openai-accounts.json")
	}

	if envAuth := strings.TrimSpace(os.Getenv("OPENCODE_AUTH_FILE")); envAuth != "" {
		authFile = envAuth
	}
	if envAccounts := strings.TrimSpace(os.Getenv("CODEX_USAGE_ACCOUNTS_FILE")); envAccounts != "" {
		accountsFile = envAccounts
	}

	return config{
		AuthFile:     authFile,
		AccountsFile: accountsFile,
		UsageURL:     defaultUsageURL,
		HTTPClient:   http.DefaultClient,
		Now:          time.Now,
	}
}

func run(ctx context.Context, cfg config, stdout io.Writer) error {
	return runWithArgs(ctx, cfg, stdout, nil)
}

func runWithArgs(ctx context.Context, cfg config, stdout io.Writer, args []string) error {
	if cfg.AuthFile == "" {
		return errors.New("auth file path is empty")
	}
	if cfg.UsageURL == "" {
		return errors.New("usage URL is empty")
	}
	if cfg.AccountsFile == "" {
		return errors.New("accounts file path is empty")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

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
			return fmt.Errorf("unknown command %q (available: accounts, list, use <id>)", command)
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

	if err := ensureCurrentAccountRegistered(cfg.AccountsFile, account, usage); err != nil {
		return err
	}

	weeklyExhausted := usedPercentAtOrAboveThreshold(usage.UsedPercent, usageRotationThreshold)

	if weeklyExhausted {
		store, err := readAccountsStore(cfg.AccountsFile)
		if err != nil {
			return err
		}

		if nextAccount, ok := selectEligibleAlternateAccount(store, usage.UserID, cfg.Now().Unix()); ok {
			if err := updateOpenAIAuthFile(cfg.AuthFile, nextAccount); err != nil {
				return err
			}
		}
	}

	_, err = io.WriteString(stdout, usage.UsedPercent)
	return err
}

type accountUsageRow struct {
	Current      bool
	Index        int
	UserID       string
	Email        string
	UsedPercent  string
	ResetDisplay string
	Err          error
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
			row.Err = errors.New("missing access token")
			rows = append(rows, row)
			continue
		}

		usage, fetchErr := fetchUsageWindow(ctx, cfg.HTTPClient, cfg.UsageURL, access)
		if fetchErr != nil {
			row.Email = accountEmailFallback(account, row.UserID)
			row.UsedPercent = "ERR"
			row.ResetDisplay = "-"
			row.Err = fetchErr
			rows = append(rows, row)
			continue
		}

		if usage.UserID != "" {
			row.UserID = usage.UserID
		}
		row.Email = usageEmailOrFallback(usage.Email, row.UserID)
		row.UsedPercent = usage.UsedPercent
		row.ResetDisplay = formatResetDisplay(usage.ResetAt, cfg.Now())
		if current {
			currentUserID = row.UserID
		}

		updated := accountWithUsage(account, usage)
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
	// The endpoint now reports `rate_limit.primary_window` as the WEEKLY
	// usage window; `secondary_window` is null and unused. The single pair
	// of columns is therefore labeled `WEEK%` / `WEEK-RESET` to keep the
	// table unambiguous for humans.
	headers := []string{"ID", "CURRENT", "EMAIL", "WEEK%", "WEEK-RESET"}

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

		cells := []string{
			strconv.Itoa(row.Index),
			marker,
			row.Email,
			row.UsedPercent,
			row.ResetDisplay,
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

func ensureCurrentAccountRegistered(accountsFile string, account map[string]any, usage usageWindow) error {
	return persistOpenAIAccount(accountsFile, accountWithUsage(account, usage))
}

func accountWithUsage(account map[string]any, usage usageWindow) map[string]any {
	accountWithUsage := copyAccountData(account)
	accountWithUsage["user_id"] = usage.UserID
	accountWithUsage["usedPercent"] = usage.UsedPercent
	accountWithUsage["email"] = usageEmailOrFallback(usage.Email, usage.UserID)
	if usage.ResetAt != nil {
		accountWithUsage["resetAt"] = *usage.ResetAt
	} else {
		delete(accountWithUsage, "resetAt")
	}

	// Strip any stale weekly-secondary fields persisted by older runs so the
	// store file matches the current contract.
	delete(accountWithUsage, "secondaryUsedPercent")
	delete(accountWithUsage, "secondaryResetAt")

	weeklyCooldown := usedPercentAtOrAboveThreshold(usage.UsedPercent, usageRotationThreshold) && usage.ResetAt != nil
	if weeklyCooldown {
		accountWithUsage["cooldownUntil"] = *usage.ResetAt
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

	// `secondary_window` is reported as `null` by the current ChatGPT usage
	// endpoint. The 5-hour window it used to expose is gone, so we no longer
	// read it for usage, rotation, cooldown, display, or eligibility. Any
	// shape it shows up in (null, missing, or a legacy object) is ignored.
	_ = rateLimit["secondary_window"]

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

func selectEligibleAlternateAccount(store map[string]map[string]any, currentUserID string, nowUnix int64) (map[string]any, bool) {
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

		// Skip candidates whose primary (now weekly) window is already
		// exhausted, when the store has FRESH data for it. "Fresh" means we
		// have BOTH a usedPercent above threshold AND a reset_at in the
		// future. Stored high usage with an expired or missing reset is
		// treated as stale: the account has had time to recover, so it must
		// remain eligible. Missing values are treated as "unknown" and the
		// candidate is not blocked (conservative: we never skip on missing
		// data, matching how `cooldownUntil` is only honored when set).
		if used, ok := account["usedPercent"].(string); ok && strings.TrimSpace(used) != "" {
			if usedPercentAtOrAboveThreshold(used, usageRotationThreshold) {
				if resetAt, hasReset := extractStoredResetAt(account, "resetAt"); hasReset && resetAt > nowUnix {
					continue
				}
			}
		}

		if cooldownUntil, hasCooldown := extractCooldownUntil(account); hasCooldown && cooldownUntil > nowUnix {
			continue
		}

		selected := copyAccountData(account)
		selected["user_id"] = storedUserID
		return selected, true
	}

	return nil, false
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
