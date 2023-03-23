package selfupdate

import (
	"fmt"
	"regexp"
	"runtime"
	"strings"

	"github.com/blang/semver"
	"github.com/google/go-github/v50/github"
)

var reVersion = regexp.MustCompile(`\d+\.\d+\.\d+`)

func findAssetFromRelease(rel *github.RepositoryRelease,
	suffixes []string, targetVersion string, filters []*regexp.Regexp,
) (*github.ReleaseAsset, semver.Version, error) {
	if targetVersion != "" && targetVersion != rel.GetTagName() {
		return nil, semver.Version{}, fmt.Errorf("%q does not match the target version (%s)", targetVersion, targetVersion)
	}

	if targetVersion == "" && rel.GetDraft() {
		return nil, semver.Version{}, fmt.Errorf("target version %q is a draft", rel.GetTagName())
	}

	if targetVersion == "" && rel.GetPrerelease() {
		return nil, semver.Version{}, fmt.Errorf("target version %q is a pre-release", rel.GetTagName())
	}

	verText := rel.GetTagName()
	indices := reVersion.FindStringIndex(verText)
	if indices == nil {
		return nil, semver.Version{}, fmt.Errorf("version %q does not adopt semantic versioning", verText)
	}
	if indices[0] > 0 {
		log.Println("Strip prefix of version", verText[:indices[0]], "from", verText)
		verText = verText[indices[0]:]
	}

	// If semver cannot parse the version text, it means that the text is not adopting
	// the semantic versioning. So it should be skipped.
	ver, err := semver.Make(verText)
	if err != nil {
		return nil, semver.Version{}, fmt.Errorf("version %q does not adopt semantic versioning: %w", verText, err)
	}

	for _, asset := range rel.Assets {
		name := asset.GetName()
		if len(filters) > 0 {
			// if some filters are defined, match them: if any one matches, the asset is selected
			matched := false
			for _, filter := range filters {
				if filter.MatchString(name) {
					log.Println("Selected filtered asset", name)
					matched = true
					break
				}
				log.Printf("Skipping asset %q not matching filter %v\n", name, filter)
			}
			if !matched {
				continue
			}
		}

		for _, s := range suffixes {
			if strings.HasSuffix(name, s) { // require version, arch etc
				// default: assume single artifact
				return asset, ver, nil
			}
		}
	}

	return nil, semver.Version{}, fmt.Errorf(
		"no suitable asset was found in release %q", rel.GetTagName())
}

func findValidationAsset(rel *github.RepositoryRelease, validationName string) (*github.ReleaseAsset, bool) {
	for _, asset := range rel.Assets {
		if asset.GetName() == validationName {
			return asset, true
		}
	}
	return nil, false
}

func findReleaseAndAsset(rels []*github.RepositoryRelease,
	targetVersion string,
	filters []*regexp.Regexp,
) (*github.RepositoryRelease, *github.ReleaseAsset, semver.Version, error) {
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

	var ver semver.Version
	var asset *github.ReleaseAsset
	var release *github.RepositoryRelease

	// Find the latest version from the list of releases.
	// Returned list from GitHub API is in the order of the date when created.
	//   ref: https://github.com/rhysd/go-github-selfupdate/issues/11
	for _, rel := range rels {
		a, v, err := findAssetFromRelease(rel, suffixes, targetVersion, filters)
		if err != nil {
			return nil, nil, ver, fmt.Errorf("could not find asset from release %q for %s %s: %w",
				rel.GetTagName(), runtime.GOOS, runtime.GOARCH, err)
		}

		// Note: any version with suffix is less than any version without suffix.
		// e.g. 0.0.1 > 0.0.1-beta
		if release == nil || v.GTE(ver) {
			ver = v
			asset = a
			release = rel
		}
	}

	if release == nil {
		return nil, nil, semver.Version{}, fmt.Errorf("could not find any release for %s and %s", runtime.GOOS, runtime.GOARCH)
	}

	return release, asset, ver, nil
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
		log.Println("API returned an error response:", err)
		if res != nil && res.StatusCode == 404 {
			return nil, fmt.Errorf("repository or release not found: %w", err)
		}

		return nil, fmt.Errorf("failed to fetch releases: %w", err)
	}

	rel, asset, ver, err := findReleaseAndAsset(rels, version, up.filters)
	if err != nil {
		return nil, fmt.Errorf("failed to find release and asset: %w", err)
	}

	url := asset.GetBrowserDownloadURL()
	log.Println("Successfully fetched the latest release. tag:", rel.GetTagName(), ", name:", rel.GetName(), ", URL:", rel.GetURL(), ", Asset:", url)

	publishedAt := rel.GetPublishedAt().Time
	release := &Release{
		Version:           ver,
		AssetURL:          url,
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

// DetectLatest detects the latest release of the repository.
// This function is a shortcut version of updater.DetectLatest() method.
func DetectLatest(owner, name string) (*Release, error) {
	return DefaultUpdater().DetectLatest(owner, name)
}

// DetectVersion detects the given release of the repository from its version.
func DetectVersion(owner, name, version string) (*Release, error) {
	return DefaultUpdater().DetectVersion(owner, name, version)
}
