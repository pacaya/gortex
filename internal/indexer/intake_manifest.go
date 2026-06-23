package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

// IntakeManifest is a privacy-safe, pre-extraction view of the corpus
// admitted by the index walk. It intentionally reports aggregate buckets and
// path hashes instead of raw paths or file contents so users can share it in
// issue reports for private repositories.
type IntakeManifest struct {
	SchemaVersion                string         `json:"schema_version"`
	RepoRef                      string         `json:"repo_ref,omitempty"`
	FilesSeen                    int            `json:"files_seen"`
	BytesSeen                    int64          `json:"bytes_seen"`
	FilesAdmitted                int            `json:"files_admitted"`
	BytesAdmitted                int64          `json:"bytes_admitted"`
	FilesSkipped                 int            `json:"files_skipped"`
	BytesSkipped                 int64          `json:"bytes_skipped"`
	TopAdmittedExtensionsByBytes []IntakeBucket `json:"top_admitted_extensions_by_bytes"`
	TopSkippedExtensionsByBytes  []IntakeBucket `json:"top_skipped_extensions_by_bytes"`
	TopAdmittedDirsByBytes       []DirBucket    `json:"top_admitted_dirs_by_bytes"`
	LargestAdmittedFiles         []FileBucket   `json:"largest_admitted_files"`
	RawPathsIncluded             bool           `json:"raw_paths_included"`
	RawFileContentsIncluded      bool           `json:"raw_file_contents_included"`
}

type IntakeBucket struct {
	Key    string `json:"key"`
	Files  int    `json:"files"`
	Bytes  int64  `json:"bytes"`
	Reason string `json:"reason,omitempty"`
}

type DirBucket struct {
	PathHash string `json:"path_hash"`
	Depth    int    `json:"depth"`
	Files    int    `json:"files"`
	Bytes    int64  `json:"bytes"`
}

type FileBucket struct {
	PathHash string `json:"path_hash"`
	Ext      string `json:"ext"`
	Bytes    int64  `json:"bytes"`
}

// DryRunIntake walks root with the same high-level language, ignore, prune and
// max-size gates used before IndexCtx starts extraction. It does not parse,
// extract, embed, or store anything.
func (idx *Indexer) DryRunIntake(_ context.Context, root string) (*IntakeManifest, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	maxSize := idx.config.MaxFileSize
	manifest := &IntakeManifest{
		SchemaVersion:           "gortex.index_intake.v1",
		RawPathsIncluded:        false,
		RawFileContentsIncluded: false,
	}
	admittedExt := map[string]*IntakeBucket{}
	skippedExt := map[string]*IntakeBucket{}
	admittedDirs := map[string]*DirBucket{}
	largest := make([]FileBucket, 0, 16)

	err = filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if idx.shouldPruneDir(path, absRoot) {
				return filepath.SkipDir
			}
			return nil
		}

		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		size := info.Size()
		manifest.FilesSeen++
		manifest.BytesSeen += size

		reason := ""
		lang, ok := idx.effectiveLanguage(path, nil)
		if !ok {
			reason = "no_language"
		} else if idx.shouldExclude(path, absRoot, false) {
			reason = "excluded"
		} else if maxSize > 0 && size > maxSize {
			reason = "max_file_size"
		}

		if reason != "" {
			manifest.FilesSkipped++
			manifest.BytesSkipped += size
			b := ensureIntakeBucket(skippedExt, extKey(path))
			b.Files++
			b.Bytes += size
			if b.Reason == "" {
				b.Reason = reason
			}
			return nil
		}

		_ = lang // language detection is the admission gate; buckets stay extension-based for issue reports.
		manifest.FilesAdmitted++
		manifest.BytesAdmitted += size

		ext := extKey(path)
		extBucket := ensureIntakeBucket(admittedExt, ext)
		extBucket.Files++
		extBucket.Bytes += size

		key, depth := hashedDirBucket(absRoot, path)
		dirBucket := admittedDirs[key]
		if dirBucket == nil {
			dirBucket = &DirBucket{PathHash: key, Depth: depth}
			admittedDirs[key] = dirBucket
		}
		dirBucket.Files++
		dirBucket.Bytes += size

		rel, _ := filepath.Rel(absRoot, path)
		largest = append(largest, FileBucket{PathHash: hashPath(rel), Ext: ext, Bytes: size})
		return nil
	})
	if err != nil {
		return nil, err
	}

	manifest.TopAdmittedExtensionsByBytes = topIntakeBuckets(admittedExt, 20)
	manifest.TopSkippedExtensionsByBytes = topIntakeBuckets(skippedExt, 20)
	manifest.TopAdmittedDirsByBytes = topDirBuckets(admittedDirs, 20)
	manifest.LargestAdmittedFiles = topFileBuckets(largest, 20)
	return manifest, nil
}

func ensureIntakeBucket(m map[string]*IntakeBucket, key string) *IntakeBucket {
	b := m[key]
	if b == nil {
		b = &IntakeBucket{Key: key}
		m[key] = b
	}
	return b
}

func extKey(path string) string {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	if ext == "" {
		return "[none]"
	}
	return ext
}

func hashedDirBucket(root, path string) (string, int) {
	rel, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil || rel == "." || rel == "" {
		return hashPath("."), 0
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	depth := 1
	return hashPath(parts[0]), depth
}

func hashPath(rel string) string {
	sum := sha256.Sum256([]byte(filepath.ToSlash(rel)))
	return hex.EncodeToString(sum[:8])
}

func topIntakeBuckets(m map[string]*IntakeBucket, limit int) []IntakeBucket {
	out := make([]IntakeBucket, 0, len(m))
	for _, b := range m {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bytes == out[j].Bytes {
			return out[i].Key < out[j].Key
		}
		return out[i].Bytes > out[j].Bytes
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func topDirBuckets(m map[string]*DirBucket, limit int) []DirBucket {
	out := make([]DirBucket, 0, len(m))
	for _, b := range m {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bytes == out[j].Bytes {
			return out[i].PathHash < out[j].PathHash
		}
		return out[i].Bytes > out[j].Bytes
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func topFileBuckets(files []FileBucket, limit int) []FileBucket {
	sort.Slice(files, func(i, j int) bool {
		if files[i].Bytes == files[j].Bytes {
			return files[i].PathHash < files[j].PathHash
		}
		return files[i].Bytes > files[j].Bytes
	})
	if len(files) > limit {
		files = files[:limit]
	}
	return files
}
