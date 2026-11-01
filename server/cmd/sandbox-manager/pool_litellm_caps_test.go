package main

import (
	"strings"
	"testing"
)

func TestLoadPoolRefusesLiteLLMWithoutTaskCaps(t *testing.T) {
	for name, c := range map[string]struct {
		set  map[string]string
		want string
	}{
		"no budget":     {map[string]string{"SANDBOX_LITELLM_RPM_LIMIT": "60"}, "SANDBOX_LITELLM_MAX_BUDGET"},
		"no rate limit": {map[string]string{"SANDBOX_LITELLM_MAX_BUDGET": "2"}, "SANDBOX_LITELLM_RPM_LIMIT"},
		"zero budget":   {map[string]string{"SANDBOX_LITELLM_MAX_BUDGET": "0", "SANDBOX_LITELLM_RPM_LIMIT": "60"}, "SANDBOX_LITELLM_MAX_BUDGET"},
	} {
		t.Run(name, func(t *testing.T) {
			env := baseEnv()
			env["SANDBOX_LITELLM_URL"] = "http://l:4000"
			env["SANDBOX_LITELLM_MASTER_KEY"] = "sk-master"
			env["SANDBOX_LITELLM_MODELS"] = "x"
			for k, v := range c.set {
				env[k] = v
			}
			_, err := loadPool(envLookup(env))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error naming %s, got %v", c.want, err)
			}
		})
	}
}
