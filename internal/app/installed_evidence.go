package app

// Retain the complete typed error tree for classification/Is/As, but never
// format arbitrary transport, certificate, response-body or credential text.
type installedEvidenceError struct{ cause error }

func (installedEvidenceError) Error() string      { return "installed observation failed" }
func (e installedEvidenceError) String() string   { return e.Error() }
func (e installedEvidenceError) GoString() string { return e.Error() }
func (e installedEvidenceError) Unwrap() error    { return e.cause }

func redactInstalledEvidence(err error) error {
	if err == nil {
		return nil
	}
	return installedEvidenceError{cause: err}
}
