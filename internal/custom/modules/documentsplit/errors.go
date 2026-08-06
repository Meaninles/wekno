package documentsplit

import (
	"errors"
	"fmt"
)

// RemoteError preserves the document-splitter service's machine-readable
// failure contract across gRPC. Retryable=false is a terminal content or
// policy rejection; retryable=true represents an infrastructure failure that
// may succeed on another attempt.
type RemoteError struct {
	Code      string
	Message   string
	Retryable bool
}

func (e *RemoteError) Error() string {
	if e == nil {
		return "document split failed"
	}
	return fmt.Sprintf(
		"document split rejected code=%s retryable=%t: %s",
		e.Code,
		e.Retryable,
		e.Message,
	)
}

// IsPermanent reports whether err contains a typed splitter rejection that
// explicitly opted out of retry. It intentionally does not classify unknown
// transport or parser errors as permanent.
func IsPermanent(err error) bool {
	var remote *RemoteError
	return errors.As(err, &remote) && !remote.Retryable
}
