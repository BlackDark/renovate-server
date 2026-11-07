package pins

import (
	"os"
	"regexp"
	"testing"
)

// mise, go.mod and the Docker builder must move together. Renovate groups the
// bumps; this fails the build when a pin is edited by hand and the others are not.
func TestGoVersionPinsAgree(t *testing.T) {
	gomod := mustRead(t, "go.mod")
	mise := mustRead(t, "mise.toml")
	docker := mustRead(t, "Dockerfile")

	goLine := regexp.MustCompile(`(?m)^go (\d+\.\d+)(?:\.(\d+))?\s*$`).FindStringSubmatch(gomod)
	if goLine == nil {
		t.Fatal("go.mod has no go directive")
	}
	minor := goLine[1]
	full := minor
	if goLine[2] != "" {
		full = minor + "." + goLine[2]
	}

	misePin := regexp.MustCompile(`(?m)^go = "([^"]+)"`).FindStringSubmatch(mise)
	if misePin == nil {
		t.Fatal(`mise.toml has no go = "…" pin`)
	}
	if misePin[1] != full {
		t.Fatalf("mise go %s, go.mod %s", misePin[1], full)
	}

	image := regexp.MustCompile(`golang:(\d+\.\d+)`).FindStringSubmatch(docker)
	if image == nil {
		t.Fatal("Dockerfile has no golang: image")
	}
	if image[1] != minor {
		t.Fatalf("Dockerfile golang %s, go.mod %s", image[1], minor)
	}
}

func mustRead(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
