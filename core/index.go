package core

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	gitignore "github.com/sabhiram/go-gitignore"
	"github.com/spf13/viper"
	"github.com/vbauerster/mpb/v4"
	"github.com/vbauerster/mpb/v4/decor"
)

// Index is a representation of the index.toml file for referencing all the files in a pack.
type Index struct {
	HashFormat string
	Files      IndexFiles
	indexFile  string
	packRoot   string
}

// indexTomlRepresentation is the TOML representation of Index (Files must be converted)
type indexTomlRepresentation struct {
	HashFormat string                       `toml:"hash-format"`
	Files      indexFilesTomlRepresentation `toml:"files"`
}

// LoadIndex attempts to load the index file from a path
func LoadIndex(indexFile string) (Index, error) {
	// Decode as indexTomlRepresentation then convert to Index
	var rep indexTomlRepresentation
	if _, err := toml.DecodeFile(indexFile, &rep); err != nil {
		return Index{}, err
	}
	if len(rep.HashFormat) == 0 {
		rep.HashFormat = "sha256"
	}
	index := Index{
		HashFormat: rep.HashFormat,
		Files:      rep.Files.toMemoryRep(),
		indexFile:  indexFile,
		packRoot:   filepath.Dir(indexFile),
	}
	return index, nil
}

// RemoveFile removes a file from the index, given a file path
func (in *Index) RemoveFile(path string) error {
	relPath, err := in.RelIndexPath(path)
	if err != nil {
		return err
	}
	delete(in.Files, relPath)
	return nil
}

func (in *Index) updateFileHashGiven(path, format, hash string, markAsMetaFile bool) error {
	// Remove format if equal to index hash format
	if in.HashFormat == format {
		format = ""
	}

	// Find in index
	relPath, err := in.RelIndexPath(path)
	if err != nil {
		return err
	}
	in.Files.updateFileEntry(relPath, format, hash, markAsMetaFile)
	return nil
}

// updateFile calculates the hash for a given path and updates it in the index
func (in *Index) updateFile(path string) error {
	var hashString string
	if viper.GetBool("no-internal-hashes") {
		hashString = ""
	} else {
		f, err := os.Open(path)
		if err != nil {
			return err
		}

		// Hash usage strategy (may change):
		// Just use SHA256, overwrite existing hash regardless of what it is
		// May update later to continue using the same hash that was already being used
		h, err := GetHashImpl("sha256")
		if err != nil {
			_ = f.Close()
			return err
		}
		if _, err := io.Copy(h, f); err != nil {
			_ = f.Close()
			return err
		}
		err = f.Close()
		if err != nil {
			return err
		}
		hashString = h.HashToString(h.Sum(nil))
	}

	markAsMetaFile := false
	// If the file has an extension of pw.toml, set markAsMetaFile to true
	if strings.HasSuffix(filepath.Base(path), MetaExtension) {
		markAsMetaFile = true
	}

	return in.updateFileHashGiven(path, "sha256", hashString, markAsMetaFile)
}

// ResolveIndexPath turns a path from the index into a file path on disk
func (in Index) ResolveIndexPath(p string) string {
	return filepath.Join(in.packRoot, filepath.FromSlash(p))
}

// RelIndexPath turns a file path on disk into a path from the index
func (in Index) RelIndexPath(p string) (string, error) {
	rel, err := filepath.Rel(in.packRoot, p)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

var ignoreDefaults = []string{
	// Defaults (can be overridden with a negating pattern preceded with !)

	// Exclude Git metadata
	".git/**",
	".gitattributes",
	".gitignore",

	// Exclude macOS metadata
	".DS_Store",

	// Exclude exported CurseForge zip files
	"/*.zip",

	// Exclude exported Modrinth packs
	"*.mrpack",

	// Exclude packwiz binaries, if the user puts them in their pack folder
	"packwiz.exe",
	"packwiz", // Note: also excludes packwiz/ as a directory - you can negate this pattern if you want a directory called packwiz
}

func readGitignore(path string) (*gitignore.GitIgnore, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		// Check if it's a permission error or other serious issue
		if !os.IsNotExist(err) {
			fmt.Printf("Warning: Could not read .packwizignore file at %s: %v\n", path, err)
		}
		return gitignore.CompileIgnoreLines(ignoreDefaults...), false
	}

	s := strings.Split(string(data), "\n")
	var lines []string
	lines = append(lines, ignoreDefaults...)
	lines = append(lines, s...)
	return gitignore.CompileIgnoreLines(lines...), true
}

// WalkFollowSymlinks is similar to filepath.Walk but follows symbolic links
func WalkFollowSymlinks(root string, fn func(path string, info os.FileInfo, err error) error) error {
	return walkDirFollowSymlinks(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return fn(path, nil, err)
		}
		info, err := d.Info()
		if err != nil {
			return fn(path, nil, err)
		}
		return fn(path, info, nil)
	})
}

// WalkDirFollowSymlinks is a public version of walkDirFollowSymlinks for use across packages
func WalkDirFollowSymlinks(root string, fn func(path string, d os.DirEntry, err error) error) error {
	return walkDirFollowSymlinks(root, fn)
}

// walkDirFollowSymlinks is similar to filepath.WalkDir but follows symbolic links
func walkDirFollowSymlinks(root string, fn func(path string, d os.DirEntry, err error) error) error {
	visited := make(map[string]bool)

	var walk func(string) error
	walk = func(path string) error {
		// Get file info using Lstat to detect symlinks
		info, err := os.Lstat(path)
		if err != nil {
			return fn(path, nil, err)
		}

		// Check if it's a symlink and resolve it if so
		realPath := path
		if info.Mode()&os.ModeSymlink != 0 {
			realPath, err = filepath.EvalSymlinks(path)
			if err != nil {
				// If we can't resolve the symlink, call the function with the error
				return fn(path, fs.FileInfoToDirEntry(info), err)
			}

			// Check if we've already visited this real path to avoid infinite loops
			if visited[realPath] {
				return nil
			}

			// Get info about the target
			info, err = os.Stat(realPath)
			if err != nil {
				return fn(path, fs.FileInfoToDirEntry(info), err)
			}
		}

		visited[realPath] = true

		d := fs.FileInfoToDirEntry(info)
		err = fn(path, d, nil)
		if err != nil {
			if err == fs.SkipDir && info.IsDir() {
				return nil
			}
			return err
		}

		if info.IsDir() {
			entries, err := os.ReadDir(path)
			if err != nil {
				return fn(path, d, err)
			}

			for _, entry := range entries {
				childPath := filepath.Join(path, entry.Name())
				if err := walk(childPath); err != nil {
					return err
				}
			}
		}

		return nil
	}

	return walk(root)
}

// Refresh updates the hashes of all the files in the index, and adds new files to the index
func (in *Index) Refresh() error {
	return in.RefreshWithOptions(true)
}

func (in *Index) RefreshWithOptions(followSymlinks bool) error {
	// Is case-sensitivity a problem?
	pathPF, _ := filepath.Abs(viper.GetString("pack-file"))
	pathIndex, _ := filepath.Abs(in.indexFile)

	pathIgnore, _ := filepath.Abs(filepath.Join(in.packRoot, ".packwizignore"))
	ignore, ignoreExists := readGitignore(pathIgnore)

	var fileList []string
	var err error

	if followSymlinks {
		err = walkDirFollowSymlinks(in.packRoot, func(path string, info os.DirEntry, err error) error {
			if err != nil {
				// Log the error but continue processing other files
				fmt.Printf("Warning: Error accessing %s: %v\n", path, err)
				return nil
			}

			// Never ignore pack root itself (gitignore doesn't allow ignoring the root)
			if path == in.packRoot {
				return nil
			}

			if info.IsDir() {
				// Don't traverse ignored directories (consistent with Git handling of ignored dirs)
				if ignore.MatchesPath(path) {
					return fs.SkipDir
				}
				// Don't add directories to the file list
				return nil
			}
			// Exit if the files are the same as the pack/index files
			absPath, _ := filepath.Abs(path)
			if absPath == pathPF || absPath == pathIndex {
				return nil
			}
			if ignoreExists {
				if absPath == pathIgnore {
					return nil
				}
			}
			if ignore.MatchesPath(path) {
				return nil
			}

			fileList = append(fileList, path)
			return nil
		})
	} else {
		err = filepath.WalkDir(in.packRoot, func(path string, info os.DirEntry, err error) error {
			if err != nil {
				// Log the error but continue processing other files
				fmt.Printf("Warning: Error accessing %s: %v\n", path, err)
				return nil
			}

			// Never ignore pack root itself (gitignore doesn't allow ignoring the root)
			if path == in.packRoot {
				return nil
			}

			if info.IsDir() {
				// Don't traverse ignored directories (consistent with Git handling of ignored dirs)
				if ignore.MatchesPath(path) {
					return fs.SkipDir
				}
				// Don't add directories to the file list
				return nil
			}
			// Exit if the files are the same as the pack/index files
			absPath, _ := filepath.Abs(path)
			if absPath == pathPF || absPath == pathIndex {
				return nil
			}
			if ignoreExists {
				if absPath == pathIgnore {
					return nil
				}
			}
			if ignore.MatchesPath(path) {
				return nil
			}

			fileList = append(fileList, path)
			return nil
		})
	}
	if err != nil {
		return err
	}

	// Use multithreaded hashing for better performance
	return in.updateFilesMultithreaded(fileList)
}

// updateFilesMultithreaded processes files in parallel for better performance
func (in *Index) updateFilesMultithreaded(fileList []string) error {
	if len(fileList) == 0 {
		return nil
	}

	// Use number of CPUs for worker count, but cap it reasonably
	numWorkers := runtime.NumCPU()
	if numWorkers > 8 {
		numWorkers = 8
	}
	if numWorkers < 1 {
		numWorkers = 1
	}

	// Create progress bar
	progressContainer := mpb.New()
	progress := progressContainer.AddBar(int64(len(fileList)),
		mpb.PrependDecorators(
			decor.Name("Refreshing index..."),
			decor.Percentage(decor.WCSyncSpace),
		),
		mpb.AppendDecorators(
			decor.OnComplete(
				decor.EwmaETA(decor.ET_STYLE_GO, 60), "done",
			),
		),
	)

	// Channel for distributing work
	fileChan := make(chan string, len(fileList))

	// Channel for collecting results
	type result struct {
		path       string
		err        error
		relPath    string
		format     string
		hash       string
		isMetaFile bool
	}
	resultChan := make(chan result, len(fileList))

	// Start workers
	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for filePath := range fileChan {
				start := time.Now()

				// Calculate hash and metadata for this file
				var hashString string
				var markAsMetaFile bool
				var err error

				if viper.GetBool("no-internal-hashes") {
					hashString = ""
				} else {
					f, openErr := os.Open(filePath)
					if openErr != nil {
						resultChan <- result{path: filePath, err: openErr}
						progress.Increment(time.Since(start))
						continue
					}

					h, hashErr := GetHashImpl("sha256")
					if hashErr != nil {
						_ = f.Close()
						resultChan <- result{path: filePath, err: hashErr}
						progress.Increment(time.Since(start))
						continue
					}

					if _, copyErr := io.Copy(h, f); copyErr != nil {
						_ = f.Close()
						resultChan <- result{path: filePath, err: copyErr}
						progress.Increment(time.Since(start))
						continue
					}

					err = f.Close()
					if err != nil {
						resultChan <- result{path: filePath, err: err}
						progress.Increment(time.Since(start))
						continue
					}
					hashString = h.HashToString(h.Sum(nil))
				}

				// Check if it's a meta file
				if strings.HasSuffix(filepath.Base(filePath), MetaExtension) {
					markAsMetaFile = true
				}

				// Get relative path
				relPath, err := in.RelIndexPath(filePath)
				if err != nil {
					resultChan <- result{path: filePath, err: err}
					progress.Increment(time.Since(start))
					continue
				}

				// Determine format
				format := "sha256"
				if in.HashFormat == format {
					format = ""
				}

				resultChan <- result{
					path:       filePath,
					relPath:    relPath,
					format:     format,
					hash:       hashString,
					isMetaFile: markAsMetaFile,
				}
				progress.Increment(time.Since(start))
			}
		}()
	}

	// Send all files to workers
	for _, filePath := range fileList {
		fileChan <- filePath
	}
	close(fileChan)

	// Wait for all workers to complete
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// Collect results and update index (sequentially to avoid race conditions)
	var errors []error
	for result := range resultChan {
		if result.err != nil {
			errors = append(errors, fmt.Errorf("error processing %s: %w", result.path, result.err))
		} else {
			// Update the index files map (this is now safe since it's sequential)
			in.Files.updateFileEntry(result.relPath, result.format, result.hash, result.isMetaFile)
		}
	}

	// Close progress bar
	progress.SetTotal(int64(len(fileList)), true)
	progressContainer.Wait()

	// Check all the files exist, remove them if they don't
	for p, file := range in.Files {
		if !file.markedFound() {
			delete(in.Files, p)
		}
	}

	// Return first error if any occurred
	if len(errors) > 0 {
		return errors[0]
	}

	return nil
}

// Write saves the index file
func (in Index) Write() error {
	// Convert to indexTomlRepresentation
	rep := indexTomlRepresentation{
		HashFormat: in.HashFormat,
		Files:      in.Files.toTomlRep(),
	}

	// Calculate hash while writing
	f, err := os.Create(in.indexFile)
	if err != nil {
		return err
	}

	// Create a hash writer to calculate the file hash while writing
	hasher, err := GetHashImpl("sha256")
	if err != nil {
		_ = f.Close()
		return err
	}

	// Use MultiWriter to write to both file and hasher simultaneously
	writer := io.MultiWriter(f, hasher)

	enc := toml.NewEncoder(writer)
	// Disable indentation
	enc.Indent = ""
	err = enc.Encode(rep)
	if err != nil {
		_ = f.Close()
		return err
	}

	err = f.Close()
	if err != nil {
		return err
	}

	// Store the calculated hash for potential future use
	// (This could be used for pack validation or change detection)
	indexHash := hasher.HashToString(hasher.Sum(nil))
	_ = indexHash // Currently unused, but calculated for future enhancement

	return nil
}

// RefreshFileWithHash updates a file in the index, given a file hash and whether it should be marked as metafile or not
func (in *Index) RefreshFileWithHash(path, format, hash string, markAsMetaFile bool) error {
	if viper.GetBool("no-internal-hashes") {
		hash = ""
	}
	return in.updateFileHashGiven(path, format, hash, markAsMetaFile)
}

// FindMod finds a mod in the index and returns its path and whether it has been found
func (in Index) FindMod(modName string) (string, bool) {
	for p, v := range in.Files {
		if v.IsMetaFile() {
			_, fileName := path.Split(p)
			fileTrimmed := strings.TrimSuffix(strings.TrimSuffix(fileName, MetaExtension), MetaExtensionOld)
			if fileTrimmed == modName {
				return in.ResolveIndexPath(p), true
			}
		}
	}
	return "", false
}

// getAllMods finds paths to every metadata file (Mod) in the index
func (in Index) getAllMods() []string {
	var list []string
	for p, v := range in.Files {
		if v.IsMetaFile() {
			list = append(list, in.ResolveIndexPath(p))
		}
	}
	return list
}

// LoadAllMods reads all metadata files into Mod structs
func (in Index) LoadAllMods() ([]*Mod, error) {
	modPaths := in.getAllMods()
	mods := make([]*Mod, len(modPaths))
	for i, v := range modPaths {
		modData, err := LoadMod(v)
		if err != nil {
			return nil, fmt.Errorf("failed to read metadata file %s: %w", v, err)
		}
		mods[i] = &modData
	}
	return mods, nil
}
