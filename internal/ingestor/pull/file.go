package pull

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/open-streamer/open-streamer/internal/domain"
)

const fileReadChunk = 188 * 56 // ~10 KiB per read

// FileReader reads a local media file and emits its bytes as MPEG-TS chunks.
// If the input URL has the query parameter "loop=true", the file is read
// repeatedly from the beginning after reaching EOF.
//
// Expected URL format: file:///absolute/path/to/source.ts
// Or plain path: /absolute/path/to/source.ts.
type FileReader struct {
	input domain.Input
	path  string
	loop  bool
	f     *os.File
	buf   []byte
}

// NewFileReader constructs a FileReader for the given input.
func NewFileReader(input domain.Input) *FileReader {
	path, loop := parseFileURL(input.URL)
	return &FileReader{
		input: input,
		path:  path,
		loop:  loop,
		buf:   make([]byte, fileReadChunk),
	}
}

// Open opens the underlying file for reading.
func (r *FileReader) Open(_ context.Context) error {
	f, err := os.Open(r.path)
	if err != nil {
		return fmt.Errorf("file reader: open %q: %w", r.path, err)
	}
	r.f = f
	return nil
}

func (r *FileReader) Read(ctx context.Context) ([]byte, error) {
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		n, err := r.f.Read(r.buf)
		if n > 0 {
			out := make([]byte, n)
			copy(out, r.buf[:n])
			return out, nil
		}
		if err == io.EOF {
			if !r.loop {
				return nil, io.EOF
			}
			// Seek back to the beginning for looping.
			if _, seekErr := r.f.Seek(0, io.SeekStart); seekErr != nil {
				return nil, fmt.Errorf("file reader: seek to start: %w", seekErr)
			}
			continue
		}
		return nil, fmt.Errorf("file reader: read: %w", err)
	}
}

// Close closes the open file, if any.
func (r *FileReader) Close() error {
	if r.f != nil {
		return r.f.Close()
	}
	return nil
}

// parseFileURL extracts the filesystem path and loop flag from a URL.
// Accepts both "file:///path" and plain absolute paths.
func parseFileURL(rawURL string) (path string, loop bool) {
	if strings.HasPrefix(rawURL, "file://") {
		u, err := url.Parse(rawURL)
		if err == nil {
			loop = u.Query().Get("loop") == "true"
			return u.Path, loop
		}
	}
	return rawURL, false
}
