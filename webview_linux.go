package main

import "os"

// WebKitGTK's DMABUF renderer paces frames at a quarter of a 144 Hz display
// on this class of hybrid-GPU Wayland laptop: scrolling measured 36 fps with
// it and ~50 fps without (NEXORA_PERF benchmark). Respect an explicit choice.
func init() {
	if _, set := os.LookupEnv("WEBKIT_DISABLE_DMABUF_RENDERER"); !set {
		_ = os.Setenv("WEBKIT_DISABLE_DMABUF_RENDERER", "1")
	}
}
