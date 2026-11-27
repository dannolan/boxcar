package nuptest

import (
	"bytes"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// Fixture is one benchmark scenario: archives laid out the way a KaiDump
// dump stores them (raw, one extent per file, no deduplication), generated
// deterministically so every run and every machine sees the same bytes.
type Fixture struct {
	Name string
	Desc string
	// Archives maps a file name to a function that builds its packages.
	// Sources are single-use readers, so Build must be called once per
	// write.
	Archives []FixtureArchive
}

// FixtureArchive is one archive of a fixture.
type FixtureArchive struct {
	Name  string
	Build func() []Package
}

// Fixtures returns the benchmark scenarios. scale multiplies every size and
// file count (1 is the documented "medium" set, about 2.6 GB in total;
// 0.01 is enough for tests).
func Fixtures(scale float64) []Fixture {
	n := func(v int) int {
		r := int(float64(v) * scale)
		if r < 1 {
			r = 1
		}
		return r
	}
	sz := func(v int64) int64 {
		r := int64(float64(v) * scale)
		if r < 1 {
			r = 1
		}
		return r
	}
	const tid = 0x0100000000010000
	return []Fixture{
		{
			Name: "large-raw",
			Desc: "two large RAW files (incompressible movie, compressible pack) plus small assets",
			Archives: []FixtureArchive{{Name: "large-raw.nup", Build: func() []Package {
				files := []File{
					{Path: "romfs/movie/intro.bin", Size: sz(384 << 20), Source: Content(1, sz(384<<20), 1)},
					{Path: "romfs/world.pak", Size: sz(320 << 20), Source: Content(2, sz(320<<20), 0.4)},
				}
				for i := 0; i < n(100); i++ {
					files = append(files, File{Path: fmt.Sprintf("romfs/ui/%03d.bin", i), Size: 256 << 10, Source: Content(int64(100+i), 256<<10, 0.3)})
				}
				return []Package{{TitleID: tid, Kind: 0, Files: withNPDM(files)}}
			}}},
		},
		{
			Name: "incompressible",
			Desc: "random assets and assets that are already zstd frames",
			Archives: []FixtureArchive{{Name: "incompressible.nup", Build: func() []Package {
				var files []File
				for i := 0; i < n(1500); i++ {
					s := int64(32<<10) + int64(i*7919)%(480<<10)
					files = append(files, File{Path: fmt.Sprintf("romfs/tex/%04d.bin", i), Size: s, Source: Content(int64(1000+i), s, 1)})
				}
				for i := 0; i < n(500); i++ {
					data := readAll(Content(int64(5000+i), int64(64<<10)+int64(i*104729)%(192<<10), 0))
					files = append(files, File{Path: fmt.Sprintf("romfs/zs/%04d.bin", i), Data: data, Extent: Frame(data, zstd.SpeedBestCompression), Codec: 1})
				}
				return []Package{{TitleID: tid, Kind: 0, Files: withNPDM(files)}}
			}}},
		},
		{
			Name: "many-small",
			Desc: "tens of thousands of small compressible files in hundreds of folders",
			Archives: []FixtureArchive{{Name: "many-small.nup", Build: func() []Package {
				var files []File
				for i := 0; i < n(40000); i++ {
					s := int64(512 + (i*2654435761)%(7<<10+512))
					files = append(files, File{Path: fmt.Sprintf("romfs/d%03d/f%05d.dat", i%400, i), Size: s, Source: Content(int64(20000+i), s, 0.1)})
				}
				return []Package{{TitleID: tid, Kind: 0, Files: withNPDM(files)}}
			}}},
		},
		{
			Name: "duplicates",
			Desc: "one content stored raw, as a fast frame and as a strong frame across base, update and DLC",
			Archives: []FixtureArchive{{Name: "duplicates.nup", Build: func() []Package {
				var base, upd, dlc []File
				for i := 0; i < n(200); i++ {
					data := readAll(Content(int64(70000+i), int64(64<<10)+int64(i*7907)%(448<<10), 0.05))
					p := fmt.Sprintf("romfs/shared/%03d.bin", i)
					base = append(base, File{Path: p, Data: data})
					upd = append(upd, File{Path: p, Data: data, Extent: Frame(data, zstd.SpeedFastest), Codec: 1})
					dlc = append(dlc, File{Path: p, Data: data, Extent: Frame(data, zstd.SpeedBestCompression), Codec: 1})
				}
				return []Package{
					{TitleID: tid, Kind: 0, Files: withNPDM(base)},
					{TitleID: tid, Version: 65536, Kind: 1, Files: withNPDM(upd)},
					{TitleID: tid | 0x1001, Kind: 2, Files: dlc},
				}
			}}},
		},
		{
			Name:     "mixed",
			Desc:     "base, update (replacing a fifth of the files) and DLC with mixed content",
			Archives: []FixtureArchive{{Name: "mixed.nup", Build: func() []Package { return mixedSet(tid, 0, scale) }}},
		},
		{
			Name: "multi",
			Desc: "a folder of four smaller mixed archives (single run over many archives)",
			Archives: []FixtureArchive{
				{Name: "multi-a.nup", Build: func() []Package { return mixedSet(tid+0x2000, 1, scale/3) }},
				{Name: "multi-b.nup", Build: func() []Package { return mixedSet(tid+0x4000, 2, scale/3) }},
				{Name: "multi-c.nup", Build: func() []Package { return mixedSet(tid+0x6000, 3, scale/3) }},
				{Name: "multi-d.nup", Build: func() []Package { return mixedSet(tid+0x8000, 4, scale/3) }},
			},
		},
	}
}

func mixedSet(tid uint64, seed int64, scale float64) []Package {
	count := int(800 * scale)
	if count < 5 {
		count = 5
	}
	size := func(i int) int64 { return int64(4<<10) + int64(i*48271)%(int64(740<<10)) }
	s0 := seed * 1_000_000
	var base, upd, dlc []File
	for i := 0; i < count; i++ {
		base = append(base, File{Path: fmt.Sprintf("romfs/m%02d/%04d.bin", i%32, i), Size: size(i), Source: Content(s0+int64(i), size(i), 0.35)})
		if i%5 == 0 {
			upd = append(upd, File{Path: fmt.Sprintf("romfs/m%02d/%04d.bin", i%32, i), Size: size(i), Source: Content(s0+500_000+int64(i), size(i), 0.35)})
		}
	}
	for i := 0; i < count/10; i++ {
		dlc = append(dlc, File{Path: fmt.Sprintf("romfs/dlc/%03d.bin", i), Size: size(i * 3), Source: Content(s0+800_000+int64(i), size(i*3), 0.2)})
	}
	return []Package{
		{TitleID: tid, Kind: 0, Files: withNPDM(base)},
		{TitleID: tid, Version: 65536, Kind: 1, Files: withNPDM(upd)},
		{TitleID: tid | 0x1001, Kind: 2, Files: dlc},
	}
}

// withNPDM prepends a minimal exefs (main.npdm + main) to a package.
func withNPDM(files []File) []File {
	npdm := make([]byte, 0x100)
	copy(npdm, "META")
	main := append([]byte("NSO0"), bytes.Repeat([]byte{1}, 4096)...)
	return append([]File{{Path: "exefs/main.npdm", Data: npdm}, {Path: "exefs/main", Data: main}}, files...)
}

// Frame encodes data as one zstd frame with its content size.
func Frame(data []byte, level zstd.EncoderLevel) []byte {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(level), zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(8<<20))
	if err != nil {
		panic(err)
	}
	var b bytes.Buffer
	enc.ResetContentSize(&b, int64(len(data)))
	if _, err := enc.Write(data); err != nil {
		panic(err)
	}
	if err := enc.Close(); err != nil {
		panic(err)
	}
	return b.Bytes()
}

func readAll(r io.Reader) []byte {
	b, err := io.ReadAll(r)
	if err != nil {
		panic(err)
	}
	return b
}

// Content returns size deterministic bytes for seed. Each 64 KiB block is
// random with probability random (0..1) and otherwise structured, text-like
// data that zstd shrinks to roughly a third, which is how game data mixes
// already-compressed media with tables, scripts and models.
func Content(seed, size int64, random float64) io.Reader {
	return &content{x: uint64(seed)*0x9E3779B97F4A7C15 + 1, left: size, random: random}
}

type content struct {
	x      uint64
	left   int64
	random float64
	block  []byte
	pos    int
}

var words = [][]byte{
	[]byte("vertex "), []byte("normal "), []byte("index="), []byte("texture:"), []byte("<node id="),
	[]byte("</node>\n"), []byte("flag "), []byte("0x00000000 "), []byte("mushroom "), []byte("kingdom "),
	[]byte("level_"), []byte("save "), []byte("anim.bfres "), []byte("\x00\x00\x00\x01"), []byte("\xff\xff"),
	[]byte("Lorem "), []byte("ipsum "), []byte("dolor "), []byte(" = "), []byte(";\n"),
}

func (c *content) next() uint64 {
	c.x ^= c.x << 13
	c.x ^= c.x >> 7
	c.x ^= c.x << 17
	return c.x
}

func (c *content) fill() {
	const bs = 64 << 10
	if c.block == nil {
		c.block = make([]byte, bs)
	}
	b := c.block[:0]
	if float64(c.next()%1000) < c.random*1000 {
		for len(b)+8 <= bs {
			v := c.next()
			b = append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24), byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56))
		}
		for len(b) < bs {
			b = append(b, byte(c.next()))
		}
	} else {
		for len(b) < bs {
			v := c.next()
			w := words[v%uint64(len(words))]
			if len(b)+len(w)+3 > bs {
				for len(b) < bs {
					b = append(b, ' ')
				}
				break
			}
			b = append(b, w...)
			n := (v >> 8) % 1000
			b = append(b, byte('0'+n/100), byte('0'+n/10%10), byte('0'+n%10))
		}
	}
	c.block, c.pos = b, 0
}

func (c *content) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, io.EOF
	}
	if c.block == nil || c.pos == len(c.block) {
		c.fill()
	}
	k := copy(p, c.block[c.pos:])
	if int64(k) > c.left {
		k = int(c.left)
	}
	c.pos += k
	c.left -= int64(k)
	return k, nil
}
