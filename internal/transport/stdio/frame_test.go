package stdio

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"
)

func readAll(t *testing.T, input string, max int) []frame {
	t.Helper()
	r := bufio.NewReaderSize(strings.NewReader(input), 16) // a tiny buffer exercises the long-line path
	var out []frame
	for {
		f, err := readFrame(r, max)
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
}

func TestReadFrameSplitsOnNewlineAndDropsOneCarriageReturn(t *testing.T) {
	frames := readAll(t, "{\"a\":1}\n{\"b\":2}\r\n\n  \n{\"c\":3}", 1024)
	want := []string{`{"a":1}`, `{"b":2}`, "", "  ", `{"c":3}`}
	if len(frames) != len(want) {
		t.Fatalf("%d frames: %+v", len(frames), frames)
	}
	for i, f := range frames {
		if f.tooLong || string(f.data) != want[i] {
			t.Errorf("frame %d: %q (tooLong=%v), want %q", i, f.data, f.tooLong, want[i])
		}
	}
}

func TestReadFrameLimitIsOnTheLineWithoutItsNewline(t *testing.T) {
	line := strings.Repeat("x", 100)
	frames := readAll(t, line+"\n"+line+"y\n"+line+"\n", 100)
	if len(frames) != 3 {
		t.Fatalf("%d frames", len(frames))
	}
	if frames[0].tooLong || len(frames[0].data) != 100 {
		t.Fatalf("a line exactly at the limit is accepted: %+v", frames[0])
	}
	if !frames[1].tooLong || frames[1].data != nil {
		t.Fatalf("a line one byte over is refused and not kept: %+v", frames[1])
	}
	if frames[2].tooLong || len(frames[2].data) != 100 {
		t.Fatalf("the line after a refused one is read normally: %+v", frames[2])
	}
}

func TestReadFrameDiscardsAnOversizeLineWithoutBufferingIt(t *testing.T) {
	huge := strings.Repeat("z", 5<<20)
	r := bufio.NewReaderSize(strings.NewReader(huge+"\n{\"ok\":true}\n"), 4096)
	f, err := readFrame(r, 1024)
	if err != nil || !f.tooLong || f.data != nil {
		t.Fatalf("%v %+v", err, f)
	}
	f, err = readFrame(r, 1024)
	if err != nil || f.tooLong || !bytes.Equal(f.data, []byte(`{"ok":true}`)) {
		t.Fatalf("%v %+v", err, f)
	}
	if _, err := readFrame(r, 1024); err != io.EOF {
		t.Fatalf("%v", err)
	}
}

func TestReadFrameLimitDoesNotCountTheCarriageReturnOfCRLF(t *testing.T) {
	line := strings.Repeat("x", 100)
	frames := readAll(t, line+"\r\n"+line+"y\r\n", 100)
	if len(frames) != 2 || frames[0].tooLong || len(frames[0].data) != 100 || !frames[1].tooLong {
		t.Fatalf("%+v", frames)
	}
}

func TestReadFrameOversizeLineAtEndOfInput(t *testing.T) {
	frames := readAll(t, strings.Repeat("q", 200), 100)
	if len(frames) != 1 || !frames[0].tooLong {
		t.Fatalf("%+v", frames)
	}
}
