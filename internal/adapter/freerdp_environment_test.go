package adapter

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestFreeRDPEnvironmentOverridesAreChildOnly(t *testing.T) {
	t.Setenv("SDL_VIDEODRIVER", "inherited")
	t.Setenv("HTTP_PROXY", "inherited-proxy")
	t.Setenv("ALL_PROXY", "inherited-proxy")
	t.Setenv("RDP_ENV_TEST", "parent")
	overrides := map[string]string{
		"SDL_VIDEODRIVER": "explicit",
		"RDP_ENV_TEST":    "literal $HOME = 值\nsecond line",
		"RDP_ENV_EMPTY":   "",
		"https_proxy":     "explicit-proxy",
	}
	env, err := displayEnvironment("wayland", overrides)
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]string)
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		if _, exists := values[key]; exists {
			t.Fatal("duplicate environment key", key)
		}
		values[key] = value
	}
	for key, want := range overrides {
		if got, ok := values[key]; !ok || got != want {
			t.Fatal("override lost", key)
		}
	}
	for _, key := range []string{"HTTP_PROXY", "ALL_PROXY"} {
		if _, ok := values[key]; ok {
			t.Fatal("inherited proxy leaked", key)
		}
	}
	if os.Getenv("SDL_VIDEODRIVER") != "inherited" || os.Getenv("RDP_ENV_TEST") != "parent" {
		t.Fatal("controller environment was modified")
	}
	for _, driver := range []string{"", "wayland"} {
		env, err := displayEnvironment(driver, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := "SDL_VIDEODRIVER=" + driver
		if driver == "" {
			want = "SDL_VIDEODRIVER=inherited"
		}
		found := false
		for _, entry := range env {
			if entry == want {
				found = true
			}
		}
		if !found {
			t.Fatal("optional env changed existing driver behavior")
		}
	}
}

func TestInvalidFreeRDPEnvironmentFailsBeforeCredentialsOrSpawn(t *testing.T) {
	cfg := displayTestConfig()
	cfg.Env = map[string]string{"BAD=KEY": "value"}
	cfg.Password = func(context.Context) (string, error) {
		t.Fatal("invalid environment requested credentials")
		return "", nil
	}
	d := NewDisplay(context.Background(), cfg, Agent{}, nil)
	if err := d.Start(context.Background(), 1, true); err == nil || !strings.Contains(err.Error(), "invalid FreeRDP environment") || d.command != nil {
		t.Fatal("invalid environment reached process startup", err)
	}
}

func TestFreeRDPEnvironmentRejectsInvalidEntries(t *testing.T) {
	for _, overrides := range []map[string]string{
		{"": "value"}, {"A=B": "value"}, {"A\x00B": "value"}, {"A": "secret\x00value"},
	} {
		if _, err := displayEnvironment("", overrides); err == nil {
			t.Fatal("invalid environment accepted")
		} else if strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid environment value was included in error")
		}
	}
}
