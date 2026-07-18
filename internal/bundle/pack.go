package bundle

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"
)

var deterministicZIPTime = time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)

// WriteDeterministicArchive writes manifest.json followed by sorted case files
// with fixed metadata. It returns the canonical manifest bytes that must also
// be stored in t_test_bundle.manifest_json.
func WriteDeterministicArchive(destination io.Writer, manifest Manifest, files map[string][]byte) ([]byte, error) {
	if destination == nil {
		return nil, fmt.Errorf("archive destination is required")
	}
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	canonicalManifest, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode canonical manifest: %w", err)
	}
	referenced := make(map[string]struct{}, len(manifest.Cases)*2)
	for _, testCase := range manifest.Cases {
		referenced[testCase.Input] = struct{}{}
		referenced[testCase.Output] = struct{}{}
	}
	if len(files) != len(referenced) {
		return nil, fmt.Errorf("archive files do not exactly match manifest cases")
	}
	names := make([]string, 0, len(referenced))
	for name := range referenced {
		if _, exists := files[name]; !exists {
			return nil, fmt.Errorf("archive is missing manifest file %q", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)

	writer := zip.NewWriter(destination)
	if err := writeDeterministicZIPEntry(writer, "manifest.json", canonicalManifest); err != nil {
		_ = writer.Close()
		return nil, err
	}
	for _, name := range names {
		if err := writeDeterministicZIPEntry(writer, name, files[name]); err != nil {
			_ = writer.Close()
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("finish deterministic bundle archive: %w", err)
	}
	return canonicalManifest, nil
}

func writeDeterministicZIPEntry(writer *zip.Writer, name string, data []byte) error {
	header := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: deterministicZIPTime}
	header.SetMode(0o600)
	entry, err := writer.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("create ZIP entry %q: %w", name, err)
	}
	if _, err := entry.Write(data); err != nil {
		return fmt.Errorf("write ZIP entry %q: %w", name, err)
	}
	return nil
}
