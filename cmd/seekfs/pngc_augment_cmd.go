package main

import (
	"context"
	"flag"
	"fmt"
	"time"
)

func cmdAugmentPNGC(args []string) error {
	fs := flag.NewFlagSet("augment-pngc", flag.ContinueOnError)
	db := fs.String("db", "", "owned v9 source index")
	out := fs.String("out", "", "new target index; must not already exist")
	maxGrowth := fs.Int64("max-output-growth", 0, "hard output-growth ceiling in bytes")
	maxHeap := fs.Uint64("max-heap", 0, "hard Go heap-in-use ceiling in bytes")
	maxScratch := fs.Int64("max-scratch", 1<<30, "hard PNGC scratch-spool ceiling in bytes")
	minFree := fs.Uint64("min-free-disk", 0, "required free disk headroom in bytes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *db == "" || *out == "" {
		return fmt.Errorf("augment-pngc requires -db and -out")
	}
	result, err := augmentPNGC(context.Background(), *db, *out, PNGCAugmentOptions{
		MaxOutputGrowth:  *maxGrowth,
		MaxHeapBytes:     *maxHeap,
		MaxScratchBytes:  *maxScratch,
		MinFreeDiskBytes: *minFree,
	})
	if err != nil {
		return err
	}
	fmt.Printf("source_bytes=%d target_bytes=%d pngc_bytes=%d scratch_bytes=%d output_growth=%d wall=%s source_sha256=%s target_sha256=%s\n",
		result.SourceBytes, result.TargetBytes, result.PNGCBytes, result.ScratchBytes, result.OutputGrowth, result.Wall.Round(time.Millisecond), result.SourceSHA256, result.TargetSHA256)
	return nil
}
