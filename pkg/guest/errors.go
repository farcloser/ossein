package guest

import "errors"

// ErrAgent indicates a vminitd (guest agent) operation failed — every RPC
// wrapper folds its failure into this sentinel, so a caller can check
// errors.Is(err, guest.ErrAgent) for "the guest agent misbehaved".
var ErrAgent = errors.New("vminitd agent failure")
