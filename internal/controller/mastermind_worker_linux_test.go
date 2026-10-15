package controller

import (
	"bytes"
	"io"
	"testing"
)

// io.Copy must use the bounded Write method, including on an ordinary Reader
// without WriterTo. Embedding bytes.Buffer would bypass that boundary.
func TestMastermindWorkerCaptureCannotBypassLimit(t *testing.T) {
	output := &mastermindWorkerCapture{limit: 17}
	reader := struct{ io.Reader }{bytes.NewReader(bytes.Repeat([]byte("x"), 20000))}
	n, err := io.Copy(output, reader)
	if err != nil || n != 20000 || output.buffer.Len() != 17 || !output.overflow {
		t.Fatalf("capture limit bypassed: read=%d kept=%d overflow=%v err=%v", n, output.buffer.Len(), output.overflow, err)
	}
}
