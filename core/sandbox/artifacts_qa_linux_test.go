//go:build linux && amd64

package sandbox_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/loveRyujin/lantern/core/sandbox"
)

func artifactQAPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, image.NewGray(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// DecodeConfig alone accepts this header; only decoding the complete image
// rejects the missing IDAT. Its IHDR and IEND CRCs are valid.
func artifactQAHeaderOnly(data []byte) []byte {
	return append(append([]byte{}, data[:33]...), data[len(data)-12:]...)
}

func TestArtifactQAStructuredContent(t *testing.T) {
	valid := artifactQAPNG(t, 2, 3)
	badCRC := append([]byte{}, valid...)
	badCRC[len(badCRC)-1] ^= 1
	cases := []struct {
		name string
		kind sandbox.ArtifactKind
		data []byte
		good bool
	}{
		{"decoded-image", sandbox.KindPNG, valid, true},
		{"header-only", sandbox.KindPNG, artifactQAHeaderOnly(valid), false},
		{"signature-only", sandbox.KindPNG, valid[:8], false},
		{"bad-crc", sandbox.KindPNG, badCRC, false},
		{"trailing-byte", sandbox.KindPNG, append(append([]byte{}, valid...), 0), false},
		{"concatenated-images", sandbox.KindPNG, append(append([]byte{}, valid...), valid...), false},
		{"dimension-limit", sandbox.KindPNG, artifactQAPNG(t, 8192, 1), true},
		{"dimension-over", sandbox.KindPNG, artifactQAPNG(t, 8193, 1), false},
		{"pixel-limit", sandbox.KindPNG, artifactQAPNG(t, 2000, 2000), true},
		{"pixel-over", sandbox.KindPNG, artifactQAPNG(t, 2001, 2000), false},
		{"csv-quoted-newline", sandbox.KindCSV, []byte("name,value\r\n\"逗号,换行\nemoji🙂\",\"\"\"quoted\"\"\"\r\n"), true},
		{"csv-single-column-is-valid", sandbox.KindCSV, []byte("one\ntwo\n"), true},
		{"csv-unclosed-quote", sandbox.KindCSV, []byte("a,b\n\"broken,c\n"), false},
		{"csv-bare-quote", sandbox.KindCSV, []byte("a,b\nx\"y,z\n"), false},
		{"csv-width", sandbox.KindCSV, []byte("a,b\n1\n"), false},
		{"csv-invalid-utf8", sandbox.KindCSV, []byte{'a', '\n', 0xff, '\n'}, false},
		{"csv-nul", sandbox.KindCSV, []byte("a\nx\x00y\n"), false},
		{"json-unicode", sandbox.KindJSON, []byte("{\"text\":\"数据🙂\",\"n\":9223372036854775807}"), true},
		{"json-invalid-utf8", sandbox.KindJSON, []byte{'"', 0xff, '"'}, false},
		{"json-trailing", sandbox.KindJSON, []byte("{} {}"), false},
		{"json-not-finite", sandbox.KindJSON, []byte("{\"value\":NaN}"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := sandbox.ValidateArtifact(sandbox.Artifact{Name: "data." + string(tc.kind), Kind: tc.kind, Data: tc.data}, 0)
			if (err == nil) != tc.good || (err != nil && !errors.Is(err, sandbox.ErrArtifact)) {
				t.Fatalf("content good=%v: %v", tc.good, err)
			}
		})
	}
	// A malicious IHDR can request huge allocation despite tiny compressed input.
	// Its checksum is corrected so validation cannot rely on a bad CRC.
	hugeHeader := append([]byte{}, valid...)
	binary.BigEndian.PutUint32(hugeHeader[16:20], 0x7fffffff)
	binary.BigEndian.PutUint32(hugeHeader[29:33], crc32.ChecksumIEEE(hugeHeader[12:29]))
	if err := sandbox.ValidateArtifact(sandbox.Artifact{Name: "huge.png", Kind: sandbox.KindPNG, Data: hugeHeader}, 0); !errors.Is(err, sandbox.ErrArtifact) {
		t.Fatalf("unbounded IHDR accepted: %v", err)
	}
}

func TestArtifactQARealCollectorRejectsInvalidOutputs(t *testing.T) {
	runner, root := blackboxRunner(t)
	validPNG := artifactQAPNG(t, 2, 2)
	cases := []struct {
		name string
		kind string
		data []byte
		code string
	}{
		{"png-header-only", "png", artifactQAHeaderOnly(validPNG), ""},
		{"png-concatenated", "png", append(append([]byte{}, validPNG...), validPNG...), ""},
		{"png-pixels", "png", artifactQAPNG(t, 2001, 2000), ""},
		{"csv-width", "csv", []byte("a,b\n1\n"), ""},
		{"csv-quote", "csv", []byte("a,b\n\"unclosed,x\n"), ""},
		{"csv-utf8", "csv", []byte{'a', '\n', 0xff}, ""},
		{"json-utf8", "json", []byte{'"', 0xff, '"'}, ""},
		{"json-trailing", "json", []byte("{}{}"), ""},
		{"extension-mismatch", "png", []byte("{}"), ""},
		{"symlink-to-readable-input", "json", []byte("{}"), "os.symlink(os.path.join(os.environ['INPUT_DIR'],'payload'),out+'/z.json')"},
		{"hardlinked-output", "json", []byte("{}"), "open(out+'/z.json','wb').write(b'{}')\nos.link(out+'/z.json',out+'/y.json')"},
		{"directory", "json", nil, "os.mkdir(out+'/z.json')"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code := "import os\nout=os.environ['OUTPUT_DIR']\nopen(out+'/a.json','w').write('{\"complete\":true}')\n"
			if tc.code != "" {
				code += tc.code
			} else {
				code += "open(out+'/z." + tc.kind + "','wb').write(open(os.path.join(os.environ['INPUT_DIR'],'payload'),'rb').read())"
			}
			code += "\nprint('candidate-produced',flush=True)\n"
			result, err := blackboxRun(t, runner, sandbox.Spec{Code: code, Inputs: map[string][]byte{"payload": tc.data}})
			if !errors.Is(err, sandbox.ErrFailed) || !strings.Contains(result.Stdout, "candidate-produced") || len(result.Artifacts) != 0 {
				t.Fatalf("must reject completed output without partial artifacts: result=%+v error=%v", result, err)
			}
			blackboxReaped(t, result.Usage.PythonPID)
			blackboxClean(t, root)
		})
	}
	// The same runner must recover, and returned byte copies outlive cleanup.
	result, err := blackboxRun(t, runner, sandbox.Spec{Code: "import os\nopen(os.environ['OUTPUT_DIR']+'/ok.csv','w').write('v\\n42\\n')"})
	if err != nil || len(result.Artifacts) != 1 || string(result.Artifacts[0].Data) != "v\n42\n" {
		t.Fatalf("recovery: result=%+v error=%v", result, err)
	}
	blackboxClean(t, root)
}
