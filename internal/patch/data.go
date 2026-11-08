package patch

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// Embedded payloads and masks. The extraction step has already b85/zlib-decoded
// the Python `image`/`mask` fields into raw .bin files, so no b85 decoder is
// required anywhere in this package.
//
//go:embed data/manifest.json data/*.bin
var dataFS embed.FS

// payloadData is the decoded native probe plus its metadata.
type payloadData struct {
	Image    []byte
	Entry    int
	Unwind   [][3]int
	SizeSlot int
	ReadSlot int
	SHA256   string
}

// readerData is the metadata-reader fingerprint for a loader pattern.
type readerData struct {
	Anchor           []byte
	NormalizedSHA256 string
	SizeDispOffset   int
	ReadDispOffset   int
	Mask             []byte
}

// patternData is a loader-code fingerprint used by profile_for.
type patternData struct {
	Anchor                  []byte
	NormalizedSHA256        string
	Name                    string
	ABI                     string
	Kind                    string
	HookDelta               int
	Expected                string
	MetadataCallDelta       int
	PreserveDuplicateUnwind bool
	Mask                    []byte
	Reader                  readerData
}

var (
	payloads       = map[string]*payloadData{}
	payloadKeys    []string // manifest order (sorted for determinism)
	legacyPayloads = map[string]*payloadData{}
	patterns       []patternData
	profiles       = map[string]Profile{}
)

// JSON wire shapes mirroring data/manifest.json.
type payloadJSON struct {
	File     string   `json:"file"`
	Entry    int      `json:"entry"`
	Unwind   [][3]int `json:"unwind"`
	SizeSlot int      `json:"size_slot"`
	ReadSlot int      `json:"read_slot"`
	SHA256   string   `json:"sha256"`
}

type legacyJSON struct {
	File   string   `json:"file"`
	Entry  int      `json:"entry"`
	Unwind [][3]int `json:"unwind"`
	SHA256 string   `json:"sha256"`
}

type readerJSON struct {
	Anchor           string `json:"anchor"`
	NormalizedSHA256 string `json:"normalized_sha256"`
	SizeDispOffset   int    `json:"size_disp_offset"`
	ReadDispOffset   int    `json:"read_disp_offset"`
	MaskFile         string `json:"mask_file"`
}

type patternJSON struct {
	Anchor                  string     `json:"anchor"`
	NormalizedSHA256        string     `json:"normalized_sha256"`
	Name                    string     `json:"name"`
	ABI                     string     `json:"abi"`
	Kind                    string     `json:"kind"`
	HookDelta               int        `json:"hook_delta"`
	Expected                string     `json:"expected"`
	MetadataCallDelta       int        `json:"metadata_call_delta"`
	PreserveDuplicateUnwind bool       `json:"preserve_duplicate_unwind"`
	MaskFile                string     `json:"mask_file"`
	Reader                  readerJSON `json:"reader"`
}

type profileJSON struct {
	Name                    string `json:"name"`
	File                    string `json:"file"`
	ABI                     string `json:"abi"`
	Hook                    int    `json:"hook"`
	Expected                string `json:"expected"`
	Kind                    string `json:"kind"`
	PreserveDuplicateUnwind bool   `json:"preserve_duplicate_unwind"`
}

type manifestJSON struct {
	Payloads map[string]payloadJSON `json:"payloads"`
	Legacy   map[string]legacyJSON  `json:"legacy"`
	Patterns []patternJSON          `json:"patterns"`
	Profiles map[string]profileJSON `json:"profiles"`
}

func init() { loadData() }

func loadData() {
	raw, err := dataFS.ReadFile("data/manifest.json")
	if err != nil {
		panic(fmt.Sprintf("patch: embed manifest: %v", err))
	}
	var m manifestJSON
	if err := json.Unmarshal(raw, &m); err != nil {
		panic(fmt.Sprintf("patch: manifest: %v", err))
	}
	readBin := func(name string) []byte {
		b, err := dataFS.ReadFile("data/" + name)
		if err != nil {
			panic(fmt.Sprintf("patch: embed %s: %v", name, err))
		}
		return b
	}
	verifySHA := func(data []byte, want, label string) {
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		if got != want {
			panic(fmt.Sprintf("patch: %s integrity check failed: got %s want %s", label, got, want))
		}
	}

	pkeys := make([]string, 0, len(m.Payloads))
	for k := range m.Payloads {
		pkeys = append(pkeys, k)
	}
	sort.Strings(pkeys)
	for _, k := range pkeys {
		spec := m.Payloads[k]
		img := readBin(spec.File)
		verifySHA(img, spec.SHA256, "payload "+k)
		payloads[k] = &payloadData{
			Image:    img,
			Entry:    spec.Entry,
			Unwind:   spec.Unwind,
			SizeSlot: spec.SizeSlot,
			ReadSlot: spec.ReadSlot,
			SHA256:   spec.SHA256,
		}
		payloadKeys = append(payloadKeys, k)
	}

	lkeys := make([]string, 0, len(m.Legacy))
	for k := range m.Legacy {
		lkeys = append(lkeys, k)
	}
	sort.Strings(lkeys)
	for _, k := range lkeys {
		spec := m.Legacy[k]
		img := readBin(spec.File)
		verifySHA(img, spec.SHA256, "legacy payload "+k)
		legacyPayloads[k] = &payloadData{
			Image:  img,
			Entry:  spec.Entry,
			Unwind: spec.Unwind,
			SHA256: spec.SHA256,
		}
	}

	for _, pj := range m.Patterns {
		patterns = append(patterns, patternData{
			Anchor:                  mustHex(pj.Anchor),
			NormalizedSHA256:        pj.NormalizedSHA256,
			Name:                    pj.Name,
			ABI:                     pj.ABI,
			Kind:                    pj.Kind,
			HookDelta:               pj.HookDelta,
			Expected:                pj.Expected,
			MetadataCallDelta:       pj.MetadataCallDelta,
			PreserveDuplicateUnwind: pj.PreserveDuplicateUnwind,
			Mask:                    readBin(pj.MaskFile),
			Reader: readerData{
				Anchor:           mustHex(pj.Reader.Anchor),
				NormalizedSHA256: pj.Reader.NormalizedSHA256,
				SizeDispOffset:   pj.Reader.SizeDispOffset,
				ReadDispOffset:   pj.Reader.ReadDispOffset,
				Mask:             readBin(pj.Reader.MaskFile),
			},
		})
	}

	for k, pj := range m.Profiles {
		profiles[k] = Profile{
			Name:                    pj.Name,
			File:                    pj.File,
			ABI:                     pj.ABI,
			Hook:                    pj.Hook,
			Expected:                pj.Expected,
			Kind:                    pj.Kind,
			PreserveDuplicateUnwind: pj.PreserveDuplicateUnwind,
		}
	}
}
