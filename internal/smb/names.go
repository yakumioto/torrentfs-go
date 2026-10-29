package smb

import (
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf16"

	"github.com/yakumioto/torrentfs-go/internal/vfs"
)

type nameIndex struct {
	version uint64
	entries []vfs.Entry
	byName  map[string]vfs.Entry
}

func (f *filesystem) nameIndex(directory *vfs.Directory) *nameIndex {
	identity, version := directory.Metadata().Identity, directory.Version()
	f.namesMu.Lock()
	if index := f.indexes[identity]; index != nil && index.version == version {
		f.namesMu.Unlock()
		return index
	}
	f.namesMu.Unlock()
	entries := smbNames(directory.Entries())
	built := &nameIndex{version: version, entries: entries, byName: make(map[string]vfs.Entry, len(entries))}
	for _, entry := range entries {
		built.byName[foldName(entry.Name)] = entry
	}
	// Subscribe outside namesMu: an already-evicted (or temporary root)
	// snapshot invokes cleanup immediately. The flag also closes the race
	// between that callback and publication of this freshly built index.
	var evicted atomic.Bool
	directory.OnCacheEviction(func() {
		evicted.Store(true)
		f.namesMu.Lock()
		if current := f.indexes[identity]; current != nil && current.version == version {
			delete(f.indexes, identity)
		}
		f.namesMu.Unlock()
	})
	f.namesMu.Lock()
	defer f.namesMu.Unlock()
	if current := f.indexes[identity]; current != nil {
		if current.version == version {
			return current
		}
		if current.version > version {
			return built
		}
	}
	if evicted.Load() {
		return built
	}
	if f.indexes == nil {
		f.indexes = make(map[uint64]*nameIndex)
	}
	f.indexes[identity] = built
	return built
}

func pathParts(raw string) ([]string, error) {
	if strings.ContainsRune(raw, 0) || strings.ContainsAny(raw, `:<>"|?*`) {
		return nil, fs.ErrInvalid
	}
	// A share-relative root separator is legal; an embedded UNC host or a
	// parent component is not. Do not clean a traversal into a different file.
	raw = strings.ReplaceAll(raw, `\`, "/")
	if strings.HasPrefix(raw, "//") {
		return nil, fs.ErrInvalid
	}
	var parts []string
	for _, part := range strings.Split(raw, "/") {
		switch part {
		case "", ".":
			continue
		case "..":
			return nil, fs.ErrInvalid
		default:
			parts = append(parts, part)
		}
	}
	return parts, nil
}

// foldName implements Unicode simple case folding rather than lowercasing:
// e.g. the long s and ASCII s must not alias two different SMB objects.
func foldName(name string) string {
	var out strings.Builder
	for _, r := range name {
		least := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < least {
				least = next
			}
		}
		out.WriteRune(least)
	}
	return out.String()
}

// smbNames assigns portable, deterministic names without changing the virtual
// identity or the original path used to open an object. Literal percent signs
// are escaped too, so a payload named a%3Ab cannot impersonate a:b.
func smbNames(entries []vfs.Entry) []vfs.Entry {
	out := append([]vfs.Entry(nil), entries...)
	counts := make(map[string]int, len(out))
	reserved := make(map[string]bool, len(out))
	for i := range out {
		out[i].Name = portableName(out[i].Name)
		key := foldName(out[i].Name)
		counts[key]++
		reserved[key] = true
	}
	used := make(map[string]bool, len(out))
	for i := range out {
		base := out[i].Name
		name := base
		if counts[foldName(base)] > 1 || utf16Length(base) > 255 {
			for salt := 0; ; salt++ {
				suffix := fmt.Sprintf("~%016x", out[i].Identity)
				if salt > 0 {
					suffix += fmt.Sprintf("-%d", salt)
				}
				name = suffixedName(base, suffix)
				key := foldName(name)
				if !reserved[key] && !used[key] {
					break
				}
			}
		}
		out[i].Name = name
		used[foldName(name)] = true
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := foldName(out[i].Name), foldName(out[j].Name)
		if a != b {
			return a < b
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func portableName(name string) string {
	var out strings.Builder
	end := len(strings.TrimRight(name, ". "))
	for index, r := range name {
		if unicode.IsControl(r) || strings.ContainsRune(`%\/:<>"|?*`, r) ||
			(index >= end && (r == '.' || r == ' ')) {
			out.WriteString(fmt.Sprintf("%%%02X", r))
		} else {
			out.WriteRune(r)
		}
	}
	result := out.String()
	stem, _, _ := strings.Cut(strings.ToUpper(result), ".")
	reserved := stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" ||
		(len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9')
	if reserved {
		// Escape the first ASCII character; an existing literal % is already
		// escaped above, keeping this transformation unambiguous.
		result = fmt.Sprintf("%%%02X%s", result[0], result[1:])
	}
	return result
}

// Keep media and subtitle extensions recognizable after disambiguation. ISO
// STRM names have a meaningful two-part ending which must stay intact too.
func suffixedName(base, suffix string) string {
	ending := ""
	if strings.HasSuffix(strings.ToLower(base), ".iso.strm") {
		ending = base[len(base)-len(".iso.strm"):]
	} else if dot := strings.LastIndexByte(base, '.'); dot >= 0 {
		ending = base[dot:]
	}
	if utf16Length(ending) > 64 {
		ending = ""
	}
	stem := strings.TrimSuffix(base, ending)
	return truncateName(stem, 255-utf16Length(suffix)-utf16Length(ending)) + suffix + ending
}

func utf16Length(name string) int {
	length := 0
	for _, r := range name {
		length += utf16.RuneLen(r)
	}
	return length
}

func truncateName(name string, maxUnits int) string {
	units := 0
	for index, r := range name {
		units += utf16.RuneLen(r)
		if units > maxUnits {
			return name[:index]
		}
	}
	return name
}

func wildcardMatch(pattern, name string) bool {
	if pattern == "" || pattern == "*" || pattern == "*.*" {
		return true
	}
	// DOS wildcard spellings used by SMB clients. Other characters, including
	// square brackets, remain literals rather than becoming glob syntax.
	pattern = strings.NewReplacer("<", "*", ">", "?", `"`, ".").Replace(pattern)
	p, n := []rune(foldName(pattern)), []rune(foldName(name))
	pi, ni, star, retry := 0, 0, -1, 0
	for ni < len(n) {
		if pi < len(p) && (p[pi] == '?' || p[pi] == n[ni]) {
			pi++
			ni++
		} else if pi < len(p) && p[pi] == '*' {
			star = pi
			pi++
			retry = ni
		} else if star >= 0 {
			retry++
			ni = retry
			pi = star + 1
		} else {
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}
