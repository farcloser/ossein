package ocispec

import "errors"

// ErrInvalidUser indicates the process user string is not a supported numeric
// uid[:gid]. ossein does not resolve names against the guest /etc/passwd.
var ErrInvalidUser = errors.New("invalid user specification")
