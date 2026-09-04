package internal

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

var (
	errNotFound   = statusErr(http.StatusNotFound)
	errBadRequest = statusErr(http.StatusBadRequest)

	errMissingIDs = errors.Join(fmt.Errorf(`missing "ids"`), errBadRequest)
)

// retryAfterErr is implemented by errors that know how long a client should
// wait before trying the same request again.
type retryAfterErr interface {
	RetryAfter() time.Duration
}

type statusErr int

var _ error = (*statusErr)(nil)

func (s statusErr) Status() int {
	return int(s)
}

func (s statusErr) Error() string {
	return fmt.Sprintf("HTTP %d: %s", s, http.StatusText(int(s)))
}
