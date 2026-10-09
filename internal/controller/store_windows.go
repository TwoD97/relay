package controller

// Windows does not support FlushFileBuffers on a read-only directory handle.
// saveLocked already flushes the complete temporary file before replacement.
func syncStoreDirectory(_ string) error { return nil }
