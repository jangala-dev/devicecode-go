//go:build pico_cm5_chunk_1024 && !pico_cm5_chunk_256

package main

const (
	chunkSize      = 1024
	chunkBase64Max = 1536
)
