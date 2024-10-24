package vex

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/openvex/go-vex/pkg/vex"
	"github.com/package-url/packageurl-go"
	"github.com/samber/oops"

	"github.com/aquasecurity/vexhub-crawler/pkg/download"
	"github.com/aquasecurity/vexhub-crawler/pkg/manifest"
	xurl "github.com/aquasecurity/vexhub-crawler/pkg/url"
)

var (
	errPURLMismatch = fmt.Errorf("PURL does not match")
	errNoStatement  = fmt.Errorf("no statements found")
)

func CrawlPackage(ctx context.Context, vexHubDir string, url *xurl.URL, purl packageurl.PackageURL) error {
	errBuilder := oops.In("crawl").With("purl", purl.String()).With("url", url)
	tmpDir, err := os.MkdirTemp("", "vexhub-crawler-*")
	if err != nil {
		return errBuilder.Wrapf(err, "failed to create a temporary directory")
	}
	defer os.RemoveAll(tmpDir)

	dst := filepath.Join(tmpDir, purl.Name)
	if err = download.Download(ctx, url.GetterString(), dst); err != nil {
		return errBuilder.Wrapf(err, "download error")
	}

	permaLink := githubPermalink(dst)
	if permaLink != nil {
		errBuilder.With("permalink", permaLink.String())
	}

	vexDir := filepath.Join(vexHubDir, "pkg", purl.Type, purl.Namespace, purl.Name, purl.Subpath)
	if purl.Type == packageurl.TypeOCI {
		name := purl.Qualifiers.Map()["repository_url"]
		vexDir = filepath.Join(vexHubDir, "pkg", purl.Type, name)
	}
	vexDir = filepath.Clean(filepath.ToSlash(vexDir))
	errBuilder = errBuilder.With("dir", vexDir)

	// Reset the directory
	if err = resetDir(vexDir); err != nil {
		return errBuilder.Wrapf(err, "failed to reset the directory")
	}

	var found bool
	var sources []manifest.Source
	logger := slog.With(slog.String("purl", purl.String()), "url", url)

	root := filepath.Join(dst, url.Subdirs())
	if _, err := os.Stat(filepath.Join(root, ".vex")); err == nil {
		root = filepath.Join(root, ".vex") // If the directory contains a .vex directory, use it as the root
	}
	err = filepath.WalkDir(root, func(filePath string, d fs.DirEntry, err error) error {
		if err != nil {
			return errBuilder.Wrapf(err, "failed to walk the directory")
		} else if d.IsDir() {
			return nil
		} else if !matchPath(filePath) {
			return nil
		}

		relPath, err := filepath.Rel(dst, filePath) // Relative path from the repository root, not from ".vex/"
		if err != nil {
			return errBuilder.With("file_path", filePath).Wrapf(err, "failed to get the relative path")
		}

		logger.Info("Parsing VEX file", slog.String("path", relPath))
		if err = validateVEX(filePath, purl.String()); errors.Is(err, errNoStatement) {
			return errBuilder.With("path", relPath).Wrapf(err, "no statement found")
		} else if errors.Is(err, errPURLMismatch) {
			logger.Info("PURL does not match", slog.String("path", relPath))
			return nil
		} else if err != nil {
			return errBuilder.Wrapf(err, "failed to validate VEX file")
		}

		found = true
		to := filepath.Join(vexDir, filepath.Base(filePath))
		if err = os.Rename(filePath, to); err != nil {
			return errBuilder.With("from", filePath).With("to", to).Wrapf(err, "failed to rename")
		}

		if src := fileSource(relPath, url, permaLink); src != nil {
			sources = append(sources, *src)
		}

		return nil
	})
	if err != nil {
		return errBuilder.Wrapf(err, "failed to walk the directory")
	}

	if !found {
		return errBuilder.Errorf("no VEX file found")
	}

	// Check if there are any changes in the VEX directory.
	// If there are no changes, we don't need to update the manifest.json file.
	// Since manifest.json has permalink pointing to the default branch,
	// it's frequently updated even if there are no changes in the VEX directory.
	if changed, err := hasVEXChanges(vexHubDir, vexDir); err == nil && !changed {
		logger.Info("No changes in the VEX directory")
		return nil
	}

	m := manifest.Manifest{
		ID:      purl.String(),
		Sources: sources,
	}
	if err = manifest.Write(filepath.Join(vexDir, manifest.FileName), m); err != nil {
		return oops.Wrapf(err, "failed to write sources")
	}

	return nil
}

func githubPermalink(repoDir string) *url.URL {
	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		return nil
	}

	r, err := repo.Remote("origin")
	if err != nil {
		return nil
	}

	urls := r.Config().URLs
	if len(urls) == 0 {
		return nil
	}
	u, err := url.Parse(urls[0])
	if err != nil || u.Host != "github.com" {
		return nil
	}
	p, _, ok := strings.Cut(u.Path, ".git")
	if !ok {
		return nil
	}
	head, err := repo.Head()
	if err != nil {
		return nil
	}

	// e.g. https://github.com/aquasecurity/vextest/blob/ed76fc6c0e8e56318ce3148bd7bd938aad41491c/
	u.Path = path.Join(p, "blob", head.Hash().String())

	u.Scheme = "https"
	u.User = nil
	u.RawQuery = ""
	return u
}

func matchPath(path string) bool {
	path = filepath.Base(path)
	if path == "openvex.json" || path == "vex.json" ||
		strings.HasSuffix(path, ".openvex.json") || strings.HasSuffix(path, ".vex.json") {
		return true
	}
	return false
}

func validateVEX(path, purl string) error {
	v, err := vex.Open(path)
	if err != nil {
		return oops.Wrapf(err, "failed to open VEX file")
	} else if len(v.Statements) == 0 {
		return errNoStatement
	}
	for _, statement := range v.Statements {
		for _, product := range statement.Products {
			if vex.PurlMatches(purl, product.ID) {
				return nil
			}
		}
	}
	return errPURLMismatch
}

func fileSource(relPath string, url *xurl.URL, permaLink *url.URL) *manifest.Source {
	source := manifest.Source{
		Path: filepath.Base(relPath),
		URL:  url.String(),
	}
	if permaLink != nil {
		l := *permaLink
		l.Path = path.Join(l.Path, relPath)
		source.URL = l.String()
	}
	return &source
}

// resetDir removes all files other than manifest.json in the directory and creates a new directory.
func resetDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return oops.Wrapf(err, "failed to read the directory")
	}
	for _, entry := range entries {
		if !entry.IsDir() && entry.Name() == manifest.FileName {
			continue
		}
		if err = os.RemoveAll(entry.Name()); err != nil {
			return oops.With("file_path", entry.Name()).Wrapf(err, "failed to remove the directory")
		}
	}
	if err = os.MkdirAll(dir, 0755); err != nil {
		return oops.With("dir", dir).Wrapf(err, "failed to create a director")
	}
	return nil
}

// hasVEXChanges checks if there are any changes in the .vex/ directory excluding the manifest.json file
func hasVEXChanges(vexHubDir, vexDir string) (bool, error) {
	errBuilder := oops.In("git_error").With("vex_hub_dir", vexHubDir).With("dir", vexDir)
	// Open the repository
	repo, err := git.PlainOpen(vexHubDir)
	if err != nil {
		return false, errBuilder.Wrapf(err, "open git repository")
	}

	// Get the worktree
	wt, err := repo.Worktree()
	if err != nil {
		return false, errBuilder.Wrapf(err, "git worktree")
	}

	// Get the current status
	status, err := wt.Status()
	if err != nil {
		return false, errBuilder.Wrapf(err, "git status")
	}

	// Get the relative path of vexDir from vexHubDir
	relVexDir, err := filepath.Rel(vexHubDir, vexDir)
	if err != nil {
		return false, errBuilder.Wrapf(err, "relative path")
	}

	// Check for changes in vexDir excluding manifest.json
	for filePath, fileStatus := range status {
		// Check if the file is within vexDir
		if strings.HasPrefix(filePath, relVexDir) {
			// Exclude manifest.json
			if filepath.Base(filePath) != manifest.FileName && fileStatus.Worktree != git.Unmodified {
				return true, nil
			}
		}
	}

	return false, nil
}
