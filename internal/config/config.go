// Package config loads and validates the Zero Two (gistbot) runtime
// configuration from environment variables supplied by systemd.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTelegramAPI  = "https://api.telegram.org"
	defaultOpenCodeAPI  = "http://127.0.0.1:4096"
	defaultOpenCodeUnit = "opencode-gist.service"
	defaultModel        = "flash"
	defaultRunner       = "cli"
	defaultJobTimeout   = 20 * time.Minute
	defaultCloneTimeout = 5 * time.Minute
	defaultMaxQueue     = 3
	allowedHost         = "github.com"
	modelAliasFree      = "free"
	modelAliasFlash     = "flash"
	modelAliasPro       = "pro"
	runnerCLI           = "cli"
	runnerAPI           = "api"
	minMaxQueue         = 1
	maxMaxQueue         = 10
)

// ModelRef identifies an OpenCode model and optional variant.
type ModelRef struct {
	ProviderID string
	ModelID    string
	Variant    string
}

// String renders the reference as provider/model or provider/model#variant.
func (m ModelRef) String() string {
	ref := m.ProviderID + "/" + m.ModelID
	if m.Variant != "" {
		ref += "#" + m.Variant
	}
	return ref
}

// ParseModelRef parses provider/model and provider/model#variant references.
func ParseModelRef(raw string) (ModelRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ModelRef{}, errors.New("model reference is empty")
	}

	base := raw
	variant := ""
	if strings.Count(raw, "#") > 1 {
		return ModelRef{}, errors.New("model reference has more than one variant separator")
	}
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		base, variant = raw[:i], raw[i+1:]
		if variant == "" {
			return ModelRef{}, errors.New("model reference has an empty variant")
		}
		if strings.ContainsAny(variant, " \t\r\n/#") {
			return ModelRef{}, errors.New("model reference has an invalid variant")
		}
	}

	if strings.Count(base, "/") != 1 {
		return ModelRef{}, errors.New("model reference must be provider/model")
	}
	provider, modelID, _ := strings.Cut(base, "/")
	if provider == "" || modelID == "" {
		return ModelRef{}, errors.New("model reference must have a provider and model")
	}
	if strings.ContainsAny(provider, " \t\r\n#") || strings.ContainsAny(modelID, " \t\r\n#") {
		return ModelRef{}, errors.New("model reference contains invalid characters")
	}

	return ModelRef{ProviderID: provider, ModelID: modelID, Variant: variant}, nil
}

// Config is the validated runtime configuration.
type Config struct {
	TelegramToken       string
	TelegramAPI         string
	OwnerUserID         int64
	WorkDir             string
	OpenCodeBin         string
	OpenCodeAPI         string
	OpenCodeAPIPassword string
	OpenCodeUnit        string
	Models              map[string]ModelRef
	DefaultModel        string
	DefaultRunner       string
	JobTimeout          time.Duration
	CloneTimeout        time.Duration
	MaxQueue            int
	AllowedHosts        []string
	StateFile           string
	LogDir              string
}

// Load builds a Config using only the supplied getenv function. It never reads
// the process environment directly. Error messages name variables but never
// include their values.
func Load(getenv func(string) string) (Config, error) {
	if getenv == nil {
		return Config{}, errors.New("getenv func is nil")
	}

	token := strings.TrimSpace(getenv("TELEGRAM_BOT_TOKEN"))
	if token == "" {
		return Config{}, errors.New("TELEGRAM_BOT_TOKEN is required")
	}

	telegramAPI := envOr(getenv, "TELEGRAM_API_URL", defaultTelegramAPI)
	if !isHTTPURL(telegramAPI) {
		return Config{}, errors.New("TELEGRAM_API_URL must be an http(s) URL")
	}

	ownerRaw := strings.TrimSpace(getenv("OWNER_USER_ID"))
	if ownerRaw == "" {
		return Config{}, errors.New("OWNER_USER_ID is required")
	}
	ownerID, err := strconv.ParseInt(ownerRaw, 10, 64)
	if err != nil || ownerID <= 0 {
		return Config{}, errors.New("OWNER_USER_ID must be a positive integer")
	}

	workDir, err := absolutePath(getenv("WORK_DIR"), "WORK_DIR")
	if err != nil {
		return Config{}, err
	}
	if workDir == string(filepath.Separator) {
		return Config{}, errors.New("WORK_DIR must not be the filesystem root")
	}

	openCodeBin, err := absolutePath(getenv("OPENCODE_BIN"), "OPENCODE_BIN")
	if err != nil {
		return Config{}, err
	}

	openCodeAPI := envOr(getenv, "OPENCODE_API_URL", defaultOpenCodeAPI)
	if !isHTTPURL(openCodeAPI) {
		return Config{}, errors.New("OPENCODE_API_URL must be an http(s) URL")
	}

	apiPassword := strings.TrimSpace(getenv("OPENCODE_API_PASSWORD"))
	if apiPassword == "" {
		return Config{}, errors.New("OPENCODE_API_PASSWORD is required")
	}

	openCodeUnit := envOr(getenv, "OPENCODE_API_UNIT", defaultOpenCodeUnit)

	models := make(map[string]ModelRef, 3)
	for _, alias := range []string{modelAliasFree, modelAliasFlash, modelAliasPro} {
		ref, err := loadModelRef(getenv, "MODEL_"+strings.ToUpper(alias))
		if err != nil {
			return Config{}, err
		}
		models[alias] = ref
	}

	defaultModelName := envOr(getenv, "DEFAULT_MODEL", defaultModel)
	if _, ok := models[defaultModelName]; !ok {
		return Config{}, errors.New("DEFAULT_MODEL must be one of free, flash, pro")
	}

	defaultRunnerName := envOr(getenv, "DEFAULT_RUNNER", defaultRunner)
	if defaultRunnerName != runnerCLI && defaultRunnerName != runnerAPI {
		return Config{}, errors.New("DEFAULT_RUNNER must be cli or api")
	}

	jobTimeout, err := positiveDuration(getenv, "JOB_TIMEOUT", defaultJobTimeout)
	if err != nil {
		return Config{}, err
	}
	cloneTimeout, err := positiveDuration(getenv, "CLONE_TIMEOUT", defaultCloneTimeout)
	if err != nil {
		return Config{}, err
	}

	maxQueue, err := queueSize(getenv, defaultMaxQueue)
	if err != nil {
		return Config{}, err
	}

	allowedHosts, err := loadAllowedHosts(getenv)
	if err != nil {
		return Config{}, err
	}

	stateFile, err := absolutePath(getenv("STATE_FILE"), "STATE_FILE")
	if err != nil {
		return Config{}, err
	}
	logDir, err := absolutePath(getenv("LOG_DIR"), "LOG_DIR")
	if err != nil {
		return Config{}, err
	}

	return Config{
		TelegramToken:       token,
		TelegramAPI:         telegramAPI,
		OwnerUserID:         ownerID,
		WorkDir:             workDir,
		OpenCodeBin:         openCodeBin,
		OpenCodeAPI:         openCodeAPI,
		OpenCodeAPIPassword: apiPassword,
		OpenCodeUnit:        openCodeUnit,
		Models:              models,
		DefaultModel:        defaultModelName,
		DefaultRunner:       defaultRunnerName,
		JobTimeout:          jobTimeout,
		CloneTimeout:        cloneTimeout,
		MaxQueue:            maxQueue,
		AllowedHosts:        allowedHosts,
		StateFile:           stateFile,
		LogDir:              logDir,
	}, nil
}

// ResolveModel resolves an alias to a model reference. For the free alias, a
// non-empty freeOverride takes precedence. An override never applies to other
// aliases.
func (c Config) ResolveModel(alias, freeOverride string) (ModelRef, error) {
	if alias == modelAliasFree && strings.TrimSpace(freeOverride) != "" {
		ref, err := ParseModelRef(freeOverride)
		if err != nil {
			return ModelRef{}, errors.New("free model override is invalid")
		}
		return ref, nil
	}

	ref, ok := c.Models[alias]
	if !ok {
		return ModelRef{}, errors.New("unknown model alias")
	}
	return ref, nil
}

func envOr(getenv func(string) string, key, fallback string) string {
	value := strings.TrimSpace(getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func isHTTPURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func absolutePath(raw, name string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	cleaned := filepath.Clean(value)
	if !filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("%s must be an absolute path", name)
	}
	return cleaned, nil
}

func loadModelRef(getenv func(string) string, name string) (ModelRef, error) {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return ModelRef{}, fmt.Errorf("%s is required", name)
	}
	ref, err := ParseModelRef(raw)
	if err != nil {
		return ModelRef{}, fmt.Errorf("%s is not a valid model reference", name)
	}
	return ref, nil
}

func positiveDuration(getenv func(string) string, name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return value, nil
}

func queueSize(getenv func(string) string, fallback int) (int, error) {
	raw := strings.TrimSpace(getenv("MAX_QUEUE"))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minMaxQueue || value > maxMaxQueue {
		return 0, errors.New("MAX_QUEUE must be an integer between 1 and 10")
	}
	return value, nil
}

func loadAllowedHosts(getenv func(string) string) ([]string, error) {
	raw := strings.TrimSpace(getenv("ALLOWED_HOSTS"))
	if raw == "" {
		return []string{allowedHost}, nil
	}
	if raw != allowedHost {
		return nil, errors.New("ALLOWED_HOSTS must be exactly github.com")
	}
	return []string{allowedHost}, nil
}
