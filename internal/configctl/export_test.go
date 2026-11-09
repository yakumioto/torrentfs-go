package configctl

import "time"

// SetWriteBackForTest replaces the atomic write-back of the controller.
//
// The durability and storage failures of the write-back happen after the
// rename and are hard to provoke on a real file system, so the test drives
// them at the system boundary. The injected function gets the same arguments
// as the production writer.
func SetWriteBackForTest(controller *Controller, writeBack func(path string, document []byte) (bool, error)) {
	controller.writeBackFile = writeBack
}

// SetReloadTickerForTest replaces the poll clock of the controller.
//
// The external reload is driven by a timer in production. A test injects a
// channel it sends on, so it asserts the poll without waiting for the real
// interval.
func SetReloadTickerForTest(controller *Controller, ticker func(time.Duration) (<-chan time.Time, func())) {
	if ticker != nil {
		controller.newTicker = ticker
	}
}
