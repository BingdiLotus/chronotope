// Package appconfig loads the deployment-owned application endpoints. Request
// payloads and model output never select network addresses or credentials.
package appconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/bingdilotus/chronotope/internal/core/timer"
)

type Bridge struct {
	Ref         string
	TenantID    string
	URL         string
	BearerToken string
}

type Config struct {
	Bridges      []Bridge
	TimerTargets []timer.Target
}

type bridgeInput struct {
	Ref      string `json:"ref"`
	TenantID string `json:"tenant_id"`
	URL      string `json:"url"`
	TokenEnv string `json:"token_env"`
}

type targetInput struct {
	Ref         string `json:"ref"`
	TenantID    string `json:"tenant_id"`
	SubjectRef  string `json:"subject_ref"`
	ScopeID     string `json:"scope_id"`
	CallbackURL string `json:"callback_url"`
	TokenEnv    string `json:"token_env"`
}

func decode(name string, dst any) error {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("invalid %s: %w", name, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("%s must contain one JSON array", name)
	}
	return nil
}

func token(env string) (string, error) {
	value, ok := os.LookupEnv(env)
	if env == "" || !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("application endpoint requires a nonempty token_env reference")
	}
	return value, nil
}

func Load() (Config, error) {
	var input []bridgeInput
	var targets []targetInput
	if err := decode("APPLICATION_BRIDGES_JSON", &input); err != nil {
		return Config{}, err
	}
	if err := decode("APPLICATION_TIMER_TARGETS_JSON", &targets); err != nil {
		return Config{}, err
	}
	var cfg Config
	refs := map[string]bool{}
	for _, in := range input {
		u, err := url.Parse(in.URL)
		if in.Ref == "" || in.TenantID == "" || refs[in.Ref] || err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
			return Config{}, fmt.Errorf("invalid or duplicate configured application bridge")
		}
		credential, err := token(in.TokenEnv)
		if err != nil {
			return Config{}, err
		}
		refs[in.Ref] = true
		cfg.Bridges = append(cfg.Bridges, Bridge{in.Ref, in.TenantID, in.URL, credential})
	}
	refs = map[string]bool{}
	scopes := map[string]bool{}
	for _, in := range targets {
		t := timer.Target{Ref: in.Ref, TenantID: in.TenantID, SubjectRef: in.SubjectRef, ScopeID: in.ScopeID, CallbackURL: in.CallbackURL}
		key, _ := json.Marshal([]string{in.TenantID, in.SubjectRef, in.ScopeID})
		if err := t.Validate(); err != nil || refs[in.Ref] || scopes[string(key)] {
			return Config{}, fmt.Errorf("invalid or ambiguous configured timer target")
		}
		credential, err := token(in.TokenEnv)
		if err != nil {
			return Config{}, err
		}
		t.BearerToken = credential
		refs[in.Ref] = true
		scopes[string(key)] = true
		cfg.TimerTargets = append(cfg.TimerTargets, t)
	}
	return cfg, nil
}
