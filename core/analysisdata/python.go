package analysisdata

// PythonDecoder is fixed trusted source defining load_table(input_dir=None).
// It returns (original_columns, decoded_rows, metadata), preserving positional
// duplicate column names. Integers use Python int; decimal/exponent JSON numbers
// use Decimal. It never invokes eval, pickle, shell commands or pandas inference.
// With Python -I use runpy.run_path(INPUT_DIR+'/lantern_table.py')['load_table']()
// or prepend this constant as fixed source before the caller's sandbox code.
const PythonDecoder = `import csv as _lantern_csv
import json as _lantern_json
import os as _lantern_os
from decimal import Decimal as _LanternDecimal
from pathlib import Path as _LanternPath

def load_table(input_dir=None):
    root = _LanternPath(input_dir if input_dir is not None else _lantern_os.environ['INPUT_DIR'])
    with (root / 'table.meta.json').open('r', encoding='utf-8') as handle:
        meta = _lantern_json.load(handle)
    if meta.get('schema_version') != 1 or meta.get('cell_encoding') != 'json':
        raise ValueError('unsupported Lantern table encoding')
    columns = meta['columns']
    if not isinstance(columns, list) or not 1 <= len(columns) <= 32 or not all(isinstance(c, str) for c in columns):
        raise ValueError('invalid Lantern columns')
    column_ids = meta.get('column_ids')
    if column_ids != ['c%d' % i for i in range(len(columns))]:
        raise ValueError('invalid Lantern column IDs')
    rows = []
    # Python csv defaults to 128 KiB fields, smaller than valid run_sql cells.
    _lantern_csv.field_size_limit(16 * 1024 * 1024)
    with (root / 'table.csv').open('r', encoding='utf-8', newline='') as handle:
        reader = _lantern_csv.reader(handle, strict=True)
        if next(reader, None) != column_ids:
            raise ValueError('invalid Lantern CSV header')
        for record in reader:
            if len(record) != len(columns) or len(rows) >= 1000:
                raise ValueError('invalid Lantern row shape')
            rows.append([_lantern_json.loads(cell, parse_float=_LanternDecimal) for cell in record])
    if type(meta['row_count']) is not int or meta['row_count'] != len(rows):
        raise ValueError('invalid Lantern row count')
    return columns, rows, meta
`
