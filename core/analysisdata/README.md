# SQL observation → Python table

`FromToolJSON(ctx, content)` consumes `tools.Result.Content` **after** the real
`run_sql` tool succeeds. It never opens a database, repeats a query or writes to
a shared directory. The returned map can become `sandbox.Spec.Inputs` directly.
An invalid observation or cancellation returns an error and a nil map.

The source is at most 1 MiB, with 1–32 columns and 0–1000 rows. Row counts and
widths must match. Required fields, scalar metadata types, truncation metadata,
Unicode and encoded-text objects are checked. Unpaired UTF-16 JSON escapes are
rejected rather than silently replaced. Duplicate top-level or encoded-text keys
are rejected. Cells accept the current SQL wire types: null, string, finite
number (validated as finite without changing its original token), or the exact canonical-base64 encoded-text
object. All returned files together are capped at 16 MiB.

The files are:

- `table.csv`: UTF-8, positional headers `c0,c1,...`, with each cell independently
  encoded as compact JSON. A SQL NULL is the field `null`; an empty string is
  the JSON field `""`. Ordinary CSV quoting is applied around that JSON, so
  commas, quotes, newlines and Unicode round-trip safely.
- `table.meta.json`: `schema_version:1`, `cell_encoding:"json"`, `column_ids`,
  original `columns` in the same order, `row_count`, `truncated`, and optional
  `truncate_reason:"bytes"`. Original empty and duplicate column names survive.
  A true truncation flag without a reason denotes the existing row-limit case.
- `lantern_table.py`: fixed decoder source, also exported as `PythonDecoder`.

`UseNumber` avoids rounding integers through Go float64. The Python helper returns
Python int for integer JSON tokens and Decimal for tokens with a fraction or
exponent. It raises Python's CSV field limit to a bounded 16 MiB because the
normal 128 KiB default would reject valid SQL strings.

With Python `-I`, the input directory is not automatically added to `sys.path`.
Load the fixed helper explicitly inside the sandbox:

```python
import os, runpy
load_table = runpy.run_path(
    os.path.join(os.environ['INPUT_DIR'], 'lantern_table.py')
)['load_table']
columns, rows, metadata = load_table()
```

Alternatively, prepend the exported `PythonDecoder` constant as fixed source
before the sandbox code. Neither path evaluates data cells as Python source.
When constructing a pandas frame, use positional columns and explicit object
dtype if preservation of null/integer values is required; direct `read_csv`
inference does not implement this wire format.

This preserves **observable tool JSON**, not missing SQLite storage metadata.
The current SQL tool represents BLOB as an ordinary base64 string, so it cannot
be distinguished from an identical TEXT string. Invalid UTF-8 TEXT remains the
explicit `{encoding:"base64",data:"..."}` object; the helper does not silently
convert it to replacement characters or guess its type. JSON number tokens also
do not recover whether SQLite originally stored `1` as INTEGER or REAL.
