package buildkit

import (
	"archive/tar"
	"bytes"
	"testing"
)

func TestExtractLayerRejectsSymlinkEscape(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "evil", Typeflag: tar.TypeSymlink, Linkname: "../../etc/passwd", Mode: 0o777}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	err := ExtractLayer(bytes.NewReader(buf.Bytes()), "layer.tar", t.TempDir(), 0)
	if err == nil || err.Error() != "symlink escapes destination" {
		t.Fatalf("err = %v", err)
	}
}
