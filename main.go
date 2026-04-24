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
const usageRotationThreshold = 80.0

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
		default:
			return fmt.Errorf("unknown command %q (available: accounts, list)", command)
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

	if usedPercentAtOrAboveThreshold(usage.UsedPercent, usageRotationThreshold) {
		store, err := readAccountsStore(cfg.AccountsFile)
		if err != nil {
			return err
		}

		if nextAccount, ok := selectEligibleAlternateAccount(store, usage.UserID, time.Now().Unix()); ok {
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
	for _, userID := range keys {
		account := copyAccountData(store[userID])
		access, _ := account["access"].(string)
		current := strings.TrimSpace(currentToken) != "" && access == currentToken

		storedUserID, _ := account["user_id"].(string)
		if strings.TrimSpace(storedUserID) == "" {
			storedUserID = userID
		}

		row := accountUsageRow{
			Current: current,
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

	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Current != rows[j].Current {
			return rows[i].Current
		}

		emailI := strings.ToLower(rows[i].Email)
		emailJ := strings.ToLower(rows[j].Email)
		if emailI != emailJ {
			return emailI < emailJ
		}

		return strings.ToLower(rows[i].UserID) < strings.ToLower(rows[j].UserID)
	})

	if err := printAccountsTable(stdout, rows); err != nil {
		return err
	}

	if successCount == 0 {
		return errors.New("failed to fetch usage for all saved accounts")
	}

	return nil
}

func printAccountsTable(stdout io.Writer, rows []accountUsageRow) error {
	if _, err := fmt.Fprintln(stdout, "CURRENT  EMAIL  USED%  RESET"); err != nil {
		return err
	}

	for _, row := range rows {
		marker := ""
		if row.Current {
			marker = "*"
		}

		line := fmt.Sprintf("%-7s  %s  %s  %s", marker, row.Email, row.UsedPercent, row.ResetDisplay)
		if row.Err != nil {
			line += "  [ERR: " + row.Err.Error() + "]"
		}

		if _, err := fmt.Fprintln(stdout, line); err != nil {
			return err
		}
	}

	return nil
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

	if usedPercentAtOrAboveThreshold(usage.UsedPercent, usageRotationThreshold) {
		if usage.ResetAt != nil {
			accountWithUsage["cooldownUntil"] = *usage.ResetAt
		}
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

	resetAt, err := extractResetAt(primaryWindow)
	if err != nil {
		return usageWindow{}, err
	}
	window.ResetAt = resetAt

	return window, nil
}

func extractResetAt(primaryWindow map[string]any) (*int64, error) {
	rawResetAt, ok := primaryWindow["reset_at"]
	if !ok || rawResetAt == nil {
		return nil, nil
	}

	parsed, ok := valueToInt64(rawResetAt)
	if !ok {
		return nil, fmt.Errorf("usage JSON key \"rate_limit.primary_window.reset_at\" has unsupported type %T", rawResetAt)
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

		if cooldownUntil, hasCooldown := extractCooldownUntil(account); hasCooldown && cooldownUntil > nowUnix {
			continue
		}

		selected := copyAccountData(account)
		selected["user_id"] = storedUserID
		return selected, true
	}

	return nil, false
}

func extractCooldownUntil(account map[string]any) (int64, bool) {
	raw, ok := account["cooldownUntil"]
	if !ok || raw == nil {
		return 0, false
	}

	parsed, ok := valueToInt64(raw)
	if !ok {
		return 0, false
	}

	return parsed, true
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
	}

	rawOpenAI, hasNested := payload["openai"]
	if hasNested {
		if openAIMap, ok := rawOpenAI.(map[string]any); ok {
			openAIMap["access"] = access
			if hasAccountID {
				openAIMap["accountId"] = accountID
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
