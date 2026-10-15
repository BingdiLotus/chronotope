package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// AdmitDecision 准入门的判定（修正版 §2.3）。
type AdmitDecision struct {
	IsCommissive bool    // 判定为承诺类言语行为
	Strength     string  // strong / weak / conditional
	Confidence   float64 // 校准概率（进审计——群体口径）
	Reason       string  // 判据说明（可审计）
}

// CommitmentDecider 策略缝（与 harness 适配器同构——可替换）。
type CommitmentDecider interface {
	Decide(ctx context.Context, message string, counterparty string) (AdmitDecision, error)
}

// RuleCommitmentDecider 规则降级档（完全不出站——召回率换隐私；修正版
// §2.3 的缓解路径）。触发词启发式只做「疑似」召回（低阈值高召回），
// 误报由完整抽取阶段承担。
type RuleCommitmentDecider struct{}

func (RuleCommitmentDecider) Decide(_ context.Context, message string, _ string) (AdmitDecision, error) {
	lower := strings.ToLower(message)
	markers := []string{"我会", "我明天", "我下周", "我保证", "我答应", "我来", "我负责",
		"i will", "i'll", "i promise", "i commit", "let me", "i'll get back"}
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return AdmitDecision{IsCommissive: true, Strength: "weak",
				Confidence: 0.5, Reason: "rule marker: " + m}, nil
		}
	}
	return AdmitDecision{IsCommissive: false, Confidence: 0.5, Reason: "no marker"}, nil
}

// OpenAIDecisionsDecider 默认档（中文场景优先——gpt-6-luna 的
// /v1/decisions；修正版 §2.3）。OpenAI 兼容（现有代理可走）。
type OpenAIDecisionsDecider struct {
	BaseURL string // 默认 https://api.openai.com/v1/decisions——可指向代理
	Key     string
	Model   string // 默认 gpt-6-luna
	Client  *http.Client
}

func (d *OpenAIDecisionsDecider) Decide(ctx context.Context, message string, counterparty string) (AdmitDecision, error) {
	base := d.BaseURL
	if base == "" {
		base = "https://api.openai.com/v1/decisions"
	}
	model := d.Model
	if model == "" {
		model = "gpt-6-luna"
	}
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	body, _ := json.Marshal(map[string]any{
		"model": model,
		"input": message,
		"questions": []map[string]any{
			{"type": "predicate", "name": "is_commissive",
				"instructions": "Does this message commit the speaker to a future action or obligation toward the addressee? Count promises, offers, and commitments to deliver or do something."},
			{"type": "choice", "name": "strength",
				"instructions": "How strong is the commitment?",
				"choices": []map[string]string{
					{"value": "strong", "description": "Unconditional promise."},
					{"value": "weak", "description": "Hedged or soft commitment."},
					{"value": "conditional", "description": "Commitment with conditions."},
				}},
			{"type": "predicate", "name": "has_addressee",
				"instructions": "Is the commitment directed at a specific person or audience?"},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base, bytes.NewReader(body))
	if err != nil {
		return AdmitDecision{}, fmt.Errorf("commitment decider: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if d.Key != "" {
		req.Header.Set("Authorization", "Bearer "+d.Key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return AdmitDecision{}, fmt.Errorf("commitment decider: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return AdmitDecision{}, fmt.Errorf("commitment decider: status %d", resp.StatusCode)
	}
	var out struct {
		Answers []struct {
			Type        string  `json:"type"`
			Name        string  `json:"name"`
			Probability float64 `json:"probability"`
			Choice      string  `json:"choice"`
			Confidence  float64 `json:"confidence"`
		} `json:"answers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return AdmitDecision{}, fmt.Errorf("commitment decider decode: %w", err)
	}
	var isCom, hasAddr bool
	var isConf, addrConf float64
	strength := ""
	for _, a := range out.Answers {
		switch a.Name {
		case "is_commissive":
			isCom, isConf = a.Probability >= 0.5, a.Probability
		case "has_addressee":
			hasAddr, addrConf = a.Probability >= 0.5, a.Probability
		case "strength":
			strength = a.Choice
		}
	}
	admit := AdmitDecision{IsCommissive: isCom && hasAddr, Strength: strength,
		Confidence: (isConf + addrConf) / 2}
	if !admit.IsCommissive {
		admit.Reason = "not commissive toward addressee"
	} else {
		admit.Reason = fmt.Sprintf("predicate is_commissive=%.2f has_addressee=%.2f strength=%s", isConf, addrConf, strength)
	}
	return admit, nil
}

// DeciderFromConfig 按环境选实现（修正版 §2.3 三实现）。
func DeciderFromConfig() CommitmentDecider {
	switch strings.ToLower(strings.TrimSpace(getenv("COMMITMENT_DECIDER", "openai"))) {
	case "rule", "off":
		return RuleCommitmentDecider{}
	default:
		return &OpenAIDecisionsDecider{
			BaseURL: getenv("COMMITMENT_DECIDER_BASE", ""),
			Key:     getenv("COMMITMENT_DECIDER_KEY", ""),
			Model:   getenv("COMMITMENT_DECIDER_MODEL", "gpt-6-luna"),
		}
	}
}

func getenv(k, def string) string {
	if v := strings.TrimSpace(envLookup(k)); v != "" {
		return v
	}
	return def
}

// envLookup 环境读取（可注入替身——测试隔离）。
var envLookup = os.Getenv
