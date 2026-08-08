package helper

import "errors"

type vmnetStopError struct {
	err      error
	retained bool
}

func (e *vmnetStopError) Error() string {
	return e.err.Error()
}

func (e *vmnetStopError) Unwrap() error {
	return e.err
}

func wrapVMNetStopError(err error, retained bool) error {
	if err == nil {
		return nil
	}
	return &vmnetStopError{err: err, retained: retained}
}

func stopErrorRetained(err error) bool {
	var target *vmnetStopError
	return errors.As(err, &target) && target.retained
}
