package main

import (
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

type grpIconDir struct {
	ico.ICONDIR
	Entries []grpIconDirEntry
}

func (g grpIconDir) Size() int64 {
	return int64(binary.Size(g.ICONDIR) + len(g.Entries)*binary.Size(g.Entries[0]))
}

type grpIconDirEntry struct {
	ico.IconDirEntryCommon
	ID uint16
}

func main() {
	var iconPath, outPath string
	var groupID int
	flag.StringVar(&iconPath, "ico", "", "input .ico file")
	flag.StringVar(&outPath, "o", "", "output .syso file")
	flag.IntVar(&groupID, "group-id", 3, "RT_GROUP_ICON resource id")
	flag.Parse()
	if iconPath == "" || outPath == "" {
		flag.Usage()
		os.Exit(2)
	}
	if groupID <= 0 || groupID > 0xffff {
		exitf("invalid group-id %d", groupID)
	}
	if err := generate(iconPath, outPath, uint16(groupID)); err != nil {
		exitf("%v", err)
	}
}

func generate(iconPath, outPath string, groupID uint16) error {
	f, err := os.Open(iconPath)
	if err != nil {
		return err
	}
	defer f.Close()
	icons, err := ico.DecodeHeaders(f)
	if err != nil {
		return err
	}
	out := coff.NewRSRC()
	if err := out.Arch("amd64"); err != nil {
		return err
	}
	group := grpIconDir{ICONDIR: ico.ICONDIR{Reserved: 0, Type: 1, Count: uint16(len(icons))}}
	nextID := groupID
	for _, icon := range icons {
		nextID++
		reader := io.NewSectionReader(f, int64(icon.ImageOffset), int64(icon.BytesInRes))
		out.AddResource(coff.RT_ICON, nextID, reader)
		group.Entries = append(group.Entries, grpIconDirEntry{IconDirEntryCommon: icon.IconDirEntryCommon, ID: nextID})
	}
	out.AddResource(coff.RT_GROUP_ICON, groupID, group)
	out.Freeze()
	return writeCOFF(out, outPath)
}

func writeCOFF(c *coff.Coff, outPath string) error {
	out, err := os.Create(outPath)
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
		if sized, ok := v.Interface().(binutil.SizedReader); ok {
			w.WriteFromSized(sized)
			return binutil.WALK_SKIP
		}
		return nil
	})
	if w.Err != nil {
		return fmt.Errorf("write output: %w", w.Err)
	}
	return nil
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
