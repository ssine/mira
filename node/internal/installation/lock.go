package installation

import "errors"

func errorsJoin(values ...error) error { return errors.Join(values...) }
