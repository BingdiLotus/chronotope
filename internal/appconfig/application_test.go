package appconfig

import "testing"

func TestDeploymentEndpointsRequireCredentialReferences(t *testing.T) {
	t.Setenv("APPLICATION_BRIDGES_JSON", `[{"ref":"bots","tenant_id":"org","url":"http://bots/internal/tools","token_env":"APP_TEST_TOKEN"}]`)
	t.Setenv("APPLICATION_TIMER_TARGETS_JSON", `[{"ref":"bots-fire","tenant_id":"org","subject_ref":"bot","scope_id":"channel","callback_url":"http://bots/internal/fire","token_env":"APP_TEST_TOKEN"}]`)
	t.Setenv("APP_TEST_TOKEN", "test-only")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Bridges) != 1 || cfg.Bridges[0].BearerToken != "test-only" || len(cfg.TimerTargets) != 1 {
		t.Fatalf("unexpected deployment config: %+v", cfg)
	}
	t.Setenv("APP_TEST_TOKEN", "")
	if _, err := Load(); err == nil {
		t.Fatal("empty service token accepted")
	}
}

func TestDeploymentConfigRejectsInlineSecretsAndAmbiguousTargets(t *testing.T) {
	t.Setenv("APPLICATION_BRIDGES_JSON", `[{"ref":"bots","tenant_id":"org","url":"http://bots/tools","bearer_token":"forbidden"}]`)
	t.Setenv("APPLICATION_TIMER_TARGETS_JSON", "")
	if _, err := Load(); err == nil {
		t.Fatal("inline secret accepted")
	}
	t.Setenv("APPLICATION_BRIDGES_JSON", "")
	t.Setenv("APP_TEST_TOKEN", "test-only")
	t.Setenv("APPLICATION_TIMER_TARGETS_JSON", `[{"ref":"one","tenant_id":"org","subject_ref":"bot","scope_id":"channel","callback_url":"http://bots/fire","token_env":"APP_TEST_TOKEN"},{"ref":"two","tenant_id":"org","subject_ref":"bot","scope_id":"channel","callback_url":"http://other/fire","token_env":"APP_TEST_TOKEN"}]`)
	if _, err := Load(); err == nil {
		t.Fatal("ambiguous scope accepted")
	}
}
