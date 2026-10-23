package analysisdata

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"unicode/utf8"
)

const (
	// MaxSourceBytes is the production run_sql JSON observation ceiling.
	MaxSourceBytes = 1 << 20
	MaxInputBytes  = 16 << 20
	maxColumns     = 32
	maxRows        = 1000
)

// ErrInvalidSource means the observation is not a supported, complete SQL table.
var ErrInvalidSource = errors.New("analysisdata: invalid SQL observation")

type source struct {
	Columns        []string
	Rows           [][]any
	RowCount       int
	Truncated      bool
	TruncateReason string
}
type metadata struct {
	Version        int      `json:"schema_version"`
	CellEncoding   string   `json:"cell_encoding"`
	Columns        []string `json:"columns"`
	ColumnIDs      []string `json:"column_ids"`
	RowCount       int      `json:"row_count"`
	Truncated      bool     `json:"truncated"`
	TruncateReason string   `json:"truncate_reason,omitempty"`
}

// FromToolJSON accepts Content only after production run_sql Execute succeeded.
// It returns independent in-memory files table.csv, table.meta.json and
// lantern_table.py. Any invalid input or cancellation returns a nil map.
// CSV headers are positional c0,c1,...; original names (including duplicate and
// empty names) live in metadata. Every CSV cell is compact JSON, so null, empty
// strings, integers and encoded text remain distinct without CSV type inference.
// Cells must match the current SQL wire values: null, string, finite number, or
// the exact encoded-text object {encoding:"base64",data:canonical-base64}.
func FromToolJSON(ctx context.Context, content string) (map[string][]byte, error) {
	if ctx == nil {
		return nil, errors.New("analysisdata: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(content) == 0 || len(content) > MaxSourceBytes {
		return nil, invalid("source must be 1..1 MiB")
	}
	if !utf8.ValidString(content) || !validSurrogates(content) {
		return nil, invalid("source must contain lossless Unicode")
	}
	src, err := decodeSource(content)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if len(src.Columns) == 0 || len(src.Columns) > maxColumns {
		return nil, invalid("column count must be 1..32")
	}
	if src.RowCount != len(src.Rows) || len(src.Rows) > maxRows {
		return nil, invalid("row count mismatch or more than 1000 rows")
	}
	if src.TruncateReason != "" && (src.TruncateReason != "bytes" || !src.Truncated) {
		return nil, invalid("inconsistent truncation metadata")
	}
	var buf bytes.Buffer
	writer := csv.NewWriter(&buf)
	header := make([]string, len(src.Columns))
	for i := range header {
		header[i] = "c" + strconv.Itoa(i)
	}
	if err = writer.Write(header); err != nil {
		return nil, err
	}
	for _, row := range src.Rows {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if len(row) != len(header) {
			return nil, invalid("row width does not match columns")
		}
		record := make([]string, len(row))
		for i, cell := range row {
			body, e := encodeCell(cell)
			if e != nil {
				return nil, invalid("cell: " + e.Error())
			}
			record[i] = string(body)
		}
		if err = writer.Write(record); err != nil {
			return nil, err
		}
		if buf.Len() > MaxInputBytes {
			return nil, invalid("CSV exceeds input budget")
		}
	}
	writer.Flush()
	if err = writer.Error(); err != nil {
		return nil, err
	}
	meta, err := json.Marshal(metadata{Version: 1, CellEncoding: "json", Columns: src.Columns, ColumnIDs: header, RowCount: src.RowCount, Truncated: src.Truncated, TruncateReason: src.TruncateReason})
	if err != nil {
		return nil, err
	}
	if buf.Len()+len(meta)+len(PythonDecoder) > MaxInputBytes {
		return nil, invalid("combined inputs exceed 16 MiB")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return map[string][]byte{"table.csv": buf.Bytes(), "table.meta.json": meta, "lantern_table.py": []byte(PythonDecoder)}, nil
}
func invalid(detail string) error { return fmt.Errorf("%w: %s", ErrInvalidSource, detail) }
func encodeCell(cell any) ([]byte, error) {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(cell); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'}), nil
}
func decodeSource(content string) (source, error) {
	var result source
	d := json.NewDecoder(bytes.NewBufferString(content))
	d.UseNumber()
	opening, err := d.Token()
	if err != nil || opening != json.Delim('{') {
		return result, invalid("expected a JSON table object")
	}
	seen := make(map[string]bool)
	for d.More() {
		key, e := d.Token()
		if e != nil {
			return result, invalid(e.Error())
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return result, invalid("duplicate/invalid object member")
		}
		seen[name] = true
		switch name {
		case "columns":
			var values []any
			e = d.Decode(&values)
			if e == nil && values != nil {
				result.Columns = make([]string, len(values))
				for i, value := range values {
					text, ok := value.(string)
					if !ok {
						return result, invalid("column names must be strings")
					}
					result.Columns[i] = text
				}
			}
		case "rows":
			var rows [][]json.RawMessage
			e = d.Decode(&rows)
			if e == nil && rows != nil {
				result.Rows = make([][]any, len(rows))
				for r, row := range rows {
					result.Rows[r] = make([]any, len(row))
					for c, raw := range row {
						value, cellErr := decodeCell(raw)
						if cellErr != nil {
							return result, cellErr
						}
						result.Rows[r][c] = value
					}
				}
			}
		case "row_count":
			var value *int
			e = d.Decode(&value)
			if e == nil {
				if value == nil {
					return result, invalid("row_count must be an integer")
				}
				result.RowCount = *value
			}
		case "truncated":
			var value *bool
			e = d.Decode(&value)
			if e == nil {
				if value == nil {
					return result, invalid("truncated must be a boolean")
				}
				result.Truncated = *value
			}
		case "truncate_reason":
			var value *string
			e = d.Decode(&value)
			if e == nil {
				if value == nil {
					return result, invalid("truncate_reason must be a string")
				}
				result.TruncateReason = *value
			}
		default:
			return result, invalid("unknown table member " + name)
		}
		if e != nil {
			return result, invalid(e.Error())
		}
	}
	if _, err = d.Token(); err != nil {
		return result, invalid(err.Error())
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return result, invalid("trailing or invalid JSON")
	}
	for _, name := range []string{"columns", "rows", "row_count", "truncated"} {
		if !seen[name] {
			return result, invalid("missing " + name)
		}
	}
	// JSON null is not an array, even though encoding/json can decode it to nil.
	if result.Columns == nil || result.Rows == nil {
		return result, invalid("columns and rows must be arrays")
	}
	return result, nil
}

// encoding/json replaces unpaired UTF-16 escapes with U+FFFD. Refuse that lossy
// case rather than silently changing the serialized SQL observation.
func validSurrogates(s string) bool {
	inString := false
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || s[i] != '\\' {
			continue
		}
		i++
		if i >= len(s) {
			return false
		}
		if s[i] != 'u' {
			continue
		}
		if i+4 >= len(s) {
			return false
		}
		value, err := strconv.ParseUint(s[i+1:i+5], 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if value >= 0xdc00 && value <= 0xdfff {
			return false
		}
		if value >= 0xd800 && value <= 0xdbff {
			if i+6 >= len(s) || s[i+1] != '\\' || s[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(s[i+3:i+7], 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}

func decodeCell(raw json.RawMessage) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if err := d.Decode(&value); err != nil {
		return nil, invalid("invalid cell JSON")
	}
	switch cell := value.(type) {
	case nil, string:
		return cell, nil
	case json.Number:
		text := string(cell)
		// Validate finite SQL wire numbers without using the float as transport.
		// REAL values such as 1e20 can be emitted as a JSON integer token beyond
		// int64, so lexical integers must not imply SQLite storage-class limits.
		number, err := strconv.ParseFloat(text, 64)
		if len(text) > 64 || err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
			return nil, invalid("cell must be a finite SQL wire number")
		}
		return cell, nil
	case map[string]any:
		// Parse members explicitly: decoding arbitrary maps first would silently
		// coalesce duplicate keys and make a false losslessness promise.
		object := json.NewDecoder(bytes.NewReader(raw))
		_, _ = object.Token()
		seen := map[string]bool{}
		fields := map[string]string{}
		for object.More() {
			key, err := object.Token()
			if err != nil {
				return nil, invalid("invalid encoded-text object")
			}
			name, ok := key.(string)
			if !ok || seen[name] || (name != "encoding" && name != "data") {
				return nil, invalid("invalid encoded-text members")
			}
			seen[name] = true
			var field *string
			if err = object.Decode(&field); err != nil || field == nil {
				return nil, invalid("encoded-text values must be strings")
			}
			fields[name] = *field
		}
		if len(fields) != 2 || fields["encoding"] != "base64" {
			return nil, invalid("unsupported cell object")
		}
		decoded, err := base64.StdEncoding.Strict().DecodeString(fields["data"])
		if err != nil || base64.StdEncoding.EncodeToString(decoded) != fields["data"] {
			return nil, invalid("invalid canonical encoded-text base64")
		}
		return fields, nil
	default:
		return nil, invalid("unsupported SQL cell type")
	}
}
