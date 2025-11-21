package gitattr

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/git-lfs/git-lfs/v3/filepathfilter"
	"github.com/git-lfs/git-lfs/v3/git/core"
	"github.com/git-lfs/git-lfs/v3/tools"
	"github.com/git-lfs/git-lfs/v3/tr"
	"github.com/rubyist/tracerx"
)

const (
	LockableAttrib = "lockable"
	FilterAttrib   = "filter"
)

// AttributePath is a path entry in a gitattributes file which has the LFS filter
type AttributePath struct {
	// Path entry in the attribute file
	Path string
	// The attribute file which was the source of this entry
	Source *AttributeSource
	// Path also has the 'lockable' attribute
	Lockable bool
	// Path is handled by Git LFS (i.e., filter=lfs)
	Tracked bool
}

type AttributeSource struct {
	Path       string
	LineEnding string
}

type attrFile struct {
	path       string
	readMacros bool
}

func (s *AttributeSource) String() string {
	return s.Path
}

// GetRepoAttributePaths behaves as GetAttributePaths, and loads information
// only from the repo attributes file.
func GetUserAttributePaths(mp *MacroProcessor, gitEnv core.Environment) ([]AttributePath, error) {
	reader, path, err := GetUserAttributesFile(gitEnv)
	if err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, nil
	}
	// The working directory for the user gitattributes file is blank.
	return AttrPathsFromReader(mp, path, "", reader, true), nil
}

func GetUserAttributesFile(gitEnv core.Environment) (io.Reader, string, error) {
	path, _ := gitEnv.Get("core.attributesfile")
	path, err := tools.ExpandConfigPath(path, "git/attributes")
	if err != nil {
		return nil, "", err
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, "", nil
	}

	reader, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer reader.Close()
	return reader, path, nil
}

func GetRepoAttributeFile(gitDir string) (io.Reader, string, error) {
	path := filepath.Join(gitDir, "info", "attributes")

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, "", nil
	}

	reader, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer reader.Close()
	return reader, path, nil
}

// GetSystemAttributePaths behaves as GetAttributePaths, and loads information
// only from the system gitattributes file, respecting the $PREFIX environment
// variable.
func GetSystemAttributePaths(mp *MacroProcessor, env core.Environment) ([]AttributePath, error) {
	reader, path, err := GetSystemAttributesFile(env)
	if err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, nil
	}
	// The working directory for the system gitattributes file is blank.
	return AttrPathsFromReader(mp, path, "", reader, true), nil
}

func GetSystemAttributesFile(osEnv core.Environment) (io.Reader, string, error) {
	var path string
	if core.IsGitVersionAtLeast("2.42.0") {
		cmd, err := core.GitNoLFS("var", "GIT_ATTR_SYSTEM")
		if err != nil {
			return nil, "", errors.New(tr.Tr.Get("failed to find `git var GIT_ATTR_SYSTEM`: %v", err))
		}
		out, err := cmd.Output()
		if err != nil {
			return nil, "", errors.New(tr.Tr.Get("failed to call `git var GIT_ATTR_SYSTEM`: %v", err))
		}
		paths := strings.Split(string(out), "\n")
		if len(paths) == 0 {
			return nil, "", nil
		}
		path = paths[0]
	} else {
		prefix, _ := osEnv.Get("PREFIX")
		if len(prefix) == 0 {
			prefix = string(filepath.Separator)
		}

		path = filepath.Join(prefix, "etc", "gitattributes")
	}

	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, "", nil
	}

	reader, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer reader.Close()
	return reader, path, nil
}

// GetAttributePaths returns a list of entries in .gitattributes which are
// configured with the filter=lfs attribute
// workingDir is the root of the working copy
// gitDir is the root of the git repo
func GetAttributePaths(mp *MacroProcessor, workingDir, gitDir string) []AttributePath {
	paths := make([]AttributePath, 0)

	for _, file := range findAttributeFiles(workingDir, gitDir) {
		paths = append(paths, attrPathsFromFile(mp, file.path, workingDir, file.readMacros)...)
	}

	return paths
}

func attrPathsFromFile(mp *MacroProcessor, path, workingDir string, readMacros bool) []AttributePath {
	attributes, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer attributes.Close()
	return AttrPathsFromReader(mp, path, workingDir, attributes, readMacros)
}

func AttrPathsFromReader(mp *MacroProcessor, fpath, workingDir string, rdr io.Reader, readMacros bool) []AttributePath {
	relfile, _ := filepath.Rel(workingDir, fpath)
	// Go 1.20 now always returns ".\foo" instead of "foo" in filepath.Rel,
	// but only on Windows.  Strip the extra dot here so our paths are
	// always fully relative with no "." or ".." components.
	reldir := filepath.ToSlash(tools.TrimCurrentPrefix(filepath.Dir(relfile)))
	if reldir == "." {
		reldir = ""
	}

	lines, eol, err := ParseLines(rdr)
	if err != nil {
		return nil
	}

	patternLines := mp.ProcessLines(lines, readMacros)

	return patternLinesToAttrPaths(patternLines, relfile, reldir, workingDir, eol)
}

func patternLinesToAttrPaths(patternLines []PatternLine, relfile, reldir, workingDir, eol string) []AttributePath {
	var paths []AttributePath
	source := &AttributeSource{Path: relfile}
	source.LineEnding = eol

	for _, line := range patternLines {
		lockable := false
		tracked := false
		hasFilter := false

		for _, attr := range line.Attrs() {
			if attr.K == FilterAttrib {
				hasFilter = true
				tracked = attr.V == "lfs"
			} else if attr.K == LockableAttrib && attr.V == "true" {
				lockable = true
			}
		}

		if !hasFilter && !lockable {
			continue
		}

		pattern := line.Pattern().String()
		if len(workingDir) > 0 {
			pattern = filepath.Join(reldir, pattern)
		}

		paths = append(paths, AttributePath{
			Path:     pattern,
			Source:   source,
			Lockable: lockable,
			Tracked:  tracked,
		})
	}
	return paths
}

// GetAttributeFilter returns a list of entries in .gitattributes which are
// configured with the filter=lfs attribute as a file path filter which
// file paths can be matched against
// workingDir is the root of the working copy
// gitDir is the root of the git repo
func GetAttributeFilter(workingDir, gitDir string) *filepathfilter.Filter {
	paths := GetAttributePaths(NewMacroProcessor(), workingDir, gitDir)
	patterns := make([]filepathfilter.Pattern, 0, len(paths))

	for _, path := range paths {
		// Convert all separators to `/` before creating a pattern to
		// avoid characters being escaped in situations like `subtree\*.md`
		patterns = append(patterns, filepathfilter.NewPattern(filepath.ToSlash(path.Path), filepathfilter.GitAttributes))
	}

	return filepathfilter.NewFromPatterns(patterns, nil)
}

func findAttributeFiles(workingDir, gitDir string) []attrFile {
	var paths []attrFile

	repoAttributes := filepath.Join(gitDir, "info", "attributes")
	if info, err := os.Stat(repoAttributes); err == nil && !info.IsDir() {
		paths = append(paths, attrFile{path: repoAttributes, readMacros: true})
	}

	lsFiles, err := core.NewLsFiles(workingDir, true, true)
	if err != nil {
		tracerx.Printf("Error finding .gitattributes: %v", err)
		return paths
	}

	if gitattributesFiles, present := lsFiles.FilesByName[".gitattributes"]; present {
		for _, f := range gitattributesFiles {
			tracerx.Printf("findAttributeFiles: located %s", f.FullPath)
			paths = append(paths, attrFile{
				path:       filepath.Join(workingDir, f.FullPath),
				readMacros: f.FullPath == ".gitattributes", // Read macros from the top-level attributes
			})
		}
	}

	// reverse the order of the files so more specific entries are found first
	// when iterating from the front (respects precedence)
	sort.Slice(paths[:], func(i, j int) bool {
		return len(paths[i].path) > len(paths[j].path)
	})

	return paths
}
