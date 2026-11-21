package backend

import (
	_ "embed"
	"slices"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

//go:embed CHANGELOG.md
var changelog string

type Release struct {
	Version string `json:"version"`
	Date    string `json:"date"`
	Notes   string `json:"notes"`
}

// Releases returns the history available to the running build, newest first.
func Releases(version string) []Release {
	return parseChangelog(changelog, version)
}

func parseChangelog(text, version string) []Release {
	sections := strings.Split(text, "\n## ")
	releases := make([]Release, 0, len(sections)-1)
	for _, section := range sections[1:] {
		heading, notes, _ := strings.Cut(section, "\n")
		tag, date, _ := strings.Cut(heading, " — ")
		if !semver.IsValid(tag) {
			continue
		}
		if semver.IsValid(version) && (semver.Compare(tag, version) > 0 ||
			semver.Prerelease(version) == "" && semver.Prerelease(tag) != "") {
			continue
		}
		if _, err := time.Parse(time.DateOnly, date); err != nil {
			continue
		}
		releases = append(releases, Release{Version: tag, Date: date, Notes: strings.TrimSpace(notes)})
	}
	slices.SortStableFunc(releases, func(a, b Release) int { return semver.Compare(b.Version, a.Version) })
	return releases
}
