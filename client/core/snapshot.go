package core

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	incomingSuffix    = ".peerly-incoming"
	outgoingSuffix    = ".peerly-outgoing"
	keptBackups       = 5
	maxExtractedBytes = 32 << 30
	maxSaveFiles      = 20000
	maxSaveBytes      = 32 << 30
	surveyWalkLimit   = 50000
	surveySampleSize  = 25
)

type IncludeFilter []string

func ParseInclude(raw string) IncludeFilter {
	filter := IncludeFilter{}
	for _, pattern := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(pattern); trimmed != "" {
			filter = append(filter, strings.ToLower(filepath.ToSlash(trimmed)))
		}
	}
	return filter
}

func matchesPattern(pattern string, relativePath string) bool {
	firstSegment, _, _ := strings.Cut(relativePath, "/")
	if matched, _ := path.Match(pattern, firstSegment); matched {
		return true
	}
	if matched, _ := path.Match(pattern, path.Base(relativePath)); matched {
		return true
	}
	matched, _ := path.Match(pattern, relativePath)
	return matched
}

func (f IncludeFilter) Matches(relativePath string) bool {
	lowered := strings.ToLower(relativePath)
	includeEverything := true
	for _, pattern := range f {
		if excluded, isExclusion := strings.CutPrefix(pattern, "!"); isExclusion {
			if matchesPattern(excluded, lowered) {
				return false
			}
			continue
		}
		includeEverything = false
	}
	if includeEverything {
		return true
	}
	for _, pattern := range f {
		if !strings.HasPrefix(pattern, "!") && matchesPattern(pattern, lowered) {
			return true
		}
	}
	return false
}

func (f IncludeFilter) Validate() error {
	for _, pattern := range f {
		if _, err := path.Match(strings.TrimPrefix(pattern, "!"), "probe"); err != nil {
			return fmt.Errorf("the file filter %q is not valid, square brackets must be closed", pattern)
		}
	}
	return nil
}

type TooLargeError struct {
	Folder string
	Files  int
	Bytes  int64
}

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("%d files (%.1f GB) match in %s. That does not look like one game world, narrow the file filter in Settings",
		e.Files, float64(e.Bytes)/(1<<30), e.Folder)
}

type InUseError struct {
	File string
	Err  error
}

func (e *InUseError) Error() string {
	return fmt.Sprintf("%s could not be replaced because another program is using it (%v). Close the game, pause cloud sync for that folder and try again. Nothing was changed", e.File, e.Err)
}

func (e *InUseError) Unwrap() error {
	return e.Err
}

type fileEntry struct {
	relativePath string
	absolutePath string
	info         fs.FileInfo
}

func resolveRoot(root string) string {
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		return resolved
	}
	return root
}

func listFiles(root string, filter IncludeFilter) ([]fileEntry, error) {
	root = resolveRoot(root)
	entries := []fileEntry{}
	var totalBytes int64
	err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if current == root && errors.Is(walkErr, os.ErrNotExist) {
				return filepath.SkipAll
			}
			return walkErr
		}
		if entry.IsDir() {
			if strings.HasSuffix(entry.Name(), incomingSuffix) || strings.HasSuffix(entry.Name(), outgoingSuffix) {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if !filter.Matches(relative) {
			return nil
		}
		info, err := os.Stat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		totalBytes += info.Size()
		entries = append(entries, fileEntry{relativePath: relative, absolutePath: current, info: info})
		if len(entries) > maxSaveFiles || totalBytes > maxSaveBytes {
			return &TooLargeError{Folder: root, Files: len(entries), Bytes: totalBytes}
		}
		return nil
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].relativePath < entries[j].relativePath })
	return entries, err
}

type Survey struct {
	Folder       string   `json:"folder"`
	FolderExists bool     `json:"folder_exists"`
	ParentExists bool     `json:"parent_exists"`
	Problem      string   `json:"problem"`
	MatchedCount int      `json:"matched_count"`
	MatchedBytes int64    `json:"matched_bytes"`
	Matched      []string `json:"matched"`
	OtherCount   int      `json:"other_count"`
	Others       []string `json:"others"`
}

func SurveyFolder(folder string, filter IncludeFilter) Survey {
	survey := Survey{Folder: folder, Matched: []string{}, Others: []string{}}
	if err := CheckSaveFolder(folder); err != nil {
		survey.Problem = err.Error()
		return survey
	}
	if err := filter.Validate(); err != nil {
		survey.Problem = err.Error()
		return survey
	}
	if info, err := os.Stat(folder); err == nil && info.IsDir() {
		survey.FolderExists = true
	}
	if info, err := os.Stat(filepath.Dir(folder)); err == nil && info.IsDir() {
		survey.ParentExists = true
	}
	if !survey.FolderExists {
		return survey
	}
	root := resolveRoot(folder)
	visited := 0
	err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		visited++
		if visited > surveyWalkLimit {
			return &TooLargeError{Folder: folder, Files: visited}
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return nil
		}
		relative = filepath.ToSlash(relative)
		if !filter.Matches(relative) {
			survey.OtherCount++
			if len(survey.Others) < surveySampleSize {
				survey.Others = append(survey.Others, relative)
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		survey.MatchedCount++
		survey.MatchedBytes += info.Size()
		if len(survey.Matched) < surveySampleSize {
			survey.Matched = append(survey.Matched, relative)
		}
		return nil
	})
	var tooLarge *TooLargeError
	if errors.As(err, &tooLarge) || survey.MatchedCount > maxSaveFiles || survey.MatchedBytes > maxSaveBytes {
		survey.Problem = fmt.Sprintf("%s holds too many files to be one game world. Pick a more specific folder or narrow the file filter", folder)
	}
	return survey
}

func Fingerprint(root string, filter IncludeFilter) (string, int, error) {
	entries, err := listFiles(root, filter)
	if err != nil {
		return "", 0, err
	}
	hasher := sha256.New()
	for _, entry := range entries {
		fmt.Fprintf(hasher, "%s\x00%d\x00%d\n", entry.relativePath, entry.info.Size(), entry.info.ModTime().UnixNano())
	}
	return hex.EncodeToString(hasher.Sum(nil)), len(entries), nil
}

func ManifestHash(root string, filter IncludeFilter) (string, int, error) {
	entries, err := listFiles(root, filter)
	if err != nil {
		return "", 0, err
	}
	manifest := sha256.New()
	for _, entry := range entries {
		file, err := openForRead(entry.absolutePath)
		if err != nil {
			return "", 0, err
		}
		content := sha256.New()
		size, err := io.Copy(content, file)
		file.Close()
		if err != nil {
			return "", 0, err
		}
		fmt.Fprintf(manifest, "%s\x00%d\x00%x\n", entry.relativePath, size, content.Sum(nil))
	}
	return hex.EncodeToString(manifest.Sum(nil)), len(entries), nil
}

type PackResult struct {
	ArchiveSha256 string
	ManifestHash  string
	FileCount     int
	ArchiveBytes  int64
}

func Pack(root string, filter IncludeFilter, archivePath string) (PackResult, error) {
	entries, err := listFiles(root, filter)
	if err != nil {
		return PackResult{}, err
	}
	output, err := os.Create(archivePath)
	if err != nil {
		return PackResult{}, err
	}
	defer output.Close()
	archiveHasher := sha256.New()
	compressor, err := zstd.NewWriter(io.MultiWriter(output, archiveHasher))
	if err != nil {
		return PackResult{}, err
	}
	compressorClosed := false
	defer func() {
		if !compressorClosed {
			compressor.Close()
		}
	}()
	tarWriter := tar.NewWriter(compressor)
	manifest := sha256.New()
	for _, entry := range entries {
		file, err := openForRead(entry.absolutePath)
		if err != nil {
			return PackResult{}, err
		}
		opened, err := file.Stat()
		if err != nil {
			file.Close()
			return PackResult{}, err
		}
		header := &tar.Header{
			Name:    entry.relativePath,
			Mode:    0o644,
			Size:    opened.Size(),
			ModTime: opened.ModTime(),
			Format:  tar.FormatPAX,
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			file.Close()
			return PackResult{}, err
		}
		content := sha256.New()
		written, err := io.Copy(tarWriter, io.TeeReader(io.LimitReader(file, opened.Size()), content))
		after, statErr := file.Stat()
		file.Close()
		if err := errors.Join(err, statErr); err != nil {
			return PackResult{}, err
		}
		if written != opened.Size() || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
			return PackResult{}, fmt.Errorf("%s changed while it was being read", entry.relativePath)
		}
		fmt.Fprintf(manifest, "%s\x00%d\x00%x\n", entry.relativePath, written, content.Sum(nil))
	}
	if err := tarWriter.Close(); err != nil {
		return PackResult{}, err
	}
	compressorClosed = true
	if err := compressor.Close(); err != nil {
		return PackResult{}, err
	}
	if err := output.Sync(); err != nil {
		return PackResult{}, err
	}
	info, err := output.Stat()
	if err != nil {
		return PackResult{}, err
	}
	return PackResult{
		ArchiveSha256: hex.EncodeToString(archiveHasher.Sum(nil)),
		ManifestHash:  hex.EncodeToString(manifest.Sum(nil)),
		FileCount:     len(entries),
		ArchiveBytes:  info.Size(),
	}, nil
}

func extract(archivePath string, destination string) ([]string, error) {
	archive, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer archive.Close()
	decompressor, err := zstd.NewReader(archive)
	if err != nil {
		return nil, err
	}
	defer decompressor.Close()
	tarReader := tar.NewReader(decompressor)
	extracted := []string{}
	seen := map[string]bool{}
	var totalBytes int64
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			return extracted, nil
		}
		if err != nil {
			return nil, fmt.Errorf("the downloaded save is damaged: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		totalBytes += header.Size
		if totalBytes > maxExtractedBytes || len(extracted) >= maxSaveFiles {
			return nil, errors.New("the downloaded save expands beyond the allowed size")
		}
		localName := filepath.FromSlash(header.Name)
		if !filepath.IsLocal(localName) || strings.ContainsAny(header.Name, `:\`) {
			return nil, fmt.Errorf("the downloaded save contains an unsafe path: %s", header.Name)
		}
		collisionKey := strings.ToLower(header.Name)
		if seen[collisionKey] {
			return nil, fmt.Errorf("the downloaded save contains %s twice", header.Name)
		}
		seen[collisionKey] = true
		target := filepath.Join(destination, localName)
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return nil, err
		}
		file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
		if err != nil {
			return nil, err
		}
		_, copyErr := io.Copy(file, io.LimitReader(tarReader, header.Size))
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return nil, err
		}
		if err := os.Chtimes(target, header.ModTime, header.ModTime); err != nil {
			return nil, err
		}
		extracted = append(extracted, localName)
	}
}

func copyFile(source string, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return err
	}
	input, err := openForRead(source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	output, err := os.Create(destination)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return err
	}
	return os.Chtimes(destination, info.ModTime(), info.ModTime())
}

func backupExisting(entries []fileEntry, backupRoot string, now time.Time) (string, error) {
	if len(entries) == 0 {
		return "", nil
	}
	backupDir := filepath.Join(backupRoot, now.Format("20060102-150405.000"))
	for _, entry := range entries {
		if err := copyFile(entry.absolutePath, filepath.Join(backupDir, filepath.FromSlash(entry.relativePath))); err != nil {
			os.RemoveAll(backupDir)
			return "", err
		}
	}
	return backupDir, pruneBackups(backupRoot)
}

func pruneBackups(backupRoot string) error {
	children, err := os.ReadDir(backupRoot)
	if err != nil {
		return err
	}
	names := []string{}
	for _, child := range children {
		if child.IsDir() {
			names = append(names, child.Name())
		}
	}
	sort.Strings(names)
	for len(names) > keptBackups {
		if err := os.RemoveAll(filepath.Join(backupRoot, names[1])); err != nil {
			return err
		}
		names = append(names[:1], names[2:]...)
	}
	return nil
}

func rescueInterruptedRestore(outgoing string, backupRoot string) error {
	if _, err := os.Stat(outgoing); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := os.MkdirAll(backupRoot, 0o750); err != nil {
		return err
	}
	rescued := filepath.Join(backupRoot, time.Now().Format("20060102-150405.000")+"-recovered")
	if err := os.Rename(outgoing, rescued); err != nil {
		return fmt.Errorf("files from an interrupted restore are waiting in %s, move them somewhere safe and try again: %w", outgoing, err)
	}
	return nil
}

type RestoreResult struct {
	BackupDir string
	Skipped   []string
}

func Restore(archivePath string, savePath string, filter IncludeFilter, backupRoot string, backup bool) (RestoreResult, error) {
	result := RestoreResult{}
	backupDir, skipped, err := restore(archivePath, savePath, filter, backupRoot, backup)
	result.BackupDir, result.Skipped = backupDir, skipped
	return result, err
}

func restore(archivePath string, savePath string, filter IncludeFilter, backupRoot string, backup bool) (string, []string, error) {
	savePath = resolveRoot(savePath)
	incoming := savePath + incomingSuffix
	outgoing := savePath + outgoingSuffix
	if err := rescueInterruptedRestore(outgoing, backupRoot); err != nil {
		return "", nil, err
	}
	if err := os.RemoveAll(incoming); err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(incoming)
	unpacked, err := extract(archivePath, incoming)
	if err != nil {
		return "", nil, err
	}
	extracted := []string{}
	skipped := []string{}
	for _, localName := range unpacked {
		if filter.Matches(filepath.ToSlash(localName)) {
			extracted = append(extracted, localName)
		} else {
			skipped = append(skipped, filepath.ToSlash(localName))
		}
	}
	existing, err := listFiles(savePath, filter)
	if err != nil {
		return "", skipped, err
	}
	backupDir := ""
	if backup {
		backupDir, err = backupExisting(existing, backupRoot, time.Now())
		if err != nil {
			return "", skipped, fmt.Errorf("the local backup failed so nothing was changed: %w", err)
		}
	}

	movedOut := []fileEntry{}
	putBack := func() {
		for index := len(movedOut) - 1; index >= 0; index-- {
			entry := movedOut[index]
			os.MkdirAll(filepath.Dir(entry.absolutePath), 0o750)
			os.Rename(filepath.Join(outgoing, filepath.FromSlash(entry.relativePath)), entry.absolutePath)
		}
		os.RemoveAll(outgoing)
	}
	for _, entry := range existing {
		aside := filepath.Join(outgoing, filepath.FromSlash(entry.relativePath))
		if err := os.MkdirAll(filepath.Dir(aside), 0o750); err != nil {
			putBack()
			return backupDir, skipped, err
		}
		if err := os.Rename(entry.absolutePath, aside); err != nil {
			putBack()
			return backupDir, skipped, &InUseError{File: entry.absolutePath, Err: err}
		}
		movedOut = append(movedOut, entry)
	}
	placed := []string{}
	for _, localName := range extracted {
		target := filepath.Join(savePath, localName)
		err := os.MkdirAll(filepath.Dir(target), 0o750)
		if err == nil {
			err = os.Rename(filepath.Join(incoming, localName), target)
		}
		if err != nil {
			for _, placedTarget := range placed {
				os.Remove(placedTarget)
			}
			putBack()
			return backupDir, skipped, &InUseError{File: target, Err: err}
		}
		placed = append(placed, target)
	}
	os.RemoveAll(outgoing)
	return backupDir, skipped, nil
}
