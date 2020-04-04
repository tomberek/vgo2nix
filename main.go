package main // import "github.com/adisbladis/vgo2nix"

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"golang.org/x/tools/go/vcs"
	"io"
	"math"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

type Package struct {
	GoPackagePath string
	URL           string
	Rev           string
	Sha256        string
}

type PackageResult struct {
	Package *Package
	Error   error
}

type modEntry struct {
	importPath string
	rev        string
}

const depNixFormat = `  {
    goPackagePath = "%s";
    fetch = {
      type = "%s";
      url = "%s";
      rev = "%s";
      sha256 = "%s";
    };
  }`

func getModules() ([]*modEntry, error) {
	var entries []*modEntry

	commitShaRev := regexp.MustCompile(`^v\d+\.\d+\.\d+-(?:\d+\.)?[0-9]{14}-(.*?)$`)
	commitRevV2 := regexp.MustCompile("^v.*-(.{12})\\+incompatible$")
	commitRevV3 := regexp.MustCompile(`^(v\d+\.\d+\.\d+)\+incompatible$`)

	var stderr bytes.Buffer
	cmd := exec.Command("go", "list", "-json", "-m", "all")
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(),
		"GO111MODULE=on",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	type goMod struct {
		Path    string
		Main    bool
		Version string
	}

	var mods []goMod
	dec := json.NewDecoder(stdout)
	for {
		var mod goMod
		if err := dec.Decode(&mod); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}

		if !mod.Main {
			mods = append(mods, mod)
		}
	}

	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("'go list -m all' failed with %s:\n%s", err, stderr.String())
	}

	for _, mod := range mods {
		rev := mod.Version
		if commitShaRev.MatchString(rev) {
			rev = commitShaRev.FindAllStringSubmatch(rev, -1)[0][1]
		} else if commitRevV2.MatchString(rev) {
			rev = commitRevV2.FindAllStringSubmatch(rev, -1)[0][1]
		} else if commitRevV3.MatchString(rev) {
			rev = commitRevV3.FindAllStringSubmatch(rev, -1)[0][1]
		}
		fmt.Println(fmt.Sprintf("goPackagePath %s has rev %s", mod.Path, rev))
		entries = append(entries, &modEntry{
			importPath: mod.Path,
			rev:        rev,
		})
	}

	return entries, nil
}

func getPackages(keepGoing bool, numJobs int, prevDeps map[string]*Package, urlPrefixReplacements map[string]string) ([]*Package, error) {
	entries, err := getModules()
	if err != nil {
		return nil, err
	}

	processEntry := func(entry *modEntry) (*Package, error) {
		wrapError := func(err error) error {
			return fmt.Errorf("Error processing import path \"%s\": %v", entry.importPath, err)
		}

		repoRoot, err := vcs.RepoRootForImportPath(
			entry.importPath,
			false)
		if err != nil {
			return nil, wrapError(err)
		}
		goPackagePath := repoRoot.Root

		if prevPkg, ok := prevDeps[goPackagePath]; ok {
			if prevPkg.Rev == entry.rev {
				return prevPkg, nil
			}
		}

		for prefix, replacement := range urlPrefixReplacements {
			if strings.HasPrefix(repoRoot.Repo, prefix) {
				repoRoot.Repo = strings.Replace(repoRoot.Repo, prefix, replacement, 1)
				break
			}
		}

		fmt.Println(fmt.Sprintf("Fetching %s", goPackagePath))
		// The options for nix-prefetch-git need to match how buildGoPackage
		// calls fetchgit:
		// https://github.com/NixOS/nixpkgs/blob/8d8e56824de52a0c7a64d2ad2c4ed75ed85f446a/pkgs/development/go-modules/generic/default.nix#L54-L56
		// and fetchgit's defaults:
		// https://github.com/NixOS/nixpkgs/blob/8d8e56824de52a0c7a64d2ad2c4ed75ed85f446a/pkgs/build-support/fetchgit/default.nix#L15-L23
		jsonOut, err := exec.Command(
			"nix-prefetch-git",
			"--quiet",
			"--fetch-submodules",
			"--url", repoRoot.Repo,
			"--rev", entry.rev).Output()
		fmt.Println(fmt.Sprintf("Finished fetching %s", goPackagePath))

		if err != nil {
			return nil, wrapError(err)
		}
		var resp map[string]interface{}
		if err := json.Unmarshal(jsonOut, &resp); err != nil {
			return nil, wrapError(err)
		}
		sha256 := resp["sha256"].(string)

		if sha256 == "0sjjj9z1dhilhpc8pq4154czrb79z9cm044jvn75kxcjv6v5l2m5" {
			return nil, wrapError(fmt.Errorf("Bad SHA256 for repo %s with rev %s", repoRoot.Repo, entry.rev))
		}

		rev, ok := resp["rev"].(string)
		if !ok {
			return nil, wrapError(fmt.Errorf("Unable to read rev"))
		}

		return &Package{
			GoPackagePath: repoRoot.Root,
			URL:           repoRoot.Repo,
			Rev:           rev,
			Sha256:        sha256,
		}, nil
	}

	worker := func(entries <-chan *modEntry, results chan<- *PackageResult) {
		for entry := range entries {
			pkg, err := processEntry(entry)
			result := &PackageResult{
				Package: pkg,
				Error:   err,
			}
			results <- result
		}
	}

	jobs := make(chan *modEntry, len(entries))
	results := make(chan *PackageResult, len(entries))
	for w := 1; w <= int(math.Min(float64(len(entries)), float64(numJobs))); w++ {
		go worker(jobs, results)
	}

	for _, entry := range entries {
		jobs <- entry
	}
	close(jobs)

	pkgsMap := make(map[string]*Package)
	for j := 1; j <= len(entries); j++ {
		result := <-results
		if result.Error != nil {
			if !keepGoing {
				return nil, result.Error
			}
			msg := fmt.Sprintf("Encountered error: %v", result.Error)
			fmt.Println(msg)
			continue
		}
		pkgsMap[result.Package.GoPackagePath] = result.Package
	}

	// Make output order stable
	var packages []*Package

	keys := make([]string, 0, len(pkgsMap))
	for k := range pkgsMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		packages = append(packages, pkgsMap[k])
	}

	return packages, nil
}

type urlMap map[string]string

func (m urlMap) Set(argStr string) error {
	pieces := strings.SplitN(argStr, "=", 2)
	if len(pieces) < 2 {
		return fmt.Errorf(`URL map entry %+v must contain a '=' character`, argStr)
	}
	key := pieces[0]
	value := pieces[1]
	_, alreadyPresent := m[key]
	if alreadyPresent {
		return fmt.Errorf(`URL map entry %+v present more than once`, key)
	}
	m[key] = value
	return nil
}
func (m urlMap) String() string {
	var b strings.Builder
	for k, v := range m {
		fmt.Fprintf(&b, "%v => %v; ", k, v)
	}
	return b.String()
}

func main() {
	var keepGoing = flag.Bool("keep-going", false, "Whether to panic or not if a rev cannot be resolved (default \"false\")")
	var goDir = flag.String("dir", "./", "Go project directory")
	var out = flag.String("outfile", "deps.nix", "deps.nix output file (relative to project directory)")
	var in = flag.String("infile", "deps.nix", "deps.nix input file (relative to project directory)")
	var jobs = flag.Int("jobs", 20, "Number of parallel jobs")
	var urlPrefixMap urlMap = make(urlMap)

	flag.Var(&urlPrefixMap, "urlPrefixMap", `Map of URL prefix changes, of form ORIG=NEW (example: "https://github.com/=git@github.com:")`)
	flag.Parse()

	err := os.Chdir(*goDir)
	if err != nil {
		panic(err)
	}

	// Load previous deps from deps.nix so we can reuse hashes for known revs
	prevDeps := loadDepsNix(*in)
	packages, err := getPackages(*keepGoing, *jobs, prevDeps, urlPrefixMap)
	if err != nil {
		panic(err)
	}

	outfile, err := os.Create(*out)
	if err != nil {
		panic(err)
	}
	defer func() {
		if err := outfile.Close(); err != nil {
			panic(err)
		}
	}()

	write := func(line string) {
		bytes := []byte(line + "\n")
		if _, err := outfile.Write(bytes); err != nil {
			panic(err)
		}
	}

	write("# file generated from go.mod using vgo2nix (https://github.com/adisbladis/vgo2nix)")
	write("[")
	for _, pkg := range packages {
		write(fmt.Sprintf(depNixFormat,
			pkg.GoPackagePath, "git", pkg.URL,
			pkg.Rev, pkg.Sha256))
	}
	write("]")

	fmt.Println(fmt.Sprintf("Wrote %s", *out))
}
