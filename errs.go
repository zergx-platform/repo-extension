package main

import "fmt"

// opError carries an HTTP status. Statuses < 500 are "permanent": the
// operation cannot succeed and the caller sees a 4xx. 5xx-class errors are
// "transient" (downstream unavailable): the caller may retry — under the
// lazy model every step is idempotent, so retries converge.
type opError struct {
	status int
	msg    string
}

func (e *opError) Error() string { return e.msg }

func errPermanent(status int, format string, args ...interface{}) error {
	return &opError{status: status, msg: fmt.Sprintf(format, args...)}
}

func errNotFound(format string, args ...interface{}) error {
	return errPermanent(404, format, args...)
}

func errConflict(format string, args ...interface{}) error {
	return errPermanent(409, format, args...)
}

func errBad(format string, args ...interface{}) error {
	return errPermanent(400, format, args...)
}

func errDownstream(what string, err error) error {
	return &opError{status: 502, msg: fmt.Sprintf("%s 不可用：%v", what, err)}
}

// statusOf maps an error to its HTTP status (default 502 transient).
func statusOf(err error) int {
	if oe, ok := err.(*opError); ok {
		return oe.status
	}
	return 502
}

// isPermanent reports whether the error is a terminal 4xx failure (never
// retryable); transient 5xx-class errors are retried and converge because
// every workspace step is idempotent.
func isPermanent(err error) bool { return statusOf(err) < 500 }
