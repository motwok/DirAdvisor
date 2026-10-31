// SPDX-License-Identifier: AGPL-3.0-or-later

// Package metrics writes the Prometheus text format 0.0.4.
package metrics

import (
	"bufio"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

const ContentType = "text/plain; version=0.0.4; charset=utf-8"

type Family struct {
	Name    string
	Help    string
	Type    string // gauge or counter
	Samples []Sample
}

type Sample struct {
	Labels map[string]string
	Value  float64
}

// Write writes all families with at least one sample. Labels with empty values
// are left out. Invalid UTF-8 (directory names from disk) is replaced, because
// Prometheus rejects the whole scrape otherwise.
func Write(w io.Writer, fams []Family) error {
	bw := bufio.NewWriter(w)
	for _, f := range fams {
		if len(f.Samples) == 0 {
			continue
		}
		bw.WriteString("# HELP " + f.Name + " " + escapeHelp(f.Help) + "\n")
		bw.WriteString("# TYPE " + f.Name + " " + f.Type + "\n")
		for _, s := range f.Samples {
			bw.WriteString(f.Name)
			writeLabels(bw, s.Labels)
			bw.WriteByte(' ')
			bw.WriteString(formatValue(s.Value))
			bw.WriteByte('\n')
		}
	}
	return bw.Flush()
}

func writeLabels(bw *bufio.Writer, l map[string]string) {
	names := make([]string, 0, len(l))
	for k, v := range l {
		if v != "" {
			names = append(names, k)
		}
	}
	if len(names) == 0 {
		return
	}
	sort.Strings(names)
	bw.WriteByte('{')
	for i, k := range names {
		if i > 0 {
			bw.WriteByte(',')
		}
		bw.WriteString(k + `="` + escapeValue(l[k]) + `"`)
	}
	bw.WriteByte('}')
}

var valueEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
var helpEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`)

func escapeValue(s string) string { return valueEscaper.Replace(strings.ToValidUTF8(s, "�")) }
func escapeHelp(s string) string  { return helpEscaper.Replace(strings.ToValidUTF8(s, "�")) }

func formatValue(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
