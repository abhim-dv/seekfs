// ui-rsrcgen generates the seekfs-ui resource object (.syso) with the
// application manifest at resource id 1 and the icon group at resource id 3.
//
// The id of the icon group matters: Wails loads the window icon with
// LoadIcon(hInstance, winc.AppIconID) and winc.AppIconID is hardcoded to 3.
// akavel/rsrc's Embed assigns ids sequentially (manifest 1, group 2), which
// breaks that lookup, so the group icon would never appear in the title bar.
// This tool mirrors windres numbering (group 3, frames group+1..) instead.
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"

	"github.com/akavel/rsrc/binutil"
	"github.com/akavel/rsrc/coff"
	"github.com/akavel/rsrc/ico"
)

type sizedBytes struct {
	r *bytes.Reader
	n int64
}

func newSizedBytes(b []byte) *sizedBytes {
	return &sizedBytes{r: bytes.NewReader(b), n: int64(len(b))}
}

func (s *sizedBytes) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s *sizedBytes) Size() int64                { return s.n }

func main() {
	arch := flag.String("arch", "amd64", "target architecture")
	icoPath := flag.String("ico", "", "path to .ico file")
	manifest := flag.String("manifest", "", "path to application manifest XML")
	out := flag.String("o", "", "output .syso path")
	groupID := flag.Int("group", 3, "RT_GROUP_ICON resource id (Wails default AppIconID is 3)")
	flag.Parse()
	if *icoPath == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "ui-rsrcgen: -ico and -o are required")
		os.Exit(2)
	}

	f, err := os.Open(*icoPath)
	if err != nil {
		fatal(err)
	}
	defer f.Close()
	entries, err := ico.DecodeHeaders(f)
	if err != nil {
		fatal(fmt.Errorf("decode %s: %w", *icoPath, err))
	}
	if len(entries) == 0 {
		fatal(fmt.Errorf("%s contains no icon frames", *icoPath))
	}

	c := coff.NewRSRC()
	if err := c.Arch(*arch); err != nil {
		fatal(err)
	}

	if *manifest != "" {
		mf, err := binutil.SizedOpen(*manifest)
		if err != nil {
			fatal(err)
		}
		defer mf.Close()
		c.AddResource(coff.RT_MANIFEST, 1, mf)
	}

	var group bytes.Buffer
	if err := binary.Write(&group, binary.LittleEndian, uint16(0)); err != nil { // Reserved
		fatal(err)
	}
	if err := binary.Write(&group, binary.LittleEndian, uint16(1)); err != nil { // Type: icon
		fatal(err)
	}
	if err := binary.Write(&group, binary.LittleEndian, uint16(len(entries))); err != nil { // Count
		fatal(err)
	}
	for i, entry := range entries {
		imageID := uint16(*groupID + 1 + i)
		r := io.NewSectionReader(f, int64(entry.ImageOffset), int64(entry.BytesInRes))
		c.AddResource(coff.RT_ICON, imageID, r)
		for _, v := range []interface{}{
			entry.Width, entry.Height, entry.ColorCount, entry.Reserved,
			entry.Planes, entry.BitCount, entry.BytesInRes, imageID,
		} {
			if err := binary.Write(&group, binary.LittleEndian, v); err != nil {
				fatal(err)
			}
		}
	}
	c.AddResource(coff.RT_GROUP_ICON, uint16(*groupID), newSizedBytes(group.Bytes()))

	c.Freeze()
	if err := writeCoff(c, *out); err != nil {
		fatal(err)
	}
}

func writeCoff(c *coff.Coff, fname string) error {
	out, err := os.Create(fname)
	if err != nil {
		return err
	}
	defer out.Close()
	w := binutil.Writer{W: out}
	binutil.Walk(c, func(v reflect.Value, path string) error {
		if binutil.Plain(v.Kind()) {
			w.WriteLE(v.Interface())
			return nil
		}
		if vv, ok := v.Interface().(binutil.SizedReader); ok {
			w.WriteFromSized(vv)
			return binutil.WALK_SKIP
		}
		return nil
	})
	if w.Err != nil {
		return w.Err
	}
	return out.Close()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "ui-rsrcgen:", err)
	os.Exit(1)
}
