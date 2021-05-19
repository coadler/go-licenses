// Copyright 2019 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package licenses

import (
	"context"
	"encoding/xml"
	"fmt"
	"go/build"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/golang/glog"
	"golang.org/x/tools/go/packages"
)

var (
	// TODO(RJPercival): Support replacing "master" with Go Module version
	repoPathPrefixes = map[string]string{
		"github.com":          "blob/master/",
		"bitbucket.org":       "src/master/",
		"go.googlesource.com": "+/refs/heads/master/",
	}
)

// Library is a collection of packages covered by the same license file.
type Library struct {
	// LicensePath is the path of the file containing the library's license.
	LicensePath string
	// Packages contains import paths for Go packages in this library.
	// It may not be the complete set of all packages in the library.
	Packages []string
}

// PackagesError aggregates all Packages[].Errors into a single error.
type PackagesError struct {
	pkgs []*packages.Package
}

func (e PackagesError) Error() string {
	var str strings.Builder
	str.WriteString(fmt.Sprintf("errors for %q:", e.pkgs))
	packages.Visit(e.pkgs, nil, func(pkg *packages.Package) {
		for _, err := range pkg.Errors {
			str.WriteString(fmt.Sprintf("\n%s: %s", pkg.PkgPath, err))
		}
	})
	return str.String()
}

// Libraries returns the collection of libraries used by this package, directly or transitively.
// A library is a collection of one or more packages covered by the same license file.
// Packages not covered by a license will be returned as individual libraries.
// Standard library packages will be ignored.
func Libraries(ctx context.Context, classifier Classifier, importPaths ...string) ([]*Library, error) {
	cfg := &packages.Config{
		Context: ctx,
		Mode:    packages.NeedImports | packages.NeedDeps | packages.NeedFiles | packages.NeedName,
	}

	rootPkgs, err := packages.Load(cfg, importPaths...)
	if err != nil {
		return nil, err
	}

	pkgs := map[string]*packages.Package{}
	pkgsByLicense := make(map[string][]*packages.Package)
	errorOccurred := false
	packages.Visit(rootPkgs, func(p *packages.Package) bool {
		if len(p.Errors) > 0 {
			errorOccurred = true
			return false
		}
		if isStdLib(p) {
			// No license requirements for the Go standard library.
			return false
		}
		if len(p.OtherFiles) > 0 {
			glog.Warningf("%q contains non-Go code that can't be inspected for further dependencies:\n%s", p.PkgPath, strings.Join(p.OtherFiles, "\n"))
		}
		var pkgDir string
		switch {
		case len(p.GoFiles) > 0:
			pkgDir = filepath.Dir(p.GoFiles[0])
		case len(p.CompiledGoFiles) > 0:
			pkgDir = filepath.Dir(p.CompiledGoFiles[0])
		case len(p.OtherFiles) > 0:
			pkgDir = filepath.Dir(p.OtherFiles[0])
		default:
			// This package is empty - nothing to do.
			return true
		}
		licensePath, err := Find(pkgDir, classifier)
		if err != nil {
			glog.Errorf("Failed to find license for %s: %v", p.PkgPath, err)
		}
		pkgs[p.PkgPath] = p
		pkgsByLicense[licensePath] = append(pkgsByLicense[licensePath], p)
		return true
	}, nil)
	if errorOccurred {
		return nil, PackagesError{
			pkgs: rootPkgs,
		}
	}

	var libraries []*Library
	for licensePath, pkgs := range pkgsByLicense {
		if licensePath == "" {
			// No license for these packages - return each one as a separate library.
			for _, p := range pkgs {
				libraries = append(libraries, &Library{
					Packages: []string{p.PkgPath},
				})
			}
			continue
		}
		lib := &Library{
			LicensePath: licensePath,
		}
		for _, pkg := range pkgs {
			lib.Packages = append(lib.Packages, pkg.PkgPath)
		}
		libraries = append(libraries, lib)
	}
	return libraries, nil
}

// Name is the common prefix of the import paths for all of the packages in this library.
func (l *Library) Name() string {
	return commonAncestor(l.Packages)
}

func commonAncestor(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	if len(paths) == 1 {
		return paths[0]
	}
	sort.Strings(paths)
	min, max := paths[0], paths[len(paths)-1]
	lastSlashIndex := 0
	for i := 0; i < len(min) && i < len(max); i++ {
		if min[i] != max[i] {
			return min[:lastSlashIndex]
		}
		if min[i] == '/' {
			lastSlashIndex = i
		}
	}
	return min
}

func (l *Library) String() string {
	return l.Name()
}

// FileURL attempts to determine the URL for a file in this library.
// This only works for certain supported package prefixes, such as github.com,
// bitbucket.org and googlesource.com. Prefer GitRepo.FileURL() if possible.
func (l *Library) FileURL(filePath string) (*url.URL, error) {
	relFilePath, err := filepath.Rel(filepath.Dir(l.LicensePath), filePath)
	if err != nil {
		return nil, err
	}

	lurl, err := url.Parse(l.Name())
	if err != nil {
		return nil, err
	}

	lurl.Scheme = "https"
	q := lurl.Query()
	q.Add("go-get", "1")
	lurl.RawQuery = q.Encode()

	imps, err := repoFromVanityURL(lurl.String())
	if err != nil {
		return nil, err
	}

	if len(imps) != 1 {
		nameParts := strings.SplitN(l.Name(), "/", 4)
		if len(nameParts) < 3 {
			return nil, fmt.Errorf("cannot determine URL for %q package", l.Name())
		}
		host, user, project := nameParts[0], nameParts[1], nameParts[2]
		pathPrefix, ok := repoPathPrefixes[host]
		if !ok {
			return nil, fmt.Errorf("unsupported package host %q for %q", host, l.Name())
		}
		if len(nameParts) == 4 {
			pathPrefix = path.Join(pathPrefix, nameParts[3])
		}

		return &url.URL{
			Scheme: "https",
			Host:   host,
			Path:   path.Join(user, project, pathPrefix, relFilePath),
		}, nil
	}

	name := strings.TrimSuffix(strings.TrimPrefix(imps[0].RepoRoot, "https://"), ".git")
	nameParts := strings.SplitN(name, "/", 2)
	if len(nameParts) < 2 {
		return nil, fmt.Errorf("cannot determine URL for %q package", imps[0].RepoRoot)
	}

	host := nameParts[0]
	pathPrefix, ok := repoPathPrefixes[host]
	if !ok {
		return nil, fmt.Errorf("unsupported package host %q for %q", host, imps[0].RepoRoot)
	}
	if len(nameParts) == 4 {
		pathPrefix = path.Join(pathPrefix, nameParts[3])
	}

	return &url.URL{
		Scheme: "https",
		Host:   host,
		Path:   path.Join(nameParts[1], pathPrefix, relFilePath),
	}, nil
}

// isStdLib returns true if this package is part of the Go standard library.
func isStdLib(pkg *packages.Package) bool {
	if len(pkg.GoFiles) == 0 {
		return false
	}
	return strings.HasPrefix(pkg.GoFiles[0], build.Default.GOROOT)
}

func repoFromVanityURL(url string) ([]metaImport, error) {
	res, err := http.Get(url)
	if err != nil {
		return nil, err
	}

	defer res.Body.Close()
	return parseMetaGoImports(res.Body, true)
}

type metaImport struct {
	Prefix, VCS, RepoRoot string
}

// charsetReader returns a reader for the given charset. Currently
// it only supports UTF-8 and ASCII. Otherwise, it returns a meaningful
// error which is printed by go get, so the user can find why the package
// wasn't downloaded if the encoding is not supported. Note that, in
// order to reduce potential errors, ASCII is treated as UTF-8 (i.e. characters
// greater than 0x7f are not rejected).
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(charset) {
	case "ascii":
		return input, nil
	default:
		return nil, fmt.Errorf("can't decode XML document using charset %q", charset)
	}
}

// parseMetaGoImports returns meta imports from the HTML in r.
// Parsing ends at the end of the <head> section or the beginning of the <body>.
func parseMetaGoImports(r io.Reader, preferMod bool) (imports []metaImport, err error) {
	d := xml.NewDecoder(r)
	d.CharsetReader = charsetReader
	d.Strict = false
	var t xml.Token
	for {
		t, err = d.RawToken()
		if err != nil {
			if err == io.EOF || len(imports) > 0 {
				err = nil
			}
			break
		}
		if e, ok := t.(xml.StartElement); ok && strings.EqualFold(e.Name.Local, "body") {
			break
		}
		if e, ok := t.(xml.EndElement); ok && strings.EqualFold(e.Name.Local, "head") {
			break
		}
		e, ok := t.(xml.StartElement)
		if !ok || !strings.EqualFold(e.Name.Local, "meta") {
			continue
		}
		if attrValue(e.Attr, "name") != "go-import" {
			continue
		}
		if f := strings.Fields(attrValue(e.Attr, "content")); len(f) == 3 {
			imports = append(imports, metaImport{
				Prefix:   f[0],
				VCS:      f[1],
				RepoRoot: f[2],
			})
		}
	}

	// Extract mod entries if we are paying attention to them.
	var list []metaImport
	var have map[string]bool
	if preferMod {
		have = make(map[string]bool)
		for _, m := range imports {
			if m.VCS == "mod" {
				have[m.Prefix] = true
				list = append(list, m)
			}
		}
	}

	// Append non-mod entries, ignoring those superseded by a mod entry.
	for _, m := range imports {
		if m.VCS != "mod" && !have[m.Prefix] {
			list = append(list, m)
		}
	}
	return list, nil
}

// attrValue returns the attribute value for the case-insensitive key
// `name', or the empty string if nothing is found.
func attrValue(attrs []xml.Attr, name string) string {
	for _, a := range attrs {
		if strings.EqualFold(a.Name.Local, name) {
			return a.Value
		}
	}
	return ""
}
