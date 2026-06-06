package orange

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"
)

// Format controls how results are rendered.
type Format string

const (
	FormatTable Format = "table"
	FormatJSON  Format = "json"
	FormatYAML  Format = "yaml"
)

// Printer writes command output.
type Printer struct {
	Format  Format
	Quiet   bool
	NoColor bool
	Out     io.Writer
}

// DefaultPrinter returns a Printer pointing at stdout using table format.
func DefaultPrinter() *Printer {
	return &Printer{Format: FormatTable, Out: os.Stdout}
}

// Proto prints a proto.Message according to the configured format.
func (p *Printer) Proto(msg proto.Message) error {
	switch p.Format {
	case FormatJSON:
		b, err := protojson.MarshalOptions{Multiline: true, EmitUnpopulated: false}.Marshal(msg)
		if err != nil {
			return err
		}
		fmt.Fprintln(p.Out, string(b))
	case FormatYAML:
		// Marshal to JSON first, then transcode to YAML for proto-aware field names.
		b, err := protojson.MarshalOptions{EmitUnpopulated: false}.Marshal(msg)
		if err != nil {
			return err
		}
		var raw any
		if err := json.Unmarshal(b, &raw); err != nil {
			return err
		}
		out, err := yaml.Marshal(raw)
		if err != nil {
			return err
		}
		fmt.Fprint(p.Out, string(out))
	default:
		// table: pretty-print JSON (proto-aware field names, no nulls)
		b, err := protojson.MarshalOptions{Multiline: true, EmitUnpopulated: false}.Marshal(msg)
		if err != nil {
			return err
		}
		fmt.Fprintln(p.Out, string(b))
	}
	return nil
}

// ProtoList prints a slice of proto.Message as a JSON array or YAML list.
func (p *Printer) ProtoList(msgs []proto.Message) error {
	switch p.Format {
	case FormatYAML:
		var rows []any
		for _, m := range msgs {
			b, err := protojson.MarshalOptions{EmitUnpopulated: false}.Marshal(m)
			if err != nil {
				return err
			}
			var raw any
			if err := json.Unmarshal(b, &raw); err != nil {
				return err
			}
			rows = append(rows, raw)
		}
		out, err := yaml.Marshal(rows)
		if err != nil {
			return err
		}
		fmt.Fprint(p.Out, string(out))
	default:
		fmt.Fprintln(p.Out, "[")
		for i, m := range msgs {
			b, err := protojson.MarshalOptions{Multiline: true, EmitUnpopulated: false}.Marshal(m)
			if err != nil {
				return err
			}
			comma := ","
			if i == len(msgs)-1 {
				comma = ""
			}
			fmt.Fprintf(p.Out, "  %s%s\n", string(b), comma)
		}
		fmt.Fprintln(p.Out, "]")
	}
	return nil
}

// Table prints rows with a header line using tabwriter.
func (p *Printer) Table(header string, rows []string) {
	if !p.Quiet {
		w := tabwriter.NewWriter(p.Out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, header)
		for _, r := range rows {
			fmt.Fprintln(w, r)
		}
		_ = w.Flush()
	} else {
		for _, r := range rows {
			fmt.Fprintln(p.Out, r)
		}
	}
}

// OK prints a simple success confirmation.
func (p *Printer) OK(msg string) {
	fmt.Fprintln(p.Out, msg)
}
