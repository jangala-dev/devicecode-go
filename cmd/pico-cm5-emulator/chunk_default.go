//go:build !pico_cm5_chunk_256 && !pico_cm5_chunk_1024

package main

const (
	chunkSize      = 2048
	chunkBase64Max = 3072
)
