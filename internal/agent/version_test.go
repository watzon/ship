package agent

import "testing"

func TestVersionPrefersReleaseStamp(t *testing.T) {
	orig := AgentVersion
	t.Cleanup(func() { AgentVersion = orig })
	AgentVersion = "9.9.9"
	if got := Version(); got != "9.9.9" {
		t.Fatalf("Version() = %q", got)
	}
}

func TestVersionFallsBackToDevSentinel(t *testing.T) {
	orig := AgentVersion
	t.Cleanup(func() { AgentVersion = orig })
	AgentVersion = ""
	// Under `go test` the main module has no release version recorded, so
	// Version() must land on the dev sentinel — never an asset-less guess.
	if got := Version(); got != devVersion {
		t.Fatalf("Version() = %q, want %q", got, devVersion)
	}
}

func TestIsReleaseTag(t *testing.T) {
	cases := map[string]bool{
		"v0.4.1":                               true,
		"v12.0.3":                              true,
		"0.4.1":                                false, // module versions carry the v prefix
		"(devel)":                              false,
		"":                                     false,
		"v0.4.2-0.20260702150405-abcdef123456": false, // pseudo-version from @main
		"v0.5.0-rc.1":                          false, // no release assets published for prereleases
	}
	for input, want := range cases {
		if got := isReleaseTag(input); got != want {
			t.Errorf("isReleaseTag(%q) = %v, want %v", input, got, want)
		}
	}
}
