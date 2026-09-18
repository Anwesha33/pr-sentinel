package analysis

import (
	"errors"
	"os/exec"
)

func asExitError(err error, target **exec.ExitError) bool {
	return errors.As(err, target)
}
