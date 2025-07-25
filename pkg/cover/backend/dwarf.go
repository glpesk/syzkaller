// Copyright 2021 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package backend

import (
	"bufio"
	"bytes"
	"debug/dwarf"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/google/syzkaller/pkg/log"
	"github.com/google/syzkaller/pkg/mgrconfig"
	"github.com/google/syzkaller/pkg/osutil"
	"github.com/google/syzkaller/pkg/symbolizer"
	"github.com/google/syzkaller/pkg/vminfo"
	"github.com/google/syzkaller/sys/targets"
)

type dwarfParams struct {
	target                *targets.Target
	kernelDirs            *mgrconfig.KernelDirs
	splitBuildDelimiters  []string
	moduleObj             []string
	hostModules           []*vminfo.KernelModule
	readSymbols           func(*vminfo.KernelModule, *symbolInfo) ([]*Symbol, error)
	readTextData          func(*vminfo.KernelModule) ([]byte, error)
	readModuleCoverPoints func(*targets.Target, *vminfo.KernelModule, *symbolInfo) ([2][]uint64, error)
	readTextRanges        func(*vminfo.KernelModule) ([]pcRange, []*CompileUnit, error)
	getCompilerVersion    func(string) string
}

type Arch struct {
	scanSize      int
	callLen       int
	relaOffset    uint64
	callRelocType uint64
	isCallInsn    func(arch *Arch, insn []byte) bool
	callTarget    func(arch *Arch, insn []byte, pc uint64) uint64
}

var arches = map[string]*Arch{
	targets.AMD64: {
		scanSize:      1,
		callLen:       5,
		relaOffset:    1,
		callRelocType: uint64(elf.R_X86_64_PLT32),
		isCallInsn: func(arch *Arch, insn []byte) bool {
			return insn[0] == 0xe8
		},
		callTarget: func(arch *Arch, insn []byte, pc uint64) uint64 {
			off := uint64(int64(int32(binary.LittleEndian.Uint32(insn[1:]))))
			return pc + off + uint64(arch.callLen)
		},
	},
	targets.ARM64: {
		scanSize:      4,
		callLen:       4,
		callRelocType: uint64(elf.R_AARCH64_CALL26),
		isCallInsn: func(arch *Arch, insn []byte) bool {
			const mask = uint32(0xfc000000)
			const opc = uint32(0x94000000)
			return binary.LittleEndian.Uint32(insn)&mask == opc
		},
		callTarget: func(arch *Arch, insn []byte, pc uint64) uint64 {
			off26 := binary.LittleEndian.Uint32(insn) & 0x3ffffff
			sign := off26 >> 25
			off := uint64(off26)
			// Sign-extend the 26-bit offset stored in the instruction.
			if sign == 1 {
				off |= 0xfffffffffc000000
			}
			return pc + 4*off
		},
	},
	targets.S390x: {
		scanSize:      1,
		callLen:       6,
		callRelocType: uint64(elf.R_390_PLT32DBL),
		isCallInsn: func(arch *Arch, insn []byte) bool {
			return insn[0] == 0xc0 && insn[1] == 0xe5
		},
		callTarget: func(arch *Arch, insn []byte, pc uint64) uint64 {
			off := uint64(int64(int32(binary.BigEndian.Uint32(insn[2:]))))
			return pc + 2*off
		},
	},
}

func makeDWARF(params *dwarfParams) (impl *Impl, err error) {
	defer func() {
		// It turns out that the DWARF-parsing library in Go crashes while parsing DWARF 5 data.
		// As GCC11 uses DWARF 5 by default, we can expect larger number of problems with
		// syzkallers compiled using old go versions.
		// So we just catch the panic and turn it into a meaningful error message.
		if recErr := recover(); recErr != nil {
			impl = nil
			err = fmt.Errorf("panic occurred while parsing DWARF "+
				"(possible remedy: use go1.16+ which support DWARF 5 debug data): %s", recErr)
		}
	}()
	impl, err = makeDWARFUnsafe(params)
	return
}

type Result struct {
	CoverPoints [2][]uint64
	Symbols     []*Symbol
}

func processModule(params *dwarfParams, module *vminfo.KernelModule, info *symbolInfo,
	target *targets.Target) (*Result, error) {
	symbols, err := params.readSymbols(module, info)
	if err != nil {
		return nil, err
	}

	var data []byte
	var coverPoints [2][]uint64
	if _, ok := arches[target.Arch]; !ok {
		coverPoints, err = objdump(target, module)
	} else if module.Name == "" {
		data, err = params.readTextData(module)
		if err != nil {
			return nil, err
		}
		coverPoints, err = readCoverPoints(target, info, data)
	} else {
		coverPoints, err = params.readModuleCoverPoints(target, module, info)
	}
	if err != nil {
		return nil, err
	}

	result := &Result{
		Symbols:     symbols,
		CoverPoints: coverPoints,
	}
	return result, nil
}

func makeDWARFUnsafe(params *dwarfParams) (*Impl, error) {
	target := params.target
	kernelDirs := params.kernelDirs
	splitBuildDelimiters := params.splitBuildDelimiters
	modules := params.hostModules

	// Here and below index 0 refers to coverage callbacks (__sanitizer_cov_trace_pc(_guard))
	// and index 1 refers to comparison callbacks (__sanitizer_cov_trace_cmp*).
	var allCoverPoints [2][]uint64
	var allSymbols []*Symbol
	var allRanges []pcRange
	var allUnits []*CompileUnit
	preciseCoverage := true
	type binResult struct {
		symbols     []*Symbol
		coverPoints [2][]uint64
		ranges      []pcRange
		units       []*CompileUnit
		err         error
	}
	binC := make(chan binResult, len(modules))
	for _, module := range modules {
		go func() {
			info := &symbolInfo{
				tracePC:     make(map[uint64]bool),
				traceCmp:    make(map[uint64]bool),
				tracePCIdx:  make(map[int]bool),
				traceCmpIdx: make(map[int]bool),
			}
			result, err := processModule(params, module, info, target)
			if err != nil {
				binC <- binResult{err: err}
				return
			}
			if module.Name == "" && len(result.CoverPoints[0]) == 0 {
				err = fmt.Errorf("%v doesn't contain coverage callbacks (set CONFIG_KCOV=y on linux)", module.Path)
				binC <- binResult{err: err}
				return
			}
			ranges, units, err := params.readTextRanges(module)
			if err != nil {
				binC <- binResult{err: err}
				return
			}
			binC <- binResult{symbols: result.Symbols, coverPoints: result.CoverPoints, ranges: ranges, units: units}
		}()
		if isKcovBrokenInCompiler(params.getCompilerVersion(module.Path)) {
			preciseCoverage = false
		}
	}
	for range modules {
		result := <-binC
		if err := result.err; err != nil {
			return nil, err
		}
		allSymbols = append(allSymbols, result.symbols...)
		allCoverPoints[0] = append(allCoverPoints[0], result.coverPoints[0]...)
		allCoverPoints[1] = append(allCoverPoints[1], result.coverPoints[1]...)
		allRanges = append(allRanges, result.ranges...)
		allUnits = append(allUnits, result.units...)
	}
	log.Logf(1, "discovered %v source files, %v symbols", len(allUnits), len(allSymbols))
	// TODO: need better way to remove symbols having the same Start
	uniqSymbs := make(map[uint64]*Symbol)
	for _, sym := range allSymbols {
		if _, ok := uniqSymbs[sym.Start]; !ok {
			uniqSymbs[sym.Start] = sym
		}
	}
	allSymbols = []*Symbol{}
	for _, sym := range uniqSymbs {
		allSymbols = append(allSymbols, sym)
	}
	sort.Slice(allSymbols, func(i, j int) bool {
		return allSymbols[i].Start < allSymbols[j].Start
	})
	sort.Slice(allRanges, func(i, j int) bool {
		return allRanges[i].start < allRanges[j].start
	})
	for k := range allCoverPoints {
		sort.Slice(allCoverPoints[k], func(i, j int) bool {
			return allCoverPoints[k][i] < allCoverPoints[k][j]
		})
	}

	allSymbols = buildSymbols(allSymbols, allRanges, allCoverPoints)
	nunit := 0
	for _, unit := range allUnits {
		if len(unit.PCs) == 0 {
			continue // drop the unit
		}
		// TODO: objDir won't work for out-of-tree modules.
		unit.Name, unit.Path = CleanPath(unit.Name, kernelDirs, splitBuildDelimiters)
		allUnits[nunit] = unit
		nunit++
	}
	allUnits = allUnits[:nunit]
	if len(allSymbols) == 0 || len(allUnits) == 0 {
		return nil, fmt.Errorf("failed to parse DWARF (set CONFIG_DEBUG_INFO=y on linux)")
	}
	var interner symbolizer.Interner
	impl := &Impl{
		Units:   allUnits,
		Symbols: allSymbols,
		Symbolize: func(pcs map[*vminfo.KernelModule][]uint64) ([]*Frame, error) {
			return symbolize(target, &interner, kernelDirs, splitBuildDelimiters, pcs)
		},
		CallbackPoints:  allCoverPoints[0],
		PreciseCoverage: preciseCoverage,
	}
	return impl, nil
}

func buildSymbols(symbols []*Symbol, ranges []pcRange, coverPoints [2][]uint64) []*Symbol {
	// Assign coverage point PCs to symbols.
	// Both symbols and coverage points are sorted, so we do it one pass over both.
	selectPCs := func(u *ObjectUnit, typ int) *[]uint64 {
		return [2]*[]uint64{&u.PCs, &u.CMPs}[typ]
	}
	for pcType := range coverPoints {
		pcs := coverPoints[pcType]
		var curSymbol *Symbol
		firstSymbolPC, symbolIdx := -1, 0
		for i := 0; i < len(pcs); i++ {
			pc := pcs[i]
			for ; symbolIdx < len(symbols) && pc >= symbols[symbolIdx].End; symbolIdx++ {
			}
			var symb *Symbol
			if symbolIdx < len(symbols) && pc >= symbols[symbolIdx].Start && pc < symbols[symbolIdx].End {
				symb = symbols[symbolIdx]
			}
			if curSymbol != nil && curSymbol != symb {
				*selectPCs(&curSymbol.ObjectUnit, pcType) = pcs[firstSymbolPC:i]
				firstSymbolPC = -1
			}
			curSymbol = symb
			if symb != nil && firstSymbolPC == -1 {
				firstSymbolPC = i
			}
		}
		if curSymbol != nil {
			*selectPCs(&curSymbol.ObjectUnit, pcType) = pcs[firstSymbolPC:]
		}
	}
	// Assign compile units to symbols based on unit pc ranges.
	// Do it one pass as both are sorted.
	nsymbol := 0
	rangeIndex := 0
	for _, s := range symbols {
		for ; rangeIndex < len(ranges) && ranges[rangeIndex].end <= s.Start; rangeIndex++ {
		}
		if rangeIndex == len(ranges) || s.Start < ranges[rangeIndex].start || len(s.PCs) == 0 {
			continue // drop the symbol
		}
		unit := ranges[rangeIndex].unit
		s.Unit = unit
		symbols[nsymbol] = s
		nsymbol++
	}
	symbols = symbols[:nsymbol]

	for pcType := range coverPoints {
		for _, s := range symbols {
			symbPCs := selectPCs(&s.ObjectUnit, pcType)
			unitPCs := selectPCs(&s.Unit.ObjectUnit, pcType)
			pos := len(*unitPCs)
			*unitPCs = append(*unitPCs, *symbPCs...)
			*symbPCs = (*unitPCs)[pos:]
		}
	}
	return symbols
}

// Regexps to parse compiler version string in isKcovBrokenInCompiler.
// Some targets (e.g. NetBSD) use g++ instead of gcc.
var gccRE = regexp.MustCompile(`gcc|GCC|g\+\+`)
var gccVersionRE = regexp.MustCompile(`(gcc|GCC|g\+\+).* ([0-9]{1,2})\.[0-9]+\.[0-9]+`)

// GCC < 14 incorrectly tail-calls kcov callbacks, which does not let syzkaller
// verify that collected coverage points have matching callbacks.
// See https://github.com/google/syzkaller/issues/4447 for more information.
func isKcovBrokenInCompiler(versionStr string) bool {
	if !gccRE.MatchString(versionStr) {
		return false
	}
	groups := gccVersionRE.FindStringSubmatch(versionStr)
	if len(groups) > 0 {
		version, err := strconv.Atoi(groups[2])
		if err == nil {
			return version < 14
		}
	}
	return true
}

type symbolInfo struct {
	textAddr uint64
	// Set of addresses that correspond to __sanitizer_cov_trace_pc or its trampolines.
	tracePC     map[uint64]bool
	traceCmp    map[uint64]bool
	tracePCIdx  map[int]bool
	traceCmpIdx map[int]bool
}

type pcRange struct {
	// [start; end)
	start uint64
	end   uint64
	unit  *CompileUnit
}

type pcFixFn = (func([2]uint64) ([2]uint64, bool))

func readTextRanges(debugInfo *dwarf.Data, module *vminfo.KernelModule, pcFix pcFixFn) (
	[]pcRange, []*CompileUnit, error) {
	var ranges []pcRange
	unitMap := map[string]*CompileUnit{}
	addRange := func(r [2]uint64, fileName string) {
		if pcFix != nil {
			var filtered bool
			r, filtered = pcFix(r)
			if filtered {
				return
			}
		}
		unit, ok := unitMap[fileName]
		if !ok {
			unit = &CompileUnit{
				ObjectUnit: ObjectUnit{
					Name: fileName,
				},
				Module: module,
			}
			unitMap[fileName] = unit
		}
		if module.Name == "" {
			ranges = append(ranges, pcRange{r[0], r[1], unit})
		} else {
			ranges = append(ranges, pcRange{r[0] + module.Addr, r[1] + module.Addr, unit})
		}
	}

	for r := debugInfo.Reader(); ; {
		ent, err := r.Next()
		if err != nil {
			return nil, nil, err
		}
		if ent == nil {
			break
		}
		if ent.Tag != dwarf.TagCompileUnit {
			return nil, nil, fmt.Errorf("found unexpected tag %v on top level", ent.Tag)
		}
		attrName, ok := ent.Val(dwarf.AttrName).(string)
		if !ok {
			continue
		}
		attrCompDir, _ := ent.Val(dwarf.AttrCompDir).(string)

		const languageRust = 28
		if language, ok := ent.Val(dwarf.AttrLanguage).(int64); ok && language == languageRust {
			rawRanges, err := rustRanges(debugInfo, ent)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to query Rust PC ranges: %w", err)
			}
			for _, r := range rawRanges {
				addRange([2]uint64{r.start, r.end}, r.file)
			}
		} else {
			// Compile unit names are relative to the compilation dir,
			// while per-line info isn't.
			// attrName could be an absolute path for out-of-tree modules.
			unitName := attrName
			if !filepath.IsAbs(attrName) {
				unitName = filepath.Join(attrCompDir, attrName)
			}
			ranges1, err := debugInfo.Ranges(ent)
			if err != nil {
				return nil, nil, err
			}
			for _, r := range ranges1 {
				addRange(r, unitName)
			}
		}
		r.SkipChildren()
	}
	var units []*CompileUnit
	for _, unit := range unitMap {
		units = append(units, unit)
	}
	return ranges, units, nil
}

type rustRange struct {
	// [start; end)
	start uint64
	end   uint64
	file  string
}

func rustRanges(debugInfo *dwarf.Data, ent *dwarf.Entry) ([]rustRange, error) {
	// For Rust, a single compilation unit may comprise all .rs files that belong to the crate.
	// To properly render the coverage, we need to somehow infer the ranges that belong to
	// those individual .rs files.
	// For simplicity, let's create fake ranges by looking at the DWARF line information.
	var ret []rustRange
	lr, err := debugInfo.LineReader(ent)
	if err != nil {
		return nil, fmt.Errorf("failed to query line reader: %w", err)
	}
	var startPC uint64
	var files []string
	for {
		var entry dwarf.LineEntry
		if err = lr.Next(&entry); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("failed to parse next line entry: %w", err)
		}
		if startPC == 0 || entry.Address != startPC {
			for _, file := range files {
				ret = append(ret, rustRange{
					start: startPC,
					end:   entry.Address,
					file:  file,
				})
			}
			files = files[:0]
			startPC = entry.Address
		}
		// Keep on collecting file names that are covered by the range.
		files = append(files, entry.File.Name)
	}
	if startPC != 0 {
		// We don't know the end PC for these, but let's still add them to the ranges.
		for _, file := range files {
			ret = append(ret, rustRange{
				start: startPC,
				end:   startPC + 1,
				file:  file,
			})
		}
	}
	return ret, nil
}

func symbolizeModule(target *targets.Target, interner *symbolizer.Interner, kernelDirs *mgrconfig.KernelDirs,
	splitBuildDelimiters []string, mod *vminfo.KernelModule, pcs []uint64) ([]*Frame, error) {
	procs := min(runtime.GOMAXPROCS(0)/2, len(pcs)/1000)
	const (
		minProcs = 1
		maxProcs = 4
	)
	// addr2line on a beefy vmlinux takes up to 1.6GB of RAM, so don't create too many of them.
	procs = min(procs, maxProcs)
	procs = max(procs, minProcs)
	type symbolizerResult struct {
		frames []symbolizer.Frame
		err    error
	}
	symbolizerC := make(chan symbolizerResult, procs)
	pcchan := make(chan []uint64, procs)
	for p := 0; p < procs; p++ {
		go func() {
			symb := symbolizer.Make(target)
			defer symb.Close()
			var res symbolizerResult
			for pcs := range pcchan {
				for i, pc := range pcs {
					if mod.Name == "" {
						pcs[i] = pc
					} else {
						pcs[i] = pc - mod.Addr
					}
				}
				frames, err := symb.Symbolize(mod.Path, pcs...)
				if err != nil {
					res.err = fmt.Errorf("failed to symbolize: %w", err)
				}
				res.frames = append(res.frames, frames...)
			}
			symbolizerC <- res
		}()
	}
	for i := 0; i < len(pcs); {
		end := min(i+100, len(pcs))
		pcchan <- pcs[i:end]
		i = end
	}
	close(pcchan)
	var err0 error
	var frames []*Frame
	for p := 0; p < procs; p++ {
		res := <-symbolizerC
		if res.err != nil {
			err0 = res.err
		}
		for _, frame := range res.frames {
			name, path := CleanPath(frame.File, kernelDirs, splitBuildDelimiters)
			pc := frame.PC
			if mod.Name != "" {
				pc = frame.PC + mod.Addr
			}
			frames = append(frames, &Frame{
				Module:   mod,
				PC:       pc,
				Name:     interner.Do(name),
				FuncName: frame.Func,
				Path:     interner.Do(path),
				Inline:   frame.Inline,
				Range: Range{
					StartLine: frame.Line,
					StartCol:  0,
					EndLine:   frame.Line,
					EndCol:    LineEnd,
				},
			})
		}
	}
	if err0 != nil {
		return nil, err0
	}
	return frames, nil
}

func symbolize(target *targets.Target, interner *symbolizer.Interner, kernelDirs *mgrconfig.KernelDirs,
	splitBuildDelimiters []string, pcs map[*vminfo.KernelModule][]uint64) ([]*Frame, error) {
	var frames []*Frame
	type frameResult struct {
		frames []*Frame
		err    error
	}
	frameC := make(chan frameResult, len(pcs))
	for mod, pcs1 := range pcs {
		go func(mod *vminfo.KernelModule, pcs []uint64) {
			frames, err := symbolizeModule(target, interner, kernelDirs, splitBuildDelimiters, mod, pcs)
			frameC <- frameResult{frames: frames, err: err}
		}(mod, pcs1)
	}
	for range pcs {
		res := <-frameC
		if res.err != nil {
			return nil, res.err
		}
		frames = append(frames, res.frames...)
	}
	return frames, nil
}

// nextCallTarget finds the next call instruction in data[] starting at *pos and returns that
// instruction's target and pc.
func nextCallTarget(arch *Arch, textAddr uint64, data []byte, pos *int) (uint64, uint64) {
	for *pos < len(data) {
		i := *pos
		if i+arch.callLen > len(data) {
			break
		}
		*pos += arch.scanSize
		insn := data[i : i+arch.callLen]
		if !arch.isCallInsn(arch, insn) {
			continue
		}
		pc := textAddr + uint64(i)
		callTarget := arch.callTarget(arch, insn, pc)
		*pos = i + arch.scanSize
		return callTarget, pc
	}
	return 0, 0
}

// readCoverPoints finds all coverage points (calls of __sanitizer_cov_trace_*) in the object file.
// Currently it is [amd64|arm64]-specific: looks for opcode and correct offset.
// Running objdump on the whole object file is too slow.
func readCoverPoints(target *targets.Target, info *symbolInfo, data []byte) ([2][]uint64, error) {
	var pcs [2][]uint64
	if len(info.tracePC) == 0 {
		return pcs, fmt.Errorf("no __sanitizer_cov_trace_pc symbol in the object file")
	}

	i := 0
	arch := arches[target.Arch]
	for {
		callTarget, pc := nextCallTarget(arch, info.textAddr, data, &i)
		if callTarget == 0 {
			break
		}
		if info.tracePC[callTarget] {
			pcs[0] = append(pcs[0], pc)
		} else if info.traceCmp[callTarget] {
			pcs[1] = append(pcs[1], pc)
		}
	}
	return pcs, nil
}

// Source files for Android may be split between two subdirectories: the common AOSP kernel
// and the device-specific drivers: https://source.android.com/docs/setup/build/building-pixel-kernels.
// Android build system references these subdirectories in various ways, which often results in
// paths to non-existent files being recorded in the debug info.
//
// cleanPathAndroid() assumes that the subdirectories reside in `srcDir`, with their names being listed in
// `delimiters`.
// If one of the `delimiters` occurs in the `path`, it is stripped together with the path prefix, and the
// remaining file path is appended to `srcDir + delimiter`.
// If none of the `delimiters` occur in the `path`, `path` is treated as a relative path that needs to be
// looked up in `srcDir + delimiters[i]`.
func cleanPathAndroid(path, srcDir string, delimiters []string, existFn func(string) bool) (string, string) {
	if len(delimiters) == 0 {
		return "", ""
	}
	reStr := "(" + strings.Join(delimiters, "|") + ")(.*)"
	re := regexp.MustCompile(reStr)
	match := re.FindStringSubmatch(path)
	if match != nil {
		delimiter := match[1]
		filename := match[2]
		path := filepath.Clean(srcDir + delimiter + filename)
		return filename, path
	}
	// None of the delimiters found in `path`: it is probably a relative path to the source file.
	// Try to look it up in every subdirectory of srcDir.
	for _, delimiter := range delimiters {
		absPath := filepath.Clean(srcDir + delimiter + path)
		if existFn(absPath) {
			return path, absPath
		}
	}
	return "", ""
}

func CleanPath(path string, kernelDirs *mgrconfig.KernelDirs, splitBuildDelimiters []string) (string, string) {
	filename := ""

	path = filepath.Clean(path)
	aname, apath := cleanPathAndroid(path, kernelDirs.Src, splitBuildDelimiters, osutil.IsExist)
	if aname != "" {
		return aname, apath
	}
	absPath := osutil.Abs(path)
	switch {
	case strings.HasPrefix(absPath, kernelDirs.Obj):
		// Assume the file was built there.
		path = strings.TrimPrefix(absPath, kernelDirs.Obj)
		filename = filepath.Join(kernelDirs.Obj, path)
	case strings.HasPrefix(absPath, kernelDirs.BuildSrc):
		// Assume the file was moved from buildDir to srcDir.
		path = strings.TrimPrefix(absPath, kernelDirs.BuildSrc)
		filename = filepath.Join(kernelDirs.Src, path)
	default:
		// Assume this is relative path.
		filename = filepath.Join(kernelDirs.Src, path)
	}
	return strings.TrimLeft(filepath.Clean(path), "/\\"), filename
}

// objdump is an old, slow way of finding coverage points.
// amd64 uses faster option of parsing binary directly (readCoverPoints).
// TODO: use the faster approach for all other arches and drop this.
func objdump(target *targets.Target, mod *vminfo.KernelModule) ([2][]uint64, error) {
	var pcs [2][]uint64
	cmd := osutil.Command(target.Objdump, "-d", "--no-show-raw-insn", mod.Path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return pcs, err
	}
	defer stdout.Close()
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return pcs, err
	}
	defer stderr.Close()
	if err := cmd.Start(); err != nil {
		return pcs, fmt.Errorf("failed to run objdump on %v: %w", mod.Path, err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	s := bufio.NewScanner(stdout)
	callInsns, traceFuncs := archCallInsn(target)
	for s.Scan() {
		if pc := parseLine(callInsns, traceFuncs, s.Bytes()); pc != 0 {
			if mod.Name != "" {
				pc = pc + mod.Addr
			}
			pcs[0] = append(pcs[0], pc)
		}
	}
	stderrOut, _ := io.ReadAll(stderr)
	if err := cmd.Wait(); err != nil {
		return pcs, fmt.Errorf("failed to run objdump on %v: %w\n%s", mod.Path, err, stderrOut)
	}
	if err := s.Err(); err != nil {
		return pcs, fmt.Errorf("failed to run objdump on %v: %w\n%s", mod.Path, err, stderrOut)
	}
	return pcs, nil
}

func parseLine(callInsns, traceFuncs [][]byte, ln []byte) uint64 {
	pos := -1
	for _, callInsn := range callInsns {
		if pos = bytes.Index(ln, callInsn); pos != -1 {
			break
		}
	}
	if pos == -1 {
		return 0
	}
	hasCall := false
	for _, traceFunc := range traceFuncs {
		if hasCall = bytes.Contains(ln[pos:], traceFunc); hasCall {
			break
		}
	}
	if !hasCall {
		return 0
	}
	for len(ln) != 0 && ln[0] == ' ' {
		ln = ln[1:]
	}
	colon := bytes.IndexByte(ln, ':')
	if colon == -1 {
		return 0
	}
	pc, err := strconv.ParseUint(string(ln[:colon]), 16, 64)
	if err != nil {
		return 0
	}
	return pc
}

func archCallInsn(target *targets.Target) ([][]byte, [][]byte) {
	callName := [][]byte{[]byte(" <__sanitizer_cov_trace_pc>")}
	switch target.Arch {
	case targets.I386:
		// c1000102:       call   c10001f0 <__sanitizer_cov_trace_pc>
		return [][]byte{[]byte("\tcall ")}, callName
	case targets.ARM64:
		// ffff0000080d9cc0:       bl      ffff00000820f478 <__sanitizer_cov_trace_pc>
		return [][]byte{[]byte("\tbl ")}, [][]byte{
			[]byte("<__sanitizer_cov_trace_pc>"),
			[]byte("<____sanitizer_cov_trace_pc_veneer>"),
		}

	case targets.ARM:
		// 8010252c:       bl      801c3280 <__sanitizer_cov_trace_pc>
		return [][]byte{[]byte("\tbl\t")}, callName
	case targets.PPC64LE:
		// c00000000006d904:       bl      c000000000350780 <.__sanitizer_cov_trace_pc>
		// This is only known to occur in the test:
		// 838:   bl      824 <__sanitizer_cov_trace_pc+0x8>
		// This occurs on PPC64LE:
		// c0000000001c21a8:       bl      c0000000002df4a0 <__sanitizer_cov_trace_pc>
		return [][]byte{[]byte("\tbl ")}, [][]byte{
			[]byte("<__sanitizer_cov_trace_pc>"),
			[]byte("<__sanitizer_cov_trace_pc+0x8>"),
			[]byte(" <.__sanitizer_cov_trace_pc>"),
		}
	case targets.MIPS64LE:
		// ffffffff80100420:       jal     ffffffff80205880 <__sanitizer_cov_trace_pc>
		// This is only known to occur in the test:
		// b58:   bal     b30 <__sanitizer_cov_trace_pc>
		return [][]byte{[]byte("\tjal\t"), []byte("\tbal\t")}, callName
	case targets.S390x:
		// 1001de:       brasl   %r14,2bc090 <__sanitizer_cov_trace_pc>
		return [][]byte{[]byte("\tbrasl\t")}, callName
	case targets.RiscV64:
		// ffffffe000200018:       jal     ra,ffffffe0002935b0 <__sanitizer_cov_trace_pc>
		// ffffffe0000010da:       jalr    1242(ra) # ffffffe0002935b0 <__sanitizer_cov_trace_pc>
		return [][]byte{[]byte("\tjal\t"), []byte("\tjalr\t")}, callName
	default:
		panic(fmt.Sprintf("unknown arch %q", target.Arch))
	}
}
