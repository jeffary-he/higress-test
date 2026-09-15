// Offline helper reads exact stdin bytes, never tokens from process arguments.
package main

import (
	"fmt"
	"gray-whitelist-wasm/internal/whitelist"
	"io"
	"os"
)

func main() {
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 65537))
	if err != nil || len(data) > 65536 {
		fmt.Fprintln(os.Stderr, "input read failed or exceeds 64 KiB")
		os.Exit(1)
	}
	digest, ok := whitelist.Digest(string(data))
	if !ok {
		fmt.Fprintln(os.Stderr, "empty token")
		os.Exit(1)
	}
	fmt.Println(digest)
}
