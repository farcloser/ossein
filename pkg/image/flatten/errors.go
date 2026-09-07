package flatten

import "errors"

// ErrFlatten indicates the layer stack could not be flattened into a single
// tar stream (unreadable layer, malformed entry, or spool I/O failure).
var ErrFlatten = errors.New("image flatten failure")
