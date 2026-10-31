// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestWrite(t *testing.T) {
	var b strings.Builder
	err := Write(&b, []Family{
		{Name: "a_bytes", Help: "Bytes.\nline", Type: "gauge", Samples: []Sample{
			{Labels: map[string]string{"path": `/x "q" \ y`, "empty": "", "b": "1"}, Value: 1536},
			{Labels: nil, Value: 2e10},
			{Labels: map[string]string{"path": "/home/j\xf6rg"}, Value: 3},
		}},
		{Name: "unused", Help: "x", Type: "gauge"},
		{Name: "c_total", Help: "C.", Type: "counter", Samples: []Sample{
			{Value: math.Inf(1)}, {Value: math.Inf(-1)}, {Value: math.NaN()},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `# HELP a_bytes Bytes.\nline
# TYPE a_bytes gauge
a_bytes{b="1",path="/x \"q\" \\ y"} 1536
a_bytes 2e+10
a_bytes{path="/home/j�rg"} 3
# HELP c_total C.
# TYPE c_total counter
c_total +Inf
c_total -Inf
c_total NaN
`
	if b.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("fail") }

func TestWriteError(t *testing.T) {
	err := Write(failWriter{}, []Family{{Name: "x", Type: "gauge", Samples: []Sample{{Value: 1}}}})
	if err == nil {
		t.Fatal("expected error")
	}
}
