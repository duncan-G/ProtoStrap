package infralog

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// WriteText writes a result as lines, one per record, for people and agents to read:
//
//	2026-10-29T08:30:00.000Z INFO  agent     general-3 api starting a replica replica=api-x7k2qa image=api:c-1
//
// that is, the time in UTC, the level, the source, the VM, the component or -, the message
// and the attributes. A value with spaces is quoted. A last line starting with # says when
// more records matched than were shown.
func WriteText(w io.Writer, res Result) error {
	bw := bufio.NewWriter(w)
	if len(res.Records) == 0 {
		fmt.Fprintln(bw, "# no records match")
	}
	for _, r := range res.Records {
		vm := cmpOr(r.VMName, r.VM, "-")
		fmt.Fprintf(bw, "%s %-5s %-9s %s %s %s", r.Time.UTC().Format("2006-01-02T15:04:05.000Z"), r.Level, r.Source, vm,
			cmpOr(r.Component, "-"), oneLine(r.Msg))
		for _, a := range r.Attrs {
			fmt.Fprintf(bw, " %s=%s", a.Key, quote(a.Value))
		}
		bw.WriteByte('\n')
	}
	if res.More {
		fmt.Fprintf(bw, "# more records match than the %d shown: narrow the filters or the window, or raise limit (at most %d)\n", len(res.Records), MaxLimit)
	}
	return bw.Flush()
}

func cmpOr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func oneLine(s string) string { return strings.NewReplacer("\r", `\r`, "\n", `\n`).Replace(s) }

func quote(v string) string {
	if v == "" || strings.ContainsAny(v, " \t\r\n\"=") {
		return strconv.Quote(v)
	}
	return v
}
