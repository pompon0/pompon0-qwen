package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Only endpoint-selection fields are decoded. Qwen owns the rest of its schema
// and can add settings without requiring a launcher update.
type qwenSettings struct {
	ModelProviders   map[string][]qwenModel `json:"modelProviders"`
	ProviderProtocol map[string]string      `json:"providerProtocol"`
	Model            struct {
		Name    string `json:"name"`
		BaseURL string `json:"baseUrl"`
	} `json:"model"`
	Security struct {
		Auth struct {
			SelectedType string `json:"selectedType"`
			BaseURL      string `json:"baseUrl"`
		} `json:"auth"`
	} `json:"security"`
	Env map[string]string `json:"env"`
}

type qwenModel struct {
	ID      string `json:"id"`
	BaseURL string `json:"baseUrl"`
	WireAPI string `json:"wireApi"`
}

// These host-owned inputs are mounted read-only over writable launcher-owned
// Qwen state. Other host .qwen contents, including manual sessions, are not shared.
var qwenSharedConfiguration = [...]string{
	"settings.json", ".env", "QWEN.md", "commands", "skills", "rules",
}

// prepareQwenState initializes the state directory derived from Config.Home,
// shared across launcher executions and
// repositories, separately from the host's normal .qwen. It seeds credentials
// only when absent, preserving tokens refreshed by earlier launcher sessions.
// Root-relative operations keep sandbox-created symlinks from redirecting
// supervisor writes outside launcher state. Missing host configuration is valid;
// the host .qwen directory and credential files must not be symlinks.
func prepareQwenState(home, state string) error {
	private, err := os.OpenRoot(state)
	if err != nil {
		return fmt.Errorf("OpenRoot(Qwen state): %w", err)
	}
	defer private.Close()
	host, err := os.OpenRoot(home)
	if err != nil {
		return fmt.Errorf("OpenRoot(host home): %w", err)
	}
	defer host.Close()
	info, err := host.Lstat(".qwen")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("Lstat(host .qwen): %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("host .qwen must be a directory, not a symlink")
	}
	for _, name := range []string{"oauth_creds.json", "mcp-oauth-tokens.json"} {
		if err := seedQwenCredential(host, private, name); err != nil {
			return err
		}
	}
	return nil
}

func seedQwenCredential(host, private *os.Root, name string) error {
	path := filepath.Join(".qwen", name)
	info, err := host.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("Lstat(%s): %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("host Qwen credential %s must be a regular file", path)
	}
	source, err := host.Open(path)
	if err != nil {
		return fmt.Errorf("Open(%s): %w", path, err)
	}
	defer source.Close()
	// Publish a complete file without replacing an existing credential. Parallel
	// launcher starts must not see a partially copied token or undo a refresh.
	temporary := ".seed-" + rand.Text()
	destination, err := private.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create private credential %s: %w", name, err)
	}
	defer private.Remove(temporary)
	_, copyErr := io.Copy(destination, source)
	closeErr := destination.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return fmt.Errorf("copy Qwen credential %s: %w", name, err)
	}
	if err := private.Link(temporary, name); err != nil && !os.IsExist(err) {
		return fmt.Errorf("publish Qwen credential %s: %w", name, err)
	}
	return nil
}

// Read the host's ~/.qwen user configuration shared read-only with the sandbox,
// never project-supplied settings. No Qwen/project code executes
// while the supervisor still has authority over the firewall.
func (c Config) modelEndpoint(ctx context.Context) (ModelEndpoint, error) {
	path := filepath.Join(c.Home, ".qwen/settings.json")
	var settings qwenSettings
	// Bound host settings reads to the host home. A symlink must not make the
	// supervisor read files outside that root that are unavailable in the sandbox.
	root, err := os.OpenRoot(c.Home)
	if err != nil {
		return ModelEndpoint{}, fmt.Errorf("open Qwen home: %w", err)
	}
	defer root.Close()
	data, err := root.ReadFile(".qwen/settings.json")
	if err != nil && !os.IsNotExist(err) {
		return ModelEndpoint{}, fmt.Errorf("read Qwen user settings: %w", err)
	}
	if err == nil {
		data, err = stripJSONComments(data)
		if err != nil {
			return ModelEndpoint{}, fmt.Errorf("read Qwen user settings: %w", err)
		}
		if err := json.Unmarshal(data, &settings); err != nil {
			return ModelEndpoint{}, fmt.Errorf("invalid Qwen user settings at %s: %w", path, err)
		}
	}
	endpoint, err := settings.modelEndpoint(c.Environment(), c.Overrides)
	if err != nil {
		return ModelEndpoint{}, err
	}
	server, err := modelEndpointFromURL(ctx, endpoint)
	if err != nil {
		return ModelEndpoint{}, fmt.Errorf("Qwen model endpoint: %w (edit ~/.qwen/settings.json on the host)", err)
	}
	return server, nil
}

// Endpoint resolution intentionally stays separate from filter installation.
// Model provider metadata wins over CLI/env/legacy endpoint fallbacks, following
// Qwen's documented resolution layers. Only one selected route grants access.
func (settings qwenSettings) modelEndpoint(environment map[string]string, overrides modelOverrides) (string, error) {
	env := make(map[string]string, len(settings.Env)+len(environment))
	maps.Copy(env, settings.Env)
	maps.Copy(env, environment)
	expand := func(value string) string { return expandEndpointVariables(value, env) }
	authType := cmp.Or(overrides.AuthType, expand(settings.Security.Auth.SelectedType))
	if authType == "" && env["OPENAI_API_KEY"] != "" && env["OPENAI_BASE_URL"] != "" && env["OPENAI_MODEL"] != "" {
		authType = "openai"
	}
	if authType == "" || authType == "qwen-oauth" {
		return "", nil
	}
	modelEnv, urlEnv := "", ""
	switch authType {
	case "openai", "openai-responses":
		modelEnv, urlEnv = "OPENAI_MODEL", "OPENAI_BASE_URL"
	case "anthropic":
		modelEnv, urlEnv = "ANTHROPIC_MODEL", "ANTHROPIC_BASE_URL"
	case "gemini":
		modelEnv, urlEnv = "GEMINI_MODEL", "GEMINI_BASE_URL"
	default:
		return "", nil
	}
	model := cmp.Or(overrides.Model, expand(settings.Model.Name), env[modelEnv])
	// Persisted baseUrl distinguishes equal model IDs at different providers.
	hint := ""
	if overrides.Model == "" && settings.Model.Name != "" {
		hint = expand(settings.Model.BaseURL)
	}
	var matches []string
	for provider, models := range settings.ModelProviders {
		protocol := provider
		if mapped, ok := settings.ProviderProtocol[provider]; ok {
			protocol = mapped
		}
		for _, entry := range models {
			entryProtocol := protocol
			if entry.WireAPI != "" && entry.WireAPI != "chat-completions" && entry.WireAPI != "responses" {
				continue
			}
			if entry.WireAPI != "" && protocol != "openai" && protocol != "openai-responses" {
				continue
			}
			if protocol == "openai" && entry.WireAPI == "responses" {
				entryProtocol = "openai-responses"
			}
			protocolMatches := entryProtocol == authType || (isOpenAIProtocol(entryProtocol) && isOpenAIProtocol(authType))
			if !protocolMatches || expand(entry.ID) != model || model == "" {
				continue
			}
			endpoint := expand(entry.BaseURL)
			if hint != "" && endpoint != hint {
				continue
			}
			if len(matches) > 0 && matches[0] != endpoint {
				return "", fmt.Errorf("ambiguous Qwen model endpoint: set model.baseUrl in user settings to select one route")
			}
			matches = append(matches, endpoint)
		}
	}
	if len(matches) > 0 && matches[0] != "" {
		return matches[0], nil
	}
	if hint != "" {
		return "", fmt.Errorf("Qwen model.baseUrl does not match a configured provider for the selected model")
	}
	return cmp.Or(overrides.BaseURL, env[urlEnv], expand(settings.Security.Auth.BaseURL)), nil
}

func isOpenAIProtocol(protocol string) bool {
	return protocol == "openai" || protocol == "openai-responses"
}

var endpointVariable = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

func expandEndpointVariables(value string, environment map[string]string) string {
	return endpointVariable.ReplaceAllStringFunc(value, func(match string) string {
		name := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(match, "$"), "{"), "}")
		if replacement, ok := environment[name]; ok {
			return replacement
		}
		return match
	})
}

// Qwen accepts JSON with comments. Replace comments with whitespace while
// preserving strings and positions, then let encoding/json validate the rest.
func stripJSONComments(data []byte) ([]byte, error) {
	text := strings.TrimPrefix(string(data), "\ufeff")
	result := []byte(text)
	quoted, escaped := false, false
	for i := 0; i < len(result); i++ {
		if quoted {
			if escaped {
				escaped = false
			} else if result[i] == '\\' {
				escaped = true
			} else if result[i] == '"' {
				quoted = false
			}
			continue
		}
		if result[i] == '"' {
			quoted = true
			continue
		}
		if result[i] != '/' || i+1 == len(result) {
			continue
		}
		switch result[i+1] {
		case '/':
			for i < len(result) && result[i] != '\n' && result[i] != '\r' {
				result[i] = ' '
				i++
			}
		case '*':
			result[i], result[i+1] = ' ', ' '
			i += 2
			for i+1 < len(result) && !(result[i] == '*' && result[i+1] == '/') {
				if result[i] != '\n' && result[i] != '\r' {
					result[i] = ' '
				}
				i++
			}
			if i+1 >= len(result) {
				return nil, fmt.Errorf("unterminated JSON comment")
			}
			result[i], result[i+1] = ' ', ' '
			i++
		}
	}
	return result, nil
}
