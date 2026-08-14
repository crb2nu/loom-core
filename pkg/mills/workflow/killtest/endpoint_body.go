package killtest

import (
	"fmt"
	"io"
)

const maxSafetyEndpointResponseBytes = int64(1 << 20)

// readSafetyEndpointBody reads a crash-safety REST response without allowing
// the peer to allocate unbounded harness memory. Reading max+1 bytes makes an
// exact-limit response distinguishable from a truncated oversized response.
func readSafetyEndpointBody(endpoint string, body io.Reader) ([]byte, error) {
	if body == nil {
		return nil, fmt.Errorf("%s response body is missing", endpoint)
	}
	payload, err := io.ReadAll(io.LimitReader(body, maxSafetyEndpointResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s response body: %w", endpoint, err)
	}
	if int64(len(payload)) > maxSafetyEndpointResponseBytes {
		return nil, fmt.Errorf("%s response body exceeds %d bytes", endpoint, maxSafetyEndpointResponseBytes)
	}
	return payload, nil
}
