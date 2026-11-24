package pi

import "time"

// SetCallTimeout lets a test start the driver under the generous default
// call timeout (spawn and the get_state handshake are slow on a loaded
// -race runner) and only then tighten it for the call under test.
func (d *Driver) SetCallTimeout(timeout time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.opts.CallTimeout = timeout
}
