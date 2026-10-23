// Package analysisdata converts a successful run_sql observation into bounded,
// read-only sandbox inputs. It never opens a database, executes SQL or writes to
// disk. It preserves the observable tool JSON, not SQLite storage-class metadata
// absent from that JSON (notably BLOB/base64-string versus ordinary TEXT).
package analysisdata
