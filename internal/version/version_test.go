package version

import (
	"os"
	"regexp"
	"testing"
)

// TestBuildScriptUsesThisVersion: build.bash takes the version from this
// package, not from a number of its own.
func TestBuildScriptUsesThisVersion(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(Number) {
		t.Fatalf("version %q is not x.y.z", Number)
	}
	b, err := os.ReadFile("../../build.bash")
	if err != nil {
		t.Skip("build.bash not found")
	}
	if !regexp.MustCompile(`internal/version/version\.go`).Match(b) {
		t.Error("build.bash does not read internal/version/version.go")
	}
	if regexp.MustCompile(`VERSION="\$\{VERSION:-\d`).Match(b) {
		t.Error("build.bash still has a version number of its own")
	}
}
