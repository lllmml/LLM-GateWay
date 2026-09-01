package provider

import "testing"

func TestParseModelRequiresProviderNamespace(t *testing.T) {
	got, err := ParseModel("openai/gpt-test")
	if err != nil {
		t.Fatalf("parse model: %v", err)
	}
	if got.Provider != OpenAI || got.Model != "gpt-test" {
		t.Fatalf("model ref = %+v", got)
	}
	for _, raw := range []string{"gpt-test", "openai/", "/gpt-test", "openai/foo/bar"} {
		if _, err := ParseModel(raw); err == nil {
			t.Fatalf("ParseModel(%q) returned nil error", raw)
		}
	}
}
