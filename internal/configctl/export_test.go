package configctl

// SetWriteBackForTest replaces the atomic write-back of the controller.
//
// The durability and storage failures of the write-back happen after the
// rename and are hard to provoke on a real file system, so the test drives
// them at the system boundary. The injected function gets the same arguments
// as the production writer.
func SetWriteBackForTest(controller *Controller, writeBack func(path string, document []byte) (bool, error)) {
	controller.writeBackFile = writeBack
}
