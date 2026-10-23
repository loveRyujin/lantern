package sandbox

import (
	"bytes"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"unicode/utf8"
)

// PNG limits bound decoded pixel storage before the trusted collector decodes.
// They do not describe the complete decoder's peak memory consumption.
const (
	MaxPNGDimension = 8192
	MaxPNGPixels    = 4_000_000
)

func validateArtifactContent(a Artifact) error {
	switch a.Kind {
	case KindPNG:
		config, err := png.DecodeConfig(bytes.NewReader(a.Data))
		if err != nil {
			return fmt.Errorf("invalid PNG configuration: %w", err)
		}
		if config.Width <= 0 || config.Height <= 0 || config.Width > MaxPNGDimension || config.Height > MaxPNGDimension || uint64(config.Width)*uint64(config.Height) > MaxPNGPixels {
			return fmt.Errorf("PNG exceeds dimension/pixel budget")
		}
		if err := pngEndsAtIEND(a.Data); err != nil {
			return err
		}
		if _, err := png.Decode(bytes.NewReader(a.Data)); err != nil {
			return fmt.Errorf("invalid PNG: %w", err)
		}
	case KindCSV:
		if !utf8.Valid(a.Data) || bytesContainNUL(a.Data) {
			return fmt.Errorf("CSV must be UTF-8 without NUL")
		}
		reader := csv.NewReader(bytes.NewReader(a.Data))
		records := 0
		for {
			_, err := reader.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("invalid CSV: %w", err)
			}
			records++
		}
		if records == 0 {
			return fmt.Errorf("CSV requires a record")
		}
	case KindJSON:
		if !utf8.Valid(a.Data) || !json.Valid(a.Data) {
			return fmt.Errorf("invalid UTF-8 JSON")
		}
	}
	return nil
}

// Walk length-delimited chunks, rather than looking at the last twelve bytes:
// concatenated PNGs also end in IEND. png.Decode separately verifies structure,
// chunk CRCs and compressed image data.
func pngEndsAtIEND(data []byte) error {
	if !hasPNGSignature(data) {
		return fmt.Errorf("invalid PNG signature")
	}
	for offset := uint64(8); offset+12 <= uint64(len(data)); {
		length := uint64(binary.BigEndian.Uint32(data[offset : offset+4]))
		end := offset + 12 + length
		if end > uint64(len(data)) {
			return fmt.Errorf("truncated PNG chunk")
		}
		if string(data[offset+4:offset+8]) == "IEND" {
			if length != 0 || end != uint64(len(data)) {
				return fmt.Errorf("PNG has invalid IEND or trailing data")
			}
			return nil
		}
		offset = end
	}
	return fmt.Errorf("PNG missing IEND")
}
