package host

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFreeRDPEnvironmentConfigReachesAdapter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[server]\nhost = \"192.0.2.10\"\n[freerdp.env]\nSDL_RENDER_DRIVER = \"opengl\"\nEMPTY = \"\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := c.Adapter()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Display.Env["SDL_RENDER_DRIVER"] != "opengl" {
		t.Fatal("env not passed to display")
	}
	if value, ok := cfg.Display.Env["EMPTY"]; !ok || value != "" {
		t.Fatal("empty value lost")
	}
}
