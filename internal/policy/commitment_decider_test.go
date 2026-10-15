package policy

import (
	"context"
	"testing"
)

func TestRuleDecider(t *testing.T) {
	d := RuleCommitmentDecider{}
	ctx := context.Background()
	admit, err := d.Decide(ctx, "我明天给你答复", "")
	if err != nil || !admit.IsCommissive {
		t.Fatalf("「我明天给你答复」应判定为承诺: admit=%+v err=%v", admit, err)
	}
	neg, err := d.Decide(ctx, "今天的天气不错", "")
	if err != nil || neg.IsCommissive {
		t.Fatalf("「今天的天气不错」不应判定为承诺: %+v err=%v", neg, err)
	}
}

func TestDeciderFromConfig(t *testing.T) {
	old := envLookup
	defer func() { envLookup = old }()
	envLookup = func(k string) string {
		if k == "COMMITMENT_DECIDER" {
			return "rule"
		}
		return ""
	}
	if _, ok := DeciderFromConfig().(RuleCommitmentDecider); !ok {
		t.Fatal("COMMITMENT_DECIDER=rule 应选规则档")
	}
}
