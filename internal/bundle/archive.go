package bundle

import (
	"archive/zip"
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"reflect"
	"unicode/utf8"
)

const maxSpecialJudgeSourceBytes int64 = 4 << 20

type ArchiveLimits struct {
	MaxFiles            int
	MaxManifestBytes    int64
	MaxCaseBytes        int64
	MaxTotalBytes       int64
	MaxCompressionRatio uint64
}

func DefaultArchiveLimits() ArchiveLimits {
	return ArchiveLimits{
		MaxFiles:            20_002,
		MaxManifestBytes:    1 << 20,
		MaxCaseBytes:        64 << 20,
		MaxTotalBytes:       512 << 20,
		MaxCompressionRatio: 200,
	}
}

type Artifact struct {
	archive      *zip.ReadCloser
	manifest     Manifest
	manifestJSON []byte
	files        map[string]*zip.File
	limits       ArchiveLimits
}

func OpenArchive(filename string, databaseManifest []byte, limits ArchiveLimits) (*Artifact, error) {
	if err := ValidateArchiveLimits(limits); err != nil {
		return nil, err
	}
	if int64(len(databaseManifest)) > limits.MaxManifestBytes {
		return nil, fmt.Errorf("database bundle manifest exceeds size limit")
	}
	expected, err := ParseManifest(databaseManifest)
	if err != nil {
		return nil, fmt.Errorf("database bundle manifest: %w", err)
	}
	return openArchive(filename, limits, &expected)
}

// InspectArchive validates an untrusted self-describing archive and returns
// only its canonical manifest. It shares the exact ZIP hardening path used by
// immutable bundles loaded from the database.
func InspectArchive(filename string, limits ArchiveLimits) (Manifest, []byte, error) {
	if err := ValidateArchiveLimits(limits); err != nil {
		return Manifest{}, nil, err
	}
	artifact, err := openArchive(filename, limits, nil)
	if err != nil {
		return Manifest{}, nil, err
	}
	defer artifact.Close()
	for _, testCase := range artifact.manifest.Cases {
		if err := artifact.validateTextEntry(testCase.Input, limits.MaxCaseBytes); err != nil {
			return Manifest{}, nil, fmt.Errorf("validate case %q input: %w", testCase.ID, err)
		}
		if err := artifact.validateTextEntry(testCase.Output, limits.MaxCaseBytes); err != nil {
			return Manifest{}, nil, fmt.Errorf("validate case %q output: %w", testCase.ID, err)
		}
	}
	if artifact.manifest.SpecialJudge != nil {
		if err := artifact.validateTextEntry(artifact.manifest.SpecialJudge.Source, maxSpecialJudgeSourceBytes); err != nil {
			return Manifest{}, nil, fmt.Errorf("validate special judge source: %w", err)
		}
	}
	return artifact.manifest, append([]byte(nil), artifact.manifestJSON...), nil
}

func openArchive(filename string, limits ArchiveLimits, expected *Manifest) (*Artifact, error) {
	reader, err := zip.OpenReader(filename)
	if err != nil {
		return nil, fmt.Errorf("open bundle ZIP: %w", err)
	}
	artifact := &Artifact{archive: reader, files: make(map[string]*zip.File), limits: limits}
	if err := artifact.validate(expected); err != nil {
		_ = reader.Close()
		return nil, err
	}
	return artifact, nil
}

func (artifact *Artifact) Manifest() Manifest { return artifact.manifest }

func (artifact *Artifact) Close() error { return artifact.archive.Close() }

func (artifact *Artifact) ReadCase(testCase Case) (string, string, error) {
	input, err := artifact.readText(testCase.Input, artifact.limits.MaxCaseBytes)
	if err != nil {
		return "", "", fmt.Errorf("read case %q input: %w", testCase.ID, err)
	}
	output, err := artifact.readText(testCase.Output, artifact.limits.MaxCaseBytes)
	if err != nil {
		return "", "", fmt.Errorf("read case %q output: %w", testCase.ID, err)
	}
	return input, output, nil
}

func (artifact *Artifact) ReadSpecialJudge() (string, error) {
	if artifact.manifest.SpecialJudge == nil {
		return "", fmt.Errorf("bundle does not contain a special judge")
	}
	source, err := artifact.readText(artifact.manifest.SpecialJudge.Source, maxSpecialJudgeSourceBytes)
	if err != nil {
		return "", fmt.Errorf("read special judge source: %w", err)
	}
	digest := sha256.Sum256([]byte(source))
	if hex.EncodeToString(digest[:]) != artifact.manifest.SpecialJudge.SourceSHA256 {
		return "", fmt.Errorf("special judge source digest mismatch")
	}
	return source, nil
}

func (artifact *Artifact) validate(expected *Manifest) error {
	if len(artifact.archive.File) == 0 || len(artifact.archive.File) > artifact.limits.MaxFiles {
		return fmt.Errorf("bundle file count exceeds limit")
	}
	var total uint64
	for _, file := range artifact.archive.File {
		if err := validateArtifactPath(file.Name); err != nil {
			return fmt.Errorf("unsafe ZIP entry: %w", err)
		}
		if _, exists := artifact.files[file.Name]; exists {
			return fmt.Errorf("duplicate ZIP entry %q", file.Name)
		}
		if !file.Mode().IsRegular() || file.Flags&1 != 0 || (file.Method != zip.Store && file.Method != zip.Deflate) {
			return fmt.Errorf("ZIP entry %q must be an unencrypted regular file", file.Name)
		}
		if file.UncompressedSize64 > uint64(artifact.limits.MaxCaseBytes) && file.Name != "manifest.json" {
			return fmt.Errorf("ZIP entry %q exceeds per-case size limit", file.Name)
		}
		if file.Name == "manifest.json" && file.UncompressedSize64 > uint64(artifact.limits.MaxManifestBytes) {
			return fmt.Errorf("manifest.json exceeds size limit")
		}
		if exceedsCompressionRatio(file.UncompressedSize64, file.CompressedSize64, artifact.limits.MaxCompressionRatio) {
			return fmt.Errorf("ZIP entry %q exceeds compression ratio limit", file.Name)
		}
		maximumTotal := uint64(artifact.limits.MaxTotalBytes)
		if file.UncompressedSize64 > maximumTotal || total > maximumTotal-file.UncompressedSize64 {
			return fmt.Errorf("bundle exceeds total uncompressed size limit")
		}
		total += file.UncompressedSize64
		artifact.files[file.Name] = file
	}
	manifestBytes, err := artifact.readBytes("manifest.json", artifact.limits.MaxManifestBytes)
	if err != nil {
		return fmt.Errorf("read manifest.json: %w", err)
	}
	actual, err := ParseManifest(manifestBytes)
	if err != nil {
		return fmt.Errorf("artifact manifest.json: %w", err)
	}
	if expected != nil && !reflect.DeepEqual(*expected, actual) {
		return fmt.Errorf("artifact manifest.json disagrees with database manifest_json")
	}
	referenced := map[string]struct{}{"manifest.json": {}}
	if actual.SpecialJudge != nil {
		referenced[actual.SpecialJudge.Source] = struct{}{}
	}
	for _, testCase := range actual.Cases {
		referenced[testCase.Input] = struct{}{}
		referenced[testCase.Output] = struct{}{}
	}
	if len(referenced) != len(artifact.files) {
		return fmt.Errorf("bundle contains unreferenced files")
	}
	for name := range referenced {
		if artifact.files[name] == nil {
			return fmt.Errorf("bundle is missing referenced file %q", name)
		}
	}
	artifact.manifest = actual
	artifact.manifestJSON = append([]byte(nil), manifestBytes...)
	if actual.SpecialJudge != nil {
		if _, err := artifact.ReadSpecialJudge(); err != nil {
			return err
		}
	}
	return nil
}

func exceedsCompressionRatio(uncompressed, compressed, maximum uint64) bool {
	if uncompressed == 0 {
		return false
	}
	if compressed == 0 {
		return true
	}
	quotient, remainder := uncompressed/compressed, uncompressed%compressed
	return quotient > maximum || (quotient == maximum && remainder > 0)
}

func (artifact *Artifact) readText(name string, limit int64) (string, error) {
	data, err := artifact.readBytes(name, limit)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("entry is not valid UTF-8")
	}
	return string(data), nil
}

func (artifact *Artifact) validateTextEntry(name string, limit int64) error {
	file := artifact.files[name]
	if file == nil {
		return fmt.Errorf("entry %q does not exist", name)
	}
	reader, err := file.Open()
	if err != nil {
		return fmt.Errorf("open ZIP entry: %w", err)
	}
	defer reader.Close()
	buffered := bufio.NewReader(io.LimitReader(reader, limit+1))
	var size int64
	for {
		value, width, err := buffered.ReadRune()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read ZIP entry: %w", err)
		}
		if value == utf8.RuneError && width == 1 {
			return fmt.Errorf("entry is not valid UTF-8")
		}
		size += int64(width)
		if size > limit {
			return fmt.Errorf("ZIP entry size mismatch or limit exceeded")
		}
	}
	if uint64(size) != file.UncompressedSize64 {
		return fmt.Errorf("ZIP entry size mismatch or limit exceeded")
	}
	return nil
}

func (artifact *Artifact) readBytes(name string, limit int64) ([]byte, error) {
	file := artifact.files[name]
	if file == nil {
		return nil, fmt.Errorf("entry %q does not exist", name)
	}
	reader, err := file.Open()
	if err != nil {
		return nil, fmt.Errorf("open ZIP entry: %w", err)
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read ZIP entry: %w", err)
	}
	if int64(len(data)) > limit || uint64(len(data)) != file.UncompressedSize64 {
		return nil, fmt.Errorf("ZIP entry size mismatch or limit exceeded")
	}
	return data, nil
}

func ValidateArchiveLimits(limits ArchiveLimits) error {
	if limits.MaxFiles <= 0 || limits.MaxManifestBytes <= 0 || limits.MaxCaseBytes <= 0 || limits.MaxTotalBytes <= 0 || limits.MaxCompressionRatio == 0 {
		return fmt.Errorf("archive limits must be positive")
	}
	return nil
}
