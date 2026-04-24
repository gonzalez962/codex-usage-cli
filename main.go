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
	"strconv"
	"strings"
)

const defaultUsageURL = "https://chatgpt.com/backend-api/wham/usage"

type config struct {
	AuthFile     string
	AccountsFile string
	UsageURL     string
	HTTPClient   *http.Client
}

func main() {
	if err := run(context.Background(), defaultConfig(), os.Stdout); err != nil {
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
	}
}

func run(ctx context.Context, cfg config, stdout io.Writer) error {
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

	account, err := readOpenAIAccount(cfg.AuthFile)
	if err != nil {
		return err
	}

	if err := persistOpenAIAccount(cfg.AccountsFile, account); err != nil {
		return err
	}

	token, _ := account["access"].(string)
	usedPercent, err := fetchUsedPercent(ctx, cfg.HTTPClient, cfg.UsageURL, token)
	if err != nil {
		return err
	}

	_, err = io.WriteString(stdout, usedPercent)
	return err
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

	accountID, err := extractAccountID(payload)
	if err != nil {
		return nil, err
	}

	account := map[string]any{
		"accountId": accountID,
		"access":    token,
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

func extractAccountID(payload map[string]any) (string, error) {
	if raw, ok := payload["openai.accountId"]; ok {
		accountID, ok := raw.(string)
		if !ok || strings.TrimSpace(accountID) == "" {
			return "", errors.New(`auth JSON key "openai.accountId" must be a non-empty string`)
		}
		return accountID, nil
	}

	rawOpenAI, ok := payload["openai"]
	if !ok {
		return "", errors.New(`auth JSON missing key "openai.accountId"`)
	}

	openAIMap, ok := rawOpenAI.(map[string]any)
	if !ok {
		return "", errors.New(`auth JSON key "openai" is not an object`)
	}

	rawAccountID, ok := openAIMap["accountId"]
	if !ok {
		return "", errors.New(`auth JSON missing key "openai.accountId"`)
	}

	accountID, ok := rawAccountID.(string)
	if !ok || strings.TrimSpace(accountID) == "" {
		return "", errors.New(`auth JSON key "openai.accountId" must be a non-empty string`)
	}

	return accountID, nil
}

func persistOpenAIAccount(accountsFile string, account map[string]any) error {
	accountID, ok := account["accountId"].(string)
	if !ok || strings.TrimSpace(accountID) == "" {
		return errors.New("account data missing non-empty accountId")
	}

	if err := os.MkdirAll(filepath.Dir(accountsFile), 0o700); err != nil {
		return fmt.Errorf("creating accounts directory: %w", err)
	}

	store := make(map[string]map[string]any)
	if data, err := os.ReadFile(accountsFile); err == nil {
		if len(bytes.TrimSpace(data)) > 0 {
			if err := json.Unmarshal(data, &store); err != nil {
				return fmt.Errorf("parsing accounts file JSON: %w", err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading accounts file: %w", err)
	}

	copyAccount := make(map[string]any, len(account))
	for key, value := range account {
		copyAccount[key] = value
	}

	store[accountID] = copyAccount

	encoded, err := json.Marshal(store)
	if err != nil {
		return fmt.Errorf("encoding accounts JSON: %w", err)
	}

	if err := os.WriteFile(accountsFile, encoded, 0o600); err != nil {
		return fmt.Errorf("writing accounts file: %w", err)
	}

	return nil
}

func fetchUsedPercent(ctx context.Context, client *http.Client, usageURL, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return "", fmt.Errorf("building usage request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("requesting usage endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(message))
		if msg != "" {
			return "", fmt.Errorf("usage endpoint returned %d: %s", resp.StatusCode, msg)
		}
		return "", fmt.Errorf("usage endpoint returned %d", resp.StatusCode)
	}

	decoder := json.NewDecoder(resp.Body)
	decoder.UseNumber()

	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return "", fmt.Errorf("parsing usage JSON: %w", err)
	}

	usedPercent, err := extractUsedPercent(payload)
	if err != nil {
		return "", err
	}

	return usedPercent, nil
}

func extractUsedPercent(payload map[string]any) (string, error) {
	rateLimit, ok := payload["rate_limit"].(map[string]any)
	if !ok {
		return "", errors.New(`usage JSON missing object "rate_limit"`)
	}

	primaryWindow, ok := rateLimit["primary_window"].(map[string]any)
	if !ok {
		return "", errors.New(`usage JSON missing object "rate_limit.primary_window"`)
	}

	value, ok := primaryWindow["used_percent"]
	if !ok {
		return "", errors.New(`usage JSON missing key "rate_limit.primary_window.used_percent"`)
	}

	return valueToString(value)
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
