package desktopfiles

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestLongUTF8SampleKeepsBytePagesAligned(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "unicode.txt")
	data := bytes.Repeat([]byte("😀"), MaxReadBytes/4+2)
	err := os.WriteFile(path, data, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	files := New()
	t.Cleanup(files.CloseAll)
	opened, err := files.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	page, err := files.Read(opened.Handle, 0, 5)
	if err != nil || page.ContentText != "😀" || page.NextOffset != 4 || page.EndOfFile {
		t.Fatalf("UTF-8 byte page = %#v, %v", page, err)
	}
}

func TestShortUTF8ReadMakesProgressWithoutFalseEOF(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "unicode.txt")
	err := os.WriteFile(path, []byte("😀tail"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	files := New()
	t.Cleanup(files.CloseAll)
	opened, err := files.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	page, err := files.Read(opened.Handle, 0, 1)
	if err != nil || page.ByteLength != 1 || page.NextOffset != 1 || page.EndOfFile || page.Encoding != "base64" {
		t.Fatalf("short byte page = %#v, %v", page, err)
	}
}
