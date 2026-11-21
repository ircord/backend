package backend

import "testing"

func TestReleaseHistoryMatchesBuild(t *testing.T) {
	const notes = `# Changelog

## Unreleased

- Unreleased changes.

## v2.0.0 — 2026-11-21

- A future change.

## v1.0.0 — 2026-11-20

### Fixed

- A stable change.

## v1.0.0-rc.1 — 2026-11-19

- A prerelease change.
`
	stable := parseChangelog(notes, "v1.0.0")
	if len(stable) != 1 || stable[0].Version != "v1.0.0" ||
		stable[0].Date != "2026-11-20" || stable[0].Notes != "### Fixed\n\n- A stable change." {
		t.Fatalf("stable release history = %#v", stable)
	}
	prerelease := parseChangelog(notes, "v1.0.0-rc.1")
	if len(prerelease) != 1 || prerelease[0].Version != "v1.0.0-rc.1" {
		t.Fatalf("prerelease history = %#v", prerelease)
	}
}
