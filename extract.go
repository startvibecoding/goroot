package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"goroot/sandbox"
)

// cmdExtract unpacks a (possibly compressed) tar archive into dest, discarding
// ownership so the tree is owned by the invoking user.
func cmdExtract(args []string) int {
	var (
		src   string
		dest  string
		strip int
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-c" || a == "--strip":
			i++
			fmt.Sscanf(args[i], "%d", &strip)
		case strings.HasPrefix(a, "-c"):
			fmt.Sscanf(strings.TrimPrefix(a, "-c"), "%d", &strip)
		case src == "":
			src = a
		case dest == "":
			dest = a
		default:
			fatal("extract: unexpected argument %q", a)
		}
	}
	if src == "" || dest == "" {
		fmt.Fprintln(os.Stderr, "usage: goroot extract [-c N] <tarball|url> <dest>")
		return 2
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		fatal("extract: %v", err)
	}

	r, err := openArchive(src)
	if err != nil {
		fatal("extract: %v", err)
	}
	defer r.Close()

	if err := sandbox.Extract(r, dest, src, strip); err != nil {
		fatal("extract: %v", err)
	}
	return 0
}

func openArchive(src string) (io.ReadCloser, error) {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		c := &http.Client{Timeout: 5 * time.Minute}
		resp, err := c.Get(src)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("GET %s: %s", src, resp.Status)
		}
		return resp.Body, nil
	}
	return os.Open(src)
}
