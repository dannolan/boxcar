package nup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dannolan/boxcar/internal/binfmt"
	"github.com/dannolan/boxcar/internal/diskfree"
	"github.com/klauspost/compress/zstd"
)

// Estimation budget: archives whose compression candidates fit in
// sampleBudget bytes are trial-compressed in full; larger ones are sampled in
// sampleWindow pieces spread evenly over the candidates' bytes. Windows start
// on the 128 KiB grid on which a streamed file is cut into zstd blocks, so a
// sample compresses exactly the blocks the real run will (minus the history
// before them): unaligned windows straddle different block boundaries and
// report savings the file can never get.
const (
	sampleBudget = 64 << 20
	sampleWindow = 256 << 10
	zstdBlock    = 128 << 10
)

var emptyDigest = sha256.Sum256(nil)

// RepackOptions controls how Optimize stores each file.
type RepackOptions struct {
	// Level is the zstd level for files that are (re)compressed: 1, 3, 9 or
	// 19 (see Presets); 0 means 3.
	Level int
	// LargeRaw, LargeFileThreshold and MinSavings are the PackOptions
	// storage policy.
	LargeRaw           bool
	LargeFileThreshold int64
	MinSavings         float64
	// Recompress re-encodes existing zstd frames at Level and keeps the
	// smaller frame; otherwise frames that satisfy the policy are reused.
	Recompress bool
	Progress   binfmt.Progress
}

// OptimizeOptions controls Optimize.
type OptimizeOptions struct {
	RepackOptions
	// Output is the new archive's path; it must not exist. Ignored when
	// InPlace is set.
	Output string
	// InPlace replaces the source archive once the rewrite is verified.
	InPlace bool
	// DryRun plans and estimates without writing anything.
	DryRun bool
	// Force rewrites archives the plan considers already optimized.
	Force bool
	// MinGain is the estimated saving, in percent of the archive size,
	// below which an archive counts as already optimized.
	MinGain float64
	// SplitSize writes the output as numbered parts of this many bytes
	// ("Game.nup.001", ...; nx2-flow's convention) when it is larger than
	// one part. 0 splits at FAT32PartSize only when the output folder is
	// on a FAT volume, where a file of 4 GiB cannot exist; a negative value
	// never splits. In-place rewrites are never split.
	SplitSize int64
	// Measure turns an uncertain "already optimized" verdict (sampled
	// estimate, or recompression whose gain is unknown) into a real rewrite
	// whose measured size decides: the result is kept only if it saves at
	// least MinGain (at least one byte). Used for archives the user named
	// explicitly, so sampling cannot cause a false skip.
	Measure bool
	// Transform optionally replaces the package set before it is written:
	// an opt-in, content-altering step such as snapshot consolidation.
	Transform func(ctx context.Context, packages []Package) ([]Package, error)
}

// Plan is what optimizing an archive will do, worked out before anything is
// written. Counts are per distinct file content; byte counts are logical
// (uncompressed) sizes unless noted.
type Plan struct {
	ArchiveSize    int64 `json:"archive_size"`
	Packages       int   `json:"packages"`
	Files          int   `json:"files"`
	ContentBytes   int64 `json:"content_bytes"`
	UniqueContents int   `json:"unique_contents"`
	DuplicateFiles int   `json:"duplicate_files"`
	// RedundantBytes are stored bytes of extents whose content another
	// extent already holds; deduplication drops them.
	RedundantBytes int64 `json:"redundant_bytes"`
	ReuseFrames    int   `json:"reuse_frames"`
	KeepRaw        int   `json:"keep_raw"`
	Compress       int   `json:"compress"`
	CompressBytes  int64 `json:"compress_bytes"`
	Reencode       int   `json:"reencode"`
	Decode         int   `json:"decode"`
	DecodeBytes    int64 `json:"decode_bytes"`
	// PolicyFixes counts zstd extents that break the storage policy: above
	// the large-file threshold, or saving less than the minimum.
	PolicyFixes int `json:"policy_fixes"`
	// BetterExtents counts contents built from a later duplicate extent
	// because it is smaller or satisfies the policy where the first does not.
	BetterExtents int `json:"better_extents"`
	// Deviations lists what the source does that the NUP spec or the
	// reference reader does not allow (see Archive.Deviations); a rewrite
	// normalizes them.
	Deviations []string `json:"deviations,omitempty"`
	// EstimatedSize is the predicted output size. Exact reports whether
	// every file planned for compression was trial-compressed in full
	// rather than sampled. ExactSavings is the part of the savings known
	// without any compression estimate (dropped duplicate extents, frames
	// stored raw by policy); Basis says how far the total can be trusted:
	// "exact", "estimated" (sampled compression) or "lower_bound" (files to
	// re-encode are counted at their current size; their gain is unknown).
	ExactSavings     int64  `json:"exact_savings"`
	Basis            string `json:"savings_basis"`
	EstimatedSize    int64  `json:"estimated_size"`
	EstimatedSavings int64  `json:"estimated_savings"`
	Exact            bool   `json:"exact"`
	SampledBytes     int64  `json:"sampled_bytes"`
	// WorstCaseSize bounds the output size while it is written.
	WorstCaseSize int64 `json:"worst_case_size"`
}

// RepackStats reports how each distinct content was stored. The storage
// counts add up to UniqueContents.
type RepackStats struct {
	UniqueContents int `json:"unique_contents"`
	DuplicateFiles int `json:"duplicate_files"`
	ReusedFrames   int `json:"reused_frames"`
	KeptRaw        int `json:"kept_raw"`
	Compressed     int `json:"compressed"`
	// Incompressible counts raw files kept raw because zstd did not save
	// the minimum.
	Incompressible int `json:"incompressible"`
	Recompressed   int `json:"recompressed"`
	Decompressed   int `json:"decompressed"`
}

// OptimizeResult reports one Optimize call. Status is "optimized" (written,
// verified and committed, and smaller), "normalized" (written, verified and
// committed to fix spec deviations, the storage policy or the package set,
// but not smaller), "skipped" (left alone, see Reason: nothing replaced or
// published, although a measured skip wrote and then removed a temporary
// rewrite) or "planned" (dry run, with Action "optimize", "skip" or
// "measure" saying what a real run would do; "measure" means a temporary
// full rewrite decides). Callers add "failed". Verified is true for every
// committed result.
type OptimizeResult struct {
	Input               string       `json:"input"`
	Output              string       `json:"output,omitempty"`
	Parts               []string     `json:"parts,omitempty"` // the output's parts, when it was written in parts (Output is the first)
	Status              string       `json:"status"`
	Action              string       `json:"action,omitempty"`
	Reason              string       `json:"reason,omitempty"`
	InPlace             bool         `json:"in_place"`
	SizeBefore          int64        `json:"size_before"`
	SizeAfter           int64        `json:"size_after"`
	SavedBytes          int64        `json:"saved_bytes"`
	LogicalSHA256       string       `json:"logical_sha256"`
	OutputLogicalSHA256 string       `json:"output_logical_sha256,omitempty"`
	ContentChanged      bool         `json:"content_changed"`
	Verified            bool         `json:"verified"`
	Plan                Plan         `json:"plan"`
	Stats               *RepackStats `json:"stats,omitempty"`
}

type action uint8

const (
	actReuseFrame action = iota // copy the stored zstd frame verbatim
	actKeepRaw                  // copy the stored raw bytes (large-file rule)
	actCompress                 // compress raw bytes; raw when it saves too little
	actReencode                 // decode and re-encode; old frame or raw as fallback
	actDecode                   // store a zstd extent raw (policy)
)

type nodePlan struct {
	own    int    // this file's input record
	rec    int    // record whose extent supplies the bytes (writer nodes: the best extent; else own)
	writer int    // node that stores this content; -1 for empty files
	act    action // how a writer node stores it
	oldOK  bool   // the supplying frame satisfies the policy
	verify bool   // also hash this node's own input extent (it is dropped)
}

type repacking struct {
	ctx        context.Context
	a          *Archive
	packages   []Package
	level      int
	policy     codecPolicy
	recompress bool
	l          *layout
	nodes      []nodePlan
	estimate   map[int]int64
	expected   map[int]bool // estimates that already average the policy's raw-or-frame choice
	plan       Plan
	progress   func(label string, pct float64)
}

// Optimize rewrites the NUP at path without extracting it: existing frames
// are reused, raw files are compressed with the chosen level, identical
// contents are stored once and the storage policy is applied. The result is
// written to a temporary file, fully verified, compared with the source's
// logical content (or the Transform's, when one is given) and only then
// committed to opts.Output, or swapped over the source with opts.InPlace.
// Failures and cancellation leave the source untouched.
func Optimize(ctx context.Context, path string, opts OptimizeOptions) (res OptimizeResult, err error) {
	defer binfmt.Recover(&err)
	report := func(label string, pct float64) {
		if opts.Progress != nil {
			opts.Progress(label, pct)
		}
	}
	level := opts.Level
	if level == 0 {
		level = 3
	}
	binfmt.Require(validOptimizeLevel(level), "Choose compression level 1, 3, 9 or 19.")
	policy := newCodecPolicy(opts.LargeRaw, opts.LargeFileThreshold, opts.MinSavings)
	binfmt.Require(opts.MinGain >= 0 && opts.MinGain < 100, "Minimum gain must be at least 0 and below 100 percent.")

	abs, aerr := filepath.Abs(path)
	if aerr != nil {
		return res, aerr
	}
	res = OptimizeResult{Input: abs, InPlace: opts.InPlace}
	binfmt.Require(!isLinked(abs), "Linked input files are not supported.")
	inParts, _, perr := FindParts(abs)
	if perr != nil {
		return res, perr
	}
	for _, p := range inParts {
		binfmt.Require(!isLinked(p), "Linked input files are not supported.")
	}
	binfmt.Require(!opts.InPlace || len(inParts) == 1 && inParts[0] == abs,
		"An archive in parts cannot be replaced in place; write it to --output.")
	if opts.InPlace && !opts.DryRun {
		unlock, lerr := lockSource(abs)
		if lerr != nil {
			return res, lerr
		}
		defer unlock()
	}

	report("Analyzing NUP", 0)
	a, oerr := openParts(ctx, abs, inParts)
	if oerr != nil {
		return res, oerr
	}
	closed := false
	closeSource := func() {
		if !closed {
			a.Close()
			closed = true
		}
	}
	defer closeSource()
	binfmt.Require(a.headerSize == 128 && a.headerVersion == 1,
		"Unsupported NUP header extension (size %d, version %d); refusing to rewrite it.", a.headerSize, a.headerVersion)

	source := a.Contents()
	res.LogicalSHA256 = LogicalDigest(source)
	res.SizeBefore = a.stamp.size
	packages := a.Packages()
	want := source
	if opts.Transform != nil {
		packages, err = opts.Transform(ctx, packages)
		if err != nil {
			return res, err
		}
		validatePackages(packages)
		if intended := contentsOf(packages); CompareContents(source, intended) != nil {
			want = intended
			res.ContentChanged = true
		}
	}

	r := planRepack(ctx, a, packages, level, policy, opts.Recompress, func(label string, pct float64) {
		report(label, 2*pct/100)
	})
	res.Plan = r.plan
	reason, worthwhile := r.plan.verdict(opts.MinGain, res.ContentChanged, opts.Recompress)
	res.Reason = reason
	// An estimate can be wrong both ways, so when a skip verdict rests on a
	// sampled estimate and Measure is set, a real rewrite measures it (the
	// commit rule below keeps it only if it saves the minimum gain).
	need := max64(int64(math.Ceil(float64(r.plan.ArchiveSize)*opts.MinGain/100)), 1)
	measuring := !worthwhile && !opts.Force && opts.Measure && r.plan.Basis != "exact"
	// Re-encoding existing frames has no estimate (a frame does not record
	// its level), so when that is the only reason to rewrite, the real size
	// decides too: a second --recompress run commits nothing.
	recompressOnly := worthwhile && opts.Recompress && !opts.Force && !res.ContentChanged &&
		r.plan.PolicyFixes == 0 && len(r.plan.Deviations) == 0 && r.plan.EstimatedSavings < need
	if opts.DryRun {
		res.Status = "planned"
		res.Action = "skip"
		if worthwhile || opts.Force {
			res.Action = "optimize"
		}
		if measuring || recompressOnly {
			res.Action = "measure"
			res.Reason += fmt.Sprintf("; a real run measures this with a full temporary rewrite and replaces or publishes nothing unless it saves at least %d bytes (%g%%)", need, opts.MinGain)
		}
		res.SizeAfter = r.plan.EstimatedSize
		res.SavedBytes = r.plan.EstimatedSavings
		report("NUP optimization planned", 100)
		return res, nil
	}
	if !worthwhile && !opts.Force && !measuring {
		return skipped(res, reason, report), nil
	}
	measuring = measuring || recompressOnly

	final := abs
	mode := os.FileMode(0o666)
	if opts.InPlace {
		st, serr := os.Stat(abs)
		if serr != nil {
			return res, serr
		}
		if err := replaceable(st); err != nil {
			return res, err
		}
		mode = st.Mode().Perm() // the copy must never be readable by more users than the original
	} else {
		binfmt.Require(opts.Output != "", "Choose an output path or in-place mode.")
		final, aerr = filepath.Abs(opts.Output)
		if aerr != nil {
			return res, aerr
		}
		binfmt.Require(strings.ToLower(filepath.Ext(final)) == ".nup", "Output must end in .nup.")
		for _, p := range inParts {
			binfmt.Require(!sameFile(final, p), "Output is the input archive; use in-place mode to replace it.")
		}
		if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
			return res, err
		}
		if err := partsCollide(final); err != nil {
			return res, err
		}
	}
	split := opts.SplitSize
	if opts.InPlace || split < 0 {
		split = 0
	} else if split == 0 {
		if fat, _ := diskfree.IsFAT(filepath.Dir(final)); fat {
			split = FAT32PartSize
		}
	}
	// A rewrite that is likely to be discarded is measured first without
	// writing: it runs against a writer that only counts, and only one that
	// reaches the minimum gain is then written for real (a second,
	// identical pass) and verified. Likely to be discarded: the estimate
	// said skip (measuring), or it is sampled and under 1% of the archive,
	// where sampling error is of the order of the savings. Required
	// rewrites (fixes) and --force are written directly.
	fixesPlanned := res.ContentChanged || r.plan.PolicyFixes > 0 || len(r.plan.Deviations) > 0
	onePercent := max64(int64(math.Ceil(float64(r.plan.ArchiveSize)/100)), 1)
	countFirst := measuring || (!opts.Force && !fixesPlanned && r.plan.Basis != "exact" && r.plan.EstimatedSavings < onePercent)
	if countFirst {
		cf := &countingFile{}
		if _, werr := r.write(cf, func(done, total int64) {
			report("Measuring NUP", 2+78*float64(done)/float64(max64(total, 1)))
		}); werr != nil {
			return res, werr
		}
		if err := a.unchanged(); err != nil {
			return res, err
		}
		if saved := res.SizeBefore - cf.size; saved < need {
			res.SizeAfter, res.SavedBytes = cf.size, saved
			reason := fmt.Sprintf("already optimized: a full rewrite (counted, not written) would save %d bytes (%.2f%%), less than the minimum gain of %d bytes (%g%%); nothing was written",
				saved, 100*float64(saved)/float64(max64(res.SizeBefore, 1)), need, opts.MinGain)
			return skipped(res, reason, report), nil
		}
	}
	tag := randomHex()
	temp := final + "." + tag + ".partial"
	var out outFile
	var parts *partWriter
	written := func() int64 {
		if st, err := os.Stat(temp); err == nil {
			return st.Size()
		}
		return 0
	}
	if split > 0 {
		pw, perr := newPartWriter(final, tag, split, mode)
		if perr != nil {
			return res, perr
		}
		defer pw.removeTemps()
		out, parts, written = pw, pw, pw.written
	} else {
		f, cerr := os.OpenFile(temp, os.O_RDWR|os.O_CREATE|os.O_EXCL, mode)
		if cerr != nil {
			return res, cerr
		}
		defer func() {
			var keep *keepFilesError
			if !errors.As(err, &keep) {
				_ = os.Remove(temp)
			}
		}()
		out = f
	}
	release, ferr := diskfree.ReserveFunc(filepath.Dir(final), written, r.plan.WorstCaseSize)
	if ferr != nil {
		out.Close()
		return res, ferr
	}
	defer release()
	stats, werr := r.write(out, func(done, total int64) {
		report("Optimizing NUP files", 2+78*float64(done)/float64(max64(total, 1)))
	})
	if cerr := out.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return res, werr
	}
	r = nil // the layout and plan grow with the file count; free them before verifying
	if err := a.unchanged(); err != nil {
		return res, err
	}

	report("Verifying optimized NUP", 80)
	temps := []string{temp}
	if parts != nil {
		temps = parts.temps()
	}
	b, verr := openParts(ctx, temp, temps)
	if verr != nil {
		return res, verr
	}
	_, verr = b.Verify(ctx, func(_ string, v float64) {
		report("Verifying optimized NUP", 80+v*0.19)
	})
	got := b.Contents()
	b.Close()
	if verr != nil {
		return res, verr
	}
	if err := CompareContents(want, got); err != nil {
		return res, err
	}
	res.Verified = true
	res.OutputLogicalSHA256 = LogicalDigest(got)
	res.Stats = &stats
	res.SizeAfter = 0
	for _, p := range temps {
		st, serr := os.Stat(p)
		if serr != nil {
			return res, serr
		}
		res.SizeAfter += st.Size()
	}
	res.SavedBytes = res.SizeBefore - res.SizeAfter

	binfmt.Check(ctx)
	fixes := res.ContentChanged || res.Plan.PolicyFixes > 0 || len(res.Plan.Deviations) > 0
	// An estimate can be wrong both ways (sampled windows may compress
	// although their whole files do not), so unless the rewrite is
	// required (a fix) or forced, the measured size decides: a rewrite that
	// does not save the minimum gain is discarded. This keeps repeated runs
	// from writing same-size copies (idempotence).
	if !fixes && !opts.Force {
		need := max64(int64(math.Ceil(float64(res.SizeBefore)*opts.MinGain/100)), 1)
		if res.SavedBytes < need {
			reason := fmt.Sprintf("already optimized: a full rewrite saved %d bytes (%.2f%%), less than the minimum gain of %d bytes (%g%%); the temporary rewrite was discarded and nothing was replaced or published",
				res.SavedBytes, 100*float64(res.SavedBytes)/float64(max64(res.SizeBefore, 1)), need, opts.MinGain)
			return skipped(res, reason, report), nil
		}
		if measuring && !recompressOnly {
			res.Reason = fmt.Sprintf("measured: a full rewrite saved %.2f%% although the %s estimate was below the minimum gain",
				100*float64(res.SavedBytes)/float64(max64(res.SizeBefore, 1)), r0basis(res.Plan))
		}
		if recompressOnly {
			res.Reason = fmt.Sprintf("measured: recompressing saved %.2f%%", 100*float64(res.SavedBytes)/float64(max64(res.SizeBefore, 1)))
		}
	}
	if opts.InPlace {
		if res.SizeAfter >= res.SizeBefore && !fixes && !opts.Force {
			reason := fmt.Sprintf("no savings after optimization (the rewrite was %d bytes); the original was kept", res.SizeAfter)
			return skipped(res, reason, report), nil
		}
		closeSource()
		if err := replaceSource(abs, temp, a.stamp); err != nil {
			return res, err
		}
	} else if parts != nil {
		finals := parts.finals()
		if len(finals) > 1 {
			if err := partsCollide(final); err != nil {
				return res, err
			}
		}
		if err := publishParts(temps, finals); err != nil {
			return res, err
		}
		final = finals[0]
		if len(finals) > 1 {
			res.Parts = finals
		}
	} else if err := commitNew(temp, final); err != nil {
		return res, err
	}
	syncDir(filepath.Dir(final))
	res.Output = final
	res.Status = "optimized"
	if res.SavedBytes <= 0 && fixes {
		res.Status = "normalized"
	}
	report("NUP optimized", 100)
	return res, nil
}

func r0basis(p Plan) string {
	if p.Basis == "lower_bound" {
		return "lower-bound"
	}
	return p.Basis
}

// skipped reports an archive that was left as it is.
func skipped(res OptimizeResult, reason string, report func(string, float64)) OptimizeResult {
	res.Status = "skipped"
	res.Reason = reason
	res.SizeAfter = res.SizeBefore
	res.SavedBytes = 0
	res.Verified = false
	res.OutputLogicalSHA256 = ""
	res.Stats = nil
	report("NUP already optimized", 100)
	return res
}

// replaceSource swaps the verified temp file over the source archive after
// checking the source is still the very file that was read. The check and
// the rename are separate steps; lockSource keeps other Boxcar runs out of
// that window, but a program that ignores the lock and rewrites the file in
// that instant is not detected.
func replaceSource(path, temp string, stamp archiveStamp) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || isLinked(path) || !os.SameFile(st, stamp.info) ||
		st.Size() != stamp.size || st.ModTime().UnixNano() != stamp.mtime {
		return errors.New("NUP changed while it was being optimized; the original was kept.")
	}
	if err := prepareReplacement(temp, path, st); err != nil {
		return err
	}
	if st2, err := os.Lstat(path); err != nil || !os.SameFile(st2, st) ||
		st2.Size() != stamp.size || st2.ModTime().UnixNano() != stamp.mtime {
		return errors.New("NUP changed while it was being optimized; the original was kept.")
	}
	return replaceFile(temp, path)
}

func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(sa, sb)
}

// verdict explains whether optimizing is worthwhile and why.
func (p Plan) verdict(minGain float64, changed, recompress bool) (string, bool) {
	switch {
	case changed:
		return "the requested consolidation changes the package set", true
	case p.PolicyFixes > 0:
		return fmt.Sprintf("%d compressed file(s) break the storage policy and will be rewritten", p.PolicyFixes), true
	case recompress && p.Reencode > 0:
		return fmt.Sprintf("%d compressed file(s) will be recompressed; the estimate assumes no gain, "+
			"so the result is kept only if it reaches the minimum gain (at least one byte)", p.Reencode), true
	}
	percent := 100 * float64(p.EstimatedSavings) / float64(max64(p.ArchiveSize, 1))
	need := int64(math.Ceil(float64(p.ArchiveSize) * minGain / 100))
	switch {
	case p.EstimatedSavings >= max64(need, 1):
		return fmt.Sprintf("estimated savings %.1f%% reach the %g%% minimum gain", percent, minGain), true
	case len(p.Deviations) > 0:
		return fmt.Sprintf("the archive deviates from the NUP spec and will be normalized (estimated savings %.1f%%)", math.Max(percent, 0)), true
	}
	return fmt.Sprintf("already optimized: estimated savings %.1f%% are below the %g%% minimum gain", math.Max(percent, 0), minGain), false
}

// planRepack lays out the package set like Pack and decides how each distinct
// content is stored.
func planRepack(ctx context.Context, a *Archive, packages []Package, level int, policy codecPolicy, recompress bool, progress func(string, float64)) *repacking {
	r := &repacking{ctx: ctx, a: a, packages: packages, level: level, policy: policy, recompress: recompress, progress: progress}
	r.l = planLayout(ctx, packages)
	p := &r.plan
	p.ArchiveSize = a.stamp.size
	p.Packages = len(packages)
	p.Files = len(r.l.files)
	p.Deviations = a.Deviations()
	// Every non-empty content is stored once, at its first file in layout
	// order (like Pack). Its bytes come from the best of all the extents
	// that hold it (pickSource); every other extent is still read and
	// checked against the SHA-256 before it is dropped.
	type group struct{ nodes []int }
	groups := map[seenKey]*group{}
	var order []seenKey
	for i, f := range r.l.files {
		binfmt.Check(ctx)
		src := f.source
		binfmt.Require(src.archive == a, "Only files of the opened NUP can be repacked.")
		rec := a.files[src.index]
		p.ContentBytes += rec.size
		r.nodes = append(r.nodes, nodePlan{rec: src.index, own: src.index, writer: -1})
		if rec.size == 0 {
			continue
		}
		key := seenKey{digest: rec.digest, size: rec.size}
		g := groups[key]
		if g == nil {
			g = &group{}
			groups[key] = g
			order = append(order, key)
		}
		g.nodes = append(g.nodes, i)
	}
	covered := map[extentKey]bool{}
	extentOf := func(rec fileRecord) extentKey { return extentKey{off: rec.off, end: rec.off + rec.stored} }
	for _, key := range order {
		g := groups[key]
		first := g.nodes[0]
		best := r.pickSource(g.nodes)
		bestRec := a.files[best]
		covered[extentOf(bestRec)] = true
		np := &r.nodes[first]
		np.writer = first
		np.rec = best
		if best != np.own {
			p.BetterExtents++
		}
		np.act, np.oldOK = r.decide(bestRec)
		p.UniqueContents++
		switch np.act {
		case actReuseFrame:
			p.ReuseFrames++
		case actKeepRaw:
			p.KeepRaw++
		case actCompress:
			p.Compress++
			p.CompressBytes += bestRec.size
		case actReencode:
			p.Reencode++
			if !np.oldOK {
				p.PolicyFixes++
			}
		case actDecode:
			p.Decode++
			p.DecodeBytes += bestRec.size
			p.PolicyFixes++
		}
		for _, n := range g.nodes {
			nd := &r.nodes[n]
			nd.writer = first
			if n != first {
				p.DuplicateFiles++
			}
			own := a.files[nd.own]
			if ext := extentOf(own); !covered[ext] {
				covered[ext] = true
				nd.verify = true
				p.RedundantBytes += own.stored
				if act, oldOK := r.decide(own); act == actDecode || act == actReencode && !oldOK {
					p.PolicyFixes++
				}
			}
		}
	}
	r.estimateCompression()
	r.estimateSizes()
	switch {
	case p.Reencode > 0:
		p.Exact = false // re-encoded frames are estimated at their current size
		p.Basis = "lower_bound"
	case p.Exact:
		p.Basis = "exact"
	default:
		p.Basis = "estimated"
	}
	return r
}

// pickSource chooses, among the records of nodes that hold one content,
// the extent the output is built from, so that no better existing
// representation is ever thrown away and nothing is decoded or compressed
// needlessly:
//   - files the policy keeps raw: a raw extent (a plain copy), else the
//     smallest frame (decoded);
//   - otherwise the smallest frame that satisfies the policy (reused, or the
//     bar a recompression must beat), else a raw extent (compressed), else
//     the smallest frame (re-encoded).
//
// Ties go to the earliest node, so the choice is deterministic.
func (r *repacking) pickSource(nodes []int) int {
	var raw, frame, okFrame = -1, -1, -1
	for _, n := range nodes {
		idx := r.nodes[n].own
		rec := r.a.files[idx]
		switch {
		case rec.codec == 0:
			if raw < 0 {
				raw = idx
			}
		default:
			if frame < 0 || rec.stored < r.a.files[frame].stored {
				frame = idx
			}
			if rec.stored <= r.policy.maxStored(rec.size) && (okFrame < 0 || rec.stored < r.a.files[okFrame].stored) {
				okFrame = idx
			}
		}
	}
	size := r.a.files[r.nodes[nodes[0]].own].size
	if !r.policy.compress(size) {
		if raw >= 0 {
			return raw
		}
		return frame
	}
	switch {
	case okFrame >= 0:
		return okFrame
	case raw >= 0:
		return raw
	}
	return frame
}

func (r *repacking) decide(rec fileRecord) (action, bool) {
	oldOK := rec.codec == 1 && rec.stored <= r.policy.maxStored(rec.size)
	switch {
	case !r.policy.compress(rec.size) && rec.codec == 1:
		return actDecode, false
	case !r.policy.compress(rec.size):
		return actKeepRaw, false
	case rec.codec == 0:
		return actCompress, false
	case oldOK && !r.recompress:
		return actReuseFrame, true
	default:
		return actReencode, oldOK
	}
}

// estimateCompression predicts the frame size of every raw file planned for
// compression by trial-compressing it, or samples of it.
func (r *repacking) estimateCompression() {
	var cands []int
	var total int64
	for i, np := range r.nodes {
		if np.writer == i && np.act == actCompress {
			cands = append(cands, i)
			total += r.a.files[np.rec].size
		}
	}
	r.estimate = make(map[int]int64, len(cands))
	r.plan.Exact = true
	if len(cands) == 0 {
		return
	}
	// One encoder and one read buffer, from the working-memory budget.
	if err := memBudget.acquire(r.ctx, encoderCost(r.level)+Block); err != nil {
		binfmt.Check(r.ctx)
		binfmt.Fail("%v", err)
	}
	defer memBudget.release(encoderCost(r.level) + Block)
	counter := &countWriter{}
	enc, err := newZstdEncoder(counter, r.level, 0)
	binfmt.Require(err == nil, "%v", err)
	buf := make([]byte, Block)

	if total <= sampleBudget {
		var done int64
		for _, i := range cands {
			binfmt.Check(r.ctx)
			rec := r.a.files[r.nodes[i].rec]
			counter.n = 0
			enc.ResetContentSize(counter, rec.size)
			for off := int64(0); off < rec.size; off += int64(len(buf)) {
				n := min64(int64(len(buf)), rec.size-off)
				r.readRaw(rec, off, buf[:n])
				if _, err := enc.Write(buf[:n]); err != nil {
					binfmt.Fail("%v", err)
				}
			}
			if err := enc.Close(); err != nil {
				binfmt.Fail("%v", err)
			}
			r.estimate[i] = counter.n
			done += rec.size
			r.progress("Analyzing NUP", 100*float64(done)/float64(total))
		}
		r.plan.SampledBytes = total
		return
	}

	// Files without a window of their own take the ratio of their extension,
	// else of their size class (powers of two), else of every sample: game
	// data compresses consistently per file type, and pre-compressed media
	// must not drag down the estimate for everything else.
	r.plan.Exact = false
	prefix := make([]int64, len(cands)+1)
	for j, i := range cands {
		prefix[j+1] = prefix[j] + r.a.files[r.nodes[i].rec].size
	}
	type tally struct{ raw, packed int64 }
	per := map[int]*tally{}
	byExt := map[string]*tally{}
	byClass := map[int]*tally{}
	var all tally
	ext := func(rec fileRecord) string { return strings.ToLower(filepath.Ext(rec.name)) }
	class := func(rec fileRecord) int { return bits.Len64(uint64(rec.size)) }
	count := func(m *tally, raw, packed int64) {
		m.raw += raw
		m.packed += packed
	}
	seen := map[[2]int64]bool{}
	var packed []byte
	windows := int64(sampleBudget / sampleWindow)
	for s := int64(0); s < windows; s++ {
		binfmt.Check(r.ctx)
		point := (2*s + 1) * total / (2 * windows)
		j := sort.Search(len(cands), func(j int) bool { return prefix[j+1] > point })
		i := cands[j]
		rec := r.a.files[r.nodes[i].rec]
		length := min64(sampleWindow, rec.size)
		start := point - prefix[j] - length/2
		start = max64(0, min64(start, rec.size-length)) / zstdBlock * zstdBlock
		if seen[[2]int64{int64(i), start}] {
			continue
		}
		seen[[2]int64{int64(i), start}] = true
		r.readRaw(rec, start, buf[:length])
		packed = enc.EncodeAll(buf[:length], packed[:0])
		n := int64(len(packed))
		if per[i] == nil {
			per[i] = &tally{}
		}
		if byExt[ext(rec)] == nil {
			byExt[ext(rec)] = &tally{}
		}
		if byClass[class(rec)] == nil {
			byClass[class(rec)] = &tally{}
		}
		count(per[i], length, n)
		r.progress("Analyzing NUP", 100*float64(s+1)/float64(windows))
	}
	// Each sampled file is estimated from its own windows and the storage
	// policy decides, as the writer will, whether it gets a frame or stays
	// raw (minimum savings, large-file rule). Files without windows take
	// the policy-applied outcome of the sampled files of their extension,
	// else size class, else all: an average of frames and raw files, so it
	// is used as is rather than passed through the policy a second time.
	r.expected = map[int]bool{}
	for _, i := range cands {
		t := per[i]
		if t == nil {
			continue
		}
		rec := r.a.files[r.nodes[i].rec]
		est := int64(math.Ceil(float64(rec.size) * float64(t.packed) / float64(t.raw)))
		if est > r.policy.maxStored(rec.size) {
			est = rec.size
		}
		r.estimate[i] = est
		outcome := int64(math.Ceil(float64(t.raw) * float64(est) / float64(max64(rec.size, 1))))
		if byExt[ext(rec)] == nil {
			byExt[ext(rec)] = &tally{}
		}
		if byClass[class(rec)] == nil {
			byClass[class(rec)] = &tally{}
		}
		count(byExt[ext(rec)], t.raw, outcome)
		count(byClass[class(rec)], t.raw, outcome)
		count(&all, t.raw, outcome)
	}
	r.plan.SampledBytes = all.raw
	for _, i := range cands {
		if per[i] != nil {
			continue
		}
		rec := r.a.files[r.nodes[i].rec]
		t := byExt[ext(rec)]
		if t == nil {
			t = byClass[class(rec)]
		}
		if t == nil {
			t = &all
		}
		r.estimate[i] = min64(rec.size, int64(math.Ceil(float64(rec.size)*float64(t.packed)/float64(t.raw))))
		r.expected[i] = true
	}
}

func (r *repacking) readRaw(rec fileRecord, off int64, dst []byte) {
	n, err := r.a.f.ReadAt(dst, rec.off+off)
	if err != nil && !(err == io.EOF && n == len(dst)) {
		binfmt.Fail("%v", err)
	}
}

// estimateSizes predicts the output size and bounds it while it is written.
// Like Pack, every node that is not a duplicate (empty files included)
// aligns the cursor.
func (r *repacking) estimateSizes() {
	pos, worst := r.l.bloboff, r.l.bloboff
	for i, np := range r.nodes {
		if np.writer < 0 {
			pos, worst = binfmt.Align(pos, 16), binfmt.Align(worst, 16)
		}
		if np.writer != i {
			continue
		}
		rec := r.a.files[np.rec]
		est, high := rec.size, rec.size
		switch np.act {
		case actReuseFrame:
			est, high = rec.stored, rec.stored
		case actCompress:
			if e := r.estimate[i]; e <= r.policy.maxStored(rec.size) || r.expected[i] {
				est = e
			}
		case actReencode:
			if np.oldOK {
				est, high = rec.stored, rec.stored
			}
		}
		pos = binfmt.Align(pos, 16) + est
		worst = binfmt.Align(worst, 16) + high
	}
	var meta int64
	for _, p := range r.packages {
		meta += int64(len(tlvBytes(p.Metadata)))
	}
	r.plan.EstimatedSize = pos + meta
	r.plan.WorstCaseSize = worst + meta
	r.plan.EstimatedSavings = r.plan.ArchiveSize - r.plan.EstimatedSize
	r.plan.ExactSavings = r.plan.ArchiveSize - r.plan.WorstCaseSize
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// writeError marks a failed write to the output so it is never mistaken
// for a compression failure.
type writeError struct{ err error }

func (e *writeError) Error() string { return e.err.Error() }
func (e *writeError) Unwrap() error { return e.err }

var errFrameTooLarge = errors.New("zstd frame exceeds its size limit")

// sink appends to the output file and tracks the write position.
type sink struct {
	f   outFile
	pos int64
}

func (s *sink) Write(p []byte) (int, error) {
	n, err := s.f.Write(p)
	s.pos += int64(n)
	if err != nil {
		return n, &writeError{err}
	}
	return n, nil
}

// rewind drops everything written after start.
func (s *sink) rewind(start int64) {
	if err := s.f.Truncate(start); err != nil {
		binfmt.Fail("%v", err)
	}
	if _, err := s.f.Seek(start, io.SeekStart); err != nil {
		binfmt.Fail("%v", err)
	}
	s.pos = start
}

// frameLimit refuses writes that would grow a frame past limit bytes.
type frameLimit struct {
	w        io.Writer
	n, limit int64
}

func (f *frameLimit) Write(p []byte) (int, error) {
	if f.n+int64(len(p)) > f.limit {
		return 0, errFrameTooLarge
	}
	n, err := f.w.Write(p)
	f.n += int64(n)
	return n, err
}

type repackWriter struct {
	*repacking
	out      *sink
	enc      *zstd.Encoder
	dec      *zstd.Decoder
	buf      []byte
	stats    RepackStats
	total    int64
	done     int64
	reported int64
	report   func(done, total int64)
}

// progress reports how far the write has come. A file that falls back from
// compression to raw is read a second time; that must not move it back.
func (w *repackWriter) progress(done int64) {
	if done > w.reported {
		w.reported = done
		w.report(done, w.total)
	}
}

// write streams every extent, the metadata and the TOC into out.
// outFile is what the writer needs from its output: an *os.File, or a
// partWriter for an archive written in parts.
type outFile interface {
	io.Writer
	io.WriterAt
	io.Seeker
	Truncate(size int64) error
	Sync() error
	Close() error
}

func (r *repacking) write(out outFile, report func(done, total int64)) (stats RepackStats, err error) {
	defer binfmt.Recover(&err)
	w := &repackWriter{repacking: r, out: &sink{f: out}, buf: make([]byte, Block), report: report}
	defer func() {
		if w.dec != nil {
			w.dec.Close()
		}
	}()
	for _, np := range r.nodes {
		w.total += r.a.files[np.rec].size
	}
	if err := writeZeros(out, r.l.bloboff); err != nil {
		return stats, err
	}
	w.out.pos = r.l.bloboff
	w.stats.UniqueContents = r.plan.UniqueContents
	w.stats.DuplicateFiles = r.plan.DuplicateFiles
	par, perr := w.startParallel()
	if perr != nil {
		binfmt.Check(r.ctx)
		return stats, perr
	}
	defer par.stop()

	for i := range r.l.files {
		binfmt.Check(r.ctx)
		np := r.nodes[i]
		rec := r.a.files[np.rec]
		f := &r.l.files[i]
		f.digest = rec.digest
		switch {
		case np.writer < 0:
			binfmt.Require(rec.digest == emptyDigest, "File SHA-256 mismatch: %s", rec.name)
			start := binfmt.Align(w.out.pos, 16)
			w.must(writeZeros(out, start-w.out.pos))
			w.out.pos = start
			f.off, f.stored, f.codec = 0, 0, 0
		case np.writer != i:
			if np.verify {
				w.must(w.content(rec, nil))
			}
			src := r.l.files[np.writer]
			f.off, f.stored, f.codec = src.off, src.stored, src.codec
		default:
			if np.verify { // the first copy is dropped for a better extent: check it
				w.must(w.content(r.a.files[np.own], nil))
			}
			start := binfmt.Align(w.out.pos, 16)
			w.must(writeZeros(out, start-w.out.pos))
			w.out.pos = start
			f.off = start
			if j := par.take(i); j != nil {
				w.must(j.data.each(func(b []byte) error { _, err := w.out.Write(b); return err }))
				f.stored, f.codec = j.data.n, j.codec
				*j.stat++
				par.written(j)
			} else {
				f.stored, f.codec = w.store(np, rec)
			}
		}
		w.done += rec.size
		w.progress(w.done)
	}

	meta := make([]metaExtent, len(r.packages))
	for i := range r.packages {
		data := tlvBytes(r.packages[i].Metadata)
		binfmt.Require(len(data) <= 32*1024*1024, "Package metadata exceeds 32 MiB.")
		if len(data) != 0 {
			meta[i] = metaExtent{off: w.out.pos, size: int64(len(data))}
			_, werr := w.out.Write(data)
			w.must(werr)
		}
	}
	toc := r.l.toc(r.packages, meta)
	header := superblock(r.packages, toc, r.l.bloboff, w.out.pos)
	if _, err := out.WriteAt(header, 0); err != nil {
		return stats, err
	}
	if _, err := out.WriteAt(toc, 128); err != nil {
		return stats, err
	}
	if err := out.Sync(); err != nil {
		return stats, err
	}
	return w.stats, nil
}

func (w *repackWriter) must(err error) {
	if err != nil {
		binfmt.Fail("%v", err)
	}
}

// store writes one distinct content at the current output position and
// returns its stored size and codec.
func (w *repackWriter) store(np nodePlan, rec fileRecord) (int64, int) {
	start := w.out.pos
	switch np.act {
	case actReuseFrame:
		w.copyExtent(rec)
		w.stats.ReusedFrames++
		return rec.stored, 1
	case actKeepRaw:
		w.must(w.content(rec, w.out))
		w.stats.KeptRaw++
		return rec.size, 0
	case actDecode:
		w.must(w.content(rec, w.out))
		w.stats.Decompressed++
		return rec.size, 0
	case actCompress:
		if n, ok := w.encode(rec, w.policy.maxStored(rec.size)); ok {
			w.stats.Compressed++
			return n, 1
		}
		w.out.rewind(start)
		w.must(w.content(rec, w.out))
		w.stats.Incompressible++
		return rec.size, 0
	default: // actReencode
		limit := w.policy.maxStored(rec.size)
		if np.oldOK {
			limit = min64(limit, rec.stored-1)
		}
		if n, ok := w.encode(rec, limit); ok {
			w.stats.Recompressed++
			return n, 1
		}
		w.out.rewind(start)
		if np.oldOK {
			w.copyExtent(rec)
			w.stats.ReusedFrames++
			return rec.stored, 1
		}
		w.must(w.content(rec, w.out))
		w.stats.Decompressed++
		return rec.size, 0
	}
}

// encode writes rec's content as one zstd frame of at most limit bytes. It
// reports false, leaving the partial frame for the caller to drop, when the
// frame would be larger or the encoder fails.
func (w *repackWriter) encode(rec fileRecord, limit int64) (int64, bool) {
	if w.enc == nil {
		enc, err := newZstdEncoder(io.Discard, w.level, 0)
		if err != nil {
			return 0, false
		}
		w.enc = enc
	}
	fl := &frameLimit{w: w.out, limit: limit}
	w.enc.ResetContentSize(fl, rec.size)
	err := w.content(rec, w.enc)
	if err == nil {
		err = w.enc.Close()
	}
	var we *writeError
	if errors.As(err, &we) {
		binfmt.Fail("%v", we.err)
	}
	return fl.n, err == nil
}

// content streams the logical bytes of input record rec into dst (nil only
// hashes them) and checks their size and SHA-256. Input problems fail the
// repack; dst errors are returned.
func (w *repackWriter) content(rec fileRecord, dst io.Writer) error {
	var src io.Reader = io.NewSectionReader(w.a.f, rec.off, rec.stored)
	if rec.codec == 1 {
		if w.dec == nil {
			dec, err := zstd.NewReader(nil, zstd.WithDecoderMaxWindow(MaxWindow), zstd.WithDecoderConcurrency(1))
			if err != nil {
				binfmt.Fail("%v", err)
			}
			w.dec = dec
		}
		if err := w.dec.Reset(src); err != nil {
			binfmt.Fail("%v", err)
		}
		src = w.dec
	}
	h := sha256.New()
	var count int64
	for {
		binfmt.Check(w.ctx)
		n, rerr := src.Read(w.buf)
		if n > 0 {
			count += int64(n)
			binfmt.Require(count <= rec.size, "Decompressed file exceeds its declared size.")
			h.Write(w.buf[:n])
			if dst != nil {
				if _, err := dst.Write(w.buf[:n]); err != nil {
					return err
				}
			}
			w.progress(w.done + count)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			binfmt.Fail("%v", rerr)
		}
	}
	binfmt.Require(count == rec.size && bytes.Equal(h.Sum(nil), rec.digest[:]), "File SHA-256 mismatch: %s", rec.name)
	return nil
}

// copyExtent copies rec's stored bytes verbatim; the output verification
// checks them against the file's SHA-256.
func (w *repackWriter) copyExtent(rec fileRecord) {
	src := io.NewSectionReader(w.a.f, rec.off, rec.stored)
	var copied int64
	for {
		binfmt.Check(w.ctx)
		n, rerr := src.Read(w.buf)
		if n > 0 {
			_, err := w.out.Write(w.buf[:n])
			w.must(err)
			copied += int64(n)
			w.progress(w.done + int64(float64(rec.size)*float64(copied)/float64(rec.stored)))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			binfmt.Fail("%v", rerr)
		}
	}
	binfmt.Require(copied == rec.stored, "Truncated archive.")
}

// keepFilesError reports a failed replacement after which the temporary
// file may be the only complete copy: the caller must not delete it.
type keepFilesError struct{ msg string }

func (e *keepFilesError) Error() string { return e.msg }

// countingFile is an outFile that keeps nothing: it records the size the
// output would have, so a rewrite can be measured without writing it.
type countingFile struct{ pos, size int64 }

func (c *countingFile) Write(p []byte) (int, error) {
	c.pos += int64(len(p))
	c.size = max64(c.size, c.pos)
	return len(p), nil
}

func (c *countingFile) WriteAt(p []byte, off int64) (int, error) {
	c.size = max64(c.size, off+int64(len(p)))
	return len(p), nil
}

func (c *countingFile) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		c.pos = off
	case io.SeekCurrent:
		c.pos += off
	default:
		c.pos = c.size + off
	}
	return c.pos, nil
}

func (c *countingFile) Truncate(n int64) error { c.size = n; return nil }
func (c *countingFile) Sync() error            { return nil }
func (c *countingFile) Close() error           { return nil }
