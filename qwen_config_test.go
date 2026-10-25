package main

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const localModelSettings = `{
  "modelProviders": {
    "openai": [
      {"id": "local", "baseUrl": "http://192.168.1.50:11434/v1"},
      {"id": "other", "baseUrl": "http://192.168.1.60:8000/v1"}
    ]
  },
  "security": {"auth": {"selectedType": "openai"}},
  "model": {"name": "local"},
  "env": {"OLLAMA_API_KEY": "ollama"}
}`

func TestConfiguredModelServer(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".qwen"), 0o700); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(home, ".qwen/settings.json")
	if server, err := configuredModelServer(home, nil, Args{Mode: ModeQwen}); server.IsPresent() || err != nil {
		t.Fatalf("missing settings: %v, %v", server, err)
	}
	if err := os.WriteFile(settingsPath, []byte(localModelSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := configuredModelServer(home, nil, Args{Mode: ModeQwen})
	endpoint, present := server.Get()
	if err != nil || !present || endpoint.Address.String() != "192.168.1.50" || endpoint.Port != 11434 {
		t.Fatalf("configured endpoint: %v, %v", server, err)
	}
	rules := newNetworkPolicy(HostNetwork{}, nil, server).NFTables()
	if !strings.Contains(rules, "ip daddr 192.168.1.50 tcp dport 11434 counter accept") || strings.Contains(rules, "ip daddr 192.168.1.60 tcp dport 8000 counter accept") {
		t.Fatal("policy must allow only the selected endpoint")
	}
	if err := os.WriteFile(settingsPath, []byte(`{"model":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := configuredModelServer(home, nil, Args{Mode: ModeQwen}); err == nil {
		t.Fatal("invalid settings did not fail closed")
	}
}

func TestQwenSettingsCannotEscapeSandboxHome(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "settings.json"), []byte(localModelSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, ".qwen")); err != nil {
		t.Fatal(err)
	}
	if _, err := configuredModelServer(home, nil, Args{Mode: ModeQwen}); err == nil {
		t.Fatal("followed settings symlink outside sandbox home")
	}
}

func TestQwenEndpointSelection(t *testing.T) {
	var base qwenSettings
	if err := json.Unmarshal([]byte(localModelSettings), &base); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		settings  string
		env       map[string]string
		args      []string
		want      string
		wantError bool
	}{
		{name: "selected provider", want: "http://192.168.1.50:11434/v1"},
		{name: "only selected provider allowed", args: []string{"--model", "other"}, want: "http://192.168.1.60:8000/v1"},
		{name: "inline model", args: []string{"--model=other"}, want: "http://192.168.1.60:8000/v1"},
		{name: "provider precedes environment", env: map[string]string{"OPENAI_BASE_URL": "http://192.168.1.99:1234/v1"}, want: "http://192.168.1.50:11434/v1"},
		{name: "provider precedes cli url", args: []string{"--openai-base-url=http://192.168.1.99:1234/v1"}, want: "http://192.168.1.50:11434/v1"},
		{name: "no config", settings: `{}`},
		{name: "OAuth does not grant LAN", settings: `{"security":{"auth":{"selectedType":"qwen-oauth","baseUrl":"http://192.168.1.99:1234/v1"}}}`},
		{name: "legacy fallback", settings: `{"security":{"auth":{"selectedType":"openai","baseUrl":"http://192.168.1.70:8080/v1"}}}`, want: "http://192.168.1.70:8080/v1"},
		{name: "cli fallback", settings: `{}`, args: []string{"--auth-type=openai", "--openai-base-url=http://192.168.1.70:8080/v1"}, want: "http://192.168.1.70:8080/v1"},
		{name: "env fallback", settings: `{}`, env: map[string]string{"OPENAI_MODEL": "local", "OPENAI_BASE_URL": "http://192.168.1.70:8080/v1", "OPENAI_API_KEY": "placeholder"}, want: "http://192.168.1.70:8080/v1"},
		{name: "custom provider", settings: `{"providerProtocol":{"local-provider":"openai"},"modelProviders":{"local-provider":[{"id":"local","baseUrl":"http://192.168.1.80:8000/v1"}]},"security":{"auth":{"selectedType":"openai"}},"model":{"name":"local"}}`, want: "http://192.168.1.80:8000/v1"},
		{name: "ambiguous routes", settings: `{"modelProviders":{"openai":[{"id":"local","baseUrl":"http://192.168.1.80:8000/v1"},{"id":"local","baseUrl":"http://192.168.1.90:8000/v1"}]},"security":{"auth":{"selectedType":"openai"}},"model":{"name":"local"}}`, wantError: true},
		{name: "persisted route", settings: `{"modelProviders":{"openai":[{"id":"local","baseUrl":"http://192.168.1.80:8000/v1"},{"id":"local","baseUrl":"http://192.168.1.90:8000/v1"}]},"security":{"auth":{"selectedType":"openai"}},"model":{"name":"local","baseUrl":"http://192.168.1.90:8000/v1"}}`, want: "http://192.168.1.90:8000/v1"},
		{name: "unmapped provider ignored", settings: `{"modelProviders":{"typo":[{"id":"local","baseUrl":"http://192.168.1.80:8000/v1"}]},"security":{"auth":{"selectedType":"openai"}},"model":{"name":"local"}}`},
		{name: "unknown OpenAI-like provider ignored", settings: `{"modelProviders":{"openai-typo":[{"id":"local","baseUrl":"http://192.168.1.80:8000/v1"}]},"security":{"auth":{"selectedType":"openai"}},"model":{"name":"local"}}`},
		{name: "responses route", settings: `{"modelProviders":{"openai":[{"id":"local","wireApi":"responses","baseUrl":"http://192.168.1.80:8000/v1"}]},"security":{"auth":{"selectedType":"openai"}},"model":{"name":"local"}}`, want: "http://192.168.1.80:8000/v1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := base
			if test.settings != "" {
				settings = qwenSettings{}
				if err := json.Unmarshal([]byte(test.settings), &settings); err != nil {
					t.Fatal(err)
				}
			}
			got, err := settings.modelEndpoint(test.env, Args{Mode: ModeQwen, Arguments: test.args})
			if (err != nil) != test.wantError || got != test.want {
				t.Fatalf("endpoint=%q, error=%v; want %q, error=%v", got, err, test.want, test.wantError)
			}
		})
	}
}

func TestEndpointVariableExpansion(t *testing.T) {
	var settings qwenSettings
	if err := json.Unmarshal([]byte(strings.ReplaceAll(localModelSettings, "http://192.168.1.50:11434/v1", "http://${MODEL_HOST}:$MODEL_PORT/v1")), &settings); err != nil {
		t.Fatal(err)
	}
	settings.Env["MODEL_HOST"], settings.Env["MODEL_PORT"] = "192.168.1.50", "11434"
	endpoint, err := settings.modelEndpoint(map[string]string{"MODEL_PORT": "8000"}, Args{Mode: ModeQwen})
	if err != nil || endpoint != "http://192.168.1.50:8000/v1" {
		t.Fatalf("endpoint %q, %v", endpoint, err)
	}
	if got := expandEndpointVariables("${NOT_DEFINED}", nil); got != "${NOT_DEFINED}" {
		t.Fatal("undefined variables must not be erased")
	}
}

func TestEndpointOverridesSkipPromptValues(t *testing.T) {
	got, err := endpointOverrides([]string{"--prompt", "--model", "--model=other", "--", "--model=ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "other" {
		t.Fatalf("unexpected model override %q", got.Model)
	}
}

func TestJSONComments(t *testing.T) {
	input := "\ufeff" + `{
  // Endpoint URLs must not be mistaken for comments.
  "url": "http://192.168.1.50:11434/v1", /* block
  comment */ "value": "escaped quote: \" // still a string"
}`
	stripped, err := stripJSONComments([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(stripped, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"url": "http://192.168.1.50:11434/v1", "value": "escaped quote: \" // still a string"}
	if !maps.Equal(got, want) {
		t.Fatalf("got %v", got)
	}
	if _, err := stripJSONComments([]byte(`{/* never closed`)); err == nil {
		t.Fatal("unterminated comment accepted")
	}
}
