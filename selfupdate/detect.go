package selfupdate

import (
	"fmt"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/blang/semver"
	"github.com/google/go-github/v50/github"
	"github.com/samber/lo"
)

func findAssetFromRelease(
	rel *github.RepositoryRelease, filters []*regexp.Regexp,
) (*github.ReleaseAsset, error) {
	// Generate candidates
	suffixes := make([]string, 0, 2*7*2)
	for _, sep := range []rune{'_', '-'} {
		for _, ext := range []string{".zip", ".tar.gz", ".tgz", ".gzip", ".gz", ".tar.xz", ".xz", ""} {
			suffix := fmt.Sprintf("%s%c%s%s", runtime.GOOS, sep, runtime.GOARCH, ext)
			suffixes = append(suffixes, suffix)
			if runtime.GOOS == "windows" {
				suffix = fmt.Sprintf("%s%c%s.exe%s", runtime.GOOS, sep, runtime.GOARCH, ext)
				suffixes = append(suffixes, suffix)
			}
		}
	}

	for _, asset := range rel.Assets {
		name := asset.GetName()
		if len(filters) > 0 {
			// if some filters are defined, match them: if any one matches, the asset is selected
			matched := false
			for _, filter := range filters {
				if filter.MatchString(name) {
					matched = true
					break
				}
			}

			if !matched {
				continue
			}
		}

		for _, s := range suffixes {
			if strings.HasSuffix(name, s) { // require version, arch etc
				// default: assume single artifact
				return asset, nil
			}
		}
	}

	return nil, fmt.Errorf("could not find any asset from release %q for %s %s",
		rel.GetTagName(), runtime.GOOS, runtime.GOARCH)
}

func findValidationAsset(rel *github.RepositoryRelease, validationName string) (*github.ReleaseAsset, bool) {
	for _, asset := range rel.Assets {
		if asset.GetName() == validationName {
			return asset, true
		}
	}

	return nil, false
}

// DetectLatest tries to get the latest version of the repository on GitHub. 'slug' means 'owner/name' formatted string.
// It fetches releases information from GitHub API and find out the latest release with matching the tag names and asset names.
// Drafts and pre-releases are ignored. Assets would be suffixed by the OS name and the arch name such as 'foo_linux_amd64'
// where 'foo' is a command name. '-' can also be used as a separator. File can be compressed with zip, gzip, zxip, tar&zip or tar&zxip.
// So the asset can have a file extension for the corresponding compression format such as '.zip'.
// On Windows, '.exe' also can be contained such as 'foo_windows_amd64.exe.zip'.
func (up *Updater) DetectLatest(owner, name string) (*Release, error) {
	return up.DetectVersion(owner, name, "")
}

// DetectVersion tries to get the given version of the repository on Github.
func (up *Updater) DetectVersion(owner, name, version string) (*Release, error) {
	rels, res, err := up.api.Repositories.ListReleases(up.apiCtx, owner, name, nil)
	if err != nil {
		if res != nil && res.StatusCode == 404 {
			return nil, fmt.Errorf("repository or release not found: %w", err)
		}

		return nil, fmt.Errorf("failed to fetch releases: %w", err)
	}

	rel, err := findRelease(rels, version)
	if err != nil {
		return nil, err
	}

	if rel.GetDraft() {
		return nil, fmt.Errorf("%q is a draft", rel.GetTagName())
	}

	if rel.GetPrerelease() {
		return nil, fmt.Errorf("%q is a pre-release", rel.GetTagName())
	}

	asset, err := findAssetFromRelease(rel, up.filters)
	if err != nil {
		return nil, fmt.Errorf("failed to find release and asset: %w", err)
	}

	ver, err := semver.ParseTolerant(rel.GetTagName())
	if err != nil {
		return nil, fmt.Errorf("failed to parse version: %w", err)
	}

	publishedAt := rel.GetPublishedAt().Time
	release := &Release{
		Version:           ver,
		AssetURL:          asset.GetBrowserDownloadURL(),
		AssetByteSize:     asset.GetSize(),
		AssetID:           asset.GetID(),
		ValidationAssetID: -1,
		URL:               rel.GetHTMLURL(),
		ReleaseNotes:      rel.GetBody(),
		Name:              rel.GetName(),
		PublishedAt:       &publishedAt,
		RepoOwner:         owner,
		RepoName:          name,
	}

	if up.validator != nil {
		validationName := asset.GetName() + up.validator.Suffix()
		validationAsset, ok := findValidationAsset(rel, validationName)
		if !ok {
			return nil, fmt.Errorf("Failed finding validation file %q", validationName)
		}

		release.ValidationAssetID = validationAsset.GetID()
	}

	return release, nil
}

func findRelease(rels []*github.RepositoryRelease, version string) (*github.RepositoryRelease, error) {
	if len(rels) == 0 {
		return nil, fmt.Errorf("no releases found")
	}

	if version != "" {
		// Find the release with the given version.
		rel, ok := lo.Find(rels, func(rel *github.RepositoryRelease) bool {
			return rel.GetTagName() == version
		})
		if !ok {
			return nil, fmt.Errorf("release %q not found", version)
		}

		return rel, nil
	}

	// filter out drafts and pre-releases
	rels = lo.Filter(rels, func(rel *github.RepositoryRelease, i int) bool {
		return !rel.GetDraft() && !rel.GetPrerelease()
	})

	if len(rels) == 0 {
		return nil, fmt.Errorf("no published releases found")
	}

	// sort by version
	sort.SliceStable(rels, func(i, j int) bool {
		tag1 := rels[i].GetTagName()
		tag2 := rels[j].GetTagName()

		// If the tag name is not a valid semver, use the published date.
		ver1, err1 := semver.ParseTolerant(tag1)
		ver2, err2 := semver.ParseTolerant(tag2)
		switch {
		case err1 == nil && err2 == nil:
			return ver1.GTE(ver2)
		case err1 == nil:
			return true
		case err2 == nil:
			return false
		default:
			// If both are not valid semver, use the published date.
			return rels[i].GetPublishedAt().Time.After(rels[j].GetPublishedAt().Time)
		}
	})

	// take the first item
	return rels[0], nil
}

// DetectLatest detects the latest release of the repository.
// This function is a shortcut version of updater.DetectLatest() method.
func DetectLatest(owner, name string) (*Release, error) {
	return DefaultUpdater().DetectLatest(owner, name)
}

// DetectVersion detects the given release of the repository from its version.
func DetectVersion(owner, name, version string) (*Release, error) {
	return DefaultUpdater().DetectVersion(owner, name, version)
}
