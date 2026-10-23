//go:build !unix

package pyrt

import "errors"

func lockPrepared(string) (func(), error) {
	return nil, errors.New("prepared runtime leases require Unix file locking")
}
func readPreparedRegular(string, int64) ([]byte, error) {
	return nil, errors.New("prepared runtime validation is unavailable on this platform")
}
