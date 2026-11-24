// SPDX-License-Identifier: GPL-3.0-or-later

package acp

import (
	"io"
	"strings"
	"testing"
)

func TestEOFReaderSignalsOnceAtEOF(t *testing.T) {
	calls := 0
	r := &eofReader{r: strings.NewReader("payload"), onEOF: func() { calls++ }}
	if _, err := io.ReadAll(r); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("onEOF calls = %d, want 1", calls)
	}
}
