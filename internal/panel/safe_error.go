package panel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
)

// Do not interpolate untrusted request URLs, response bodies or transport
// error strings: these may contain query credentials or reflected secrets.
// Preserve a bounded diagnostic category rather than the raw error.
func safePanelTransportError(err error) error {
	var ne net.Error
	var dns *net.DNSError
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("panel transport canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("panel transport timeout")
	case errors.Is(err, syscall.ECONNREFUSED):
		return fmt.Errorf("panel transport connection refused")
	case errors.Is(err, syscall.ECONNRESET):
		return fmt.Errorf("panel transport connection reset")
	case errors.As(err, &dns):
		return fmt.Errorf("panel transport DNS failure")
	case errors.As(err, &ne) && ne.Timeout():
		return fmt.Errorf("panel transport timeout")
	default:
		return fmt.Errorf("panel transport failure")
	}
}
