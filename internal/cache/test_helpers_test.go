package cache

import "os"

// statFn is an indirection so io_test.go stays import-narrow.
var statFn = func(path string) (any, error) {
	return os.Stat(path)
}
