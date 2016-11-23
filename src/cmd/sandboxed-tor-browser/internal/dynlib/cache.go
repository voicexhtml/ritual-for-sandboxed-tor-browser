// cache.go - Dynamic linker cache routines.
// Copyright (C) 2016  Yawning Angel.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

// Package dynlib provides routines for interacting with the glibc ld.so dynamic
// linker/loader.
package dynlib

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"log"
	"path/filepath"
	"runtime"

	"cmd/sandboxed-tor-browser/internal/utils"
)

const (
	ldSoCache = "/etc/ld.so.cache"

	flagX8664Lib64 = 0x0300
	flagElf        = 1
	flagElfLibc6   = 3
)

// Quoting from sysdeps/generic/dl-cache.h:
//
// libc5 and glibc 2.0/2.1 use the same format.  For glibc 2.2 another
// format has been added in a compatible way:
// The beginning of the string table is used for the new table:
//   old_magic
//   nlibs
//   libs[0]
//   ...
//   libs[nlibs-1]
//   pad, new magic needs to be aligned
//	     - this is string[0] for the old format
//   new magic - this is string[0] for the new format
//   newnlibs
//   ...
//   newlibs[0]
//   ...
//   newlibs[newnlibs-1]
//   string 1
//   string 2
//   ...

// Cache is a representation of the `ld.so.cache` file.
type Cache struct {
	store map[string][]*cacheEntry
}

// GetLibraryPath returns the path to the given library, if any.  This routine
// makes no attempt to disambiguate multiple libraries (eg: via hwcap/search
// path).
func (c *Cache) GetLibraryPath(name string) string {
	ents, ok := c.store[name]
	if !ok {
		return ""
	}

	return ents[0].value
}

// ResolveLibraries returns a map of library paths and their aliases for a
// given set of binaries, based off the ld.so.cache, libraries known to be
// internal, and a search path.
func (c *Cache) ResolveLibraries(binaries []string, extraLibs []string, ldLibraryPath string) (map[string][]string, error) {
	searchPaths := filepath.SplitList(ldLibraryPath)
	libraries := make(map[string]string)

	// Breadth-first iteration of all the binaries, and their dependencies.
	checkedFile := make(map[string]bool)
	checkedLib := make(map[string]bool)
	toCheck := binaries
	for {
		newToCheck := make(map[string]bool)
		if len(toCheck) == 0 {
			break
		}
		for _, fn := range toCheck {
			impLibs, err := GetLibraries(fn)
			if err != nil {
				return nil, err
			}
			log.Printf("dynlib: %v imports: %v", fn, impLibs)
			checkedFile[fn] = true

			// The internal libraries also need recursive resolution,
			// so just append them to the first binary.
			if extraLibs != nil {
				log.Printf("dynlib: Appending extra libs: %v", extraLibs)
				impLibs = append(impLibs, extraLibs...)
				extraLibs = nil
			}

			for _, lib := range impLibs {
				if checkedLib[lib] {
					continue
				}

				// Look for the library in the search path.
				libPath := ""
				inLdLibraryPath := false
				for _, d := range searchPaths {
					maybePath := filepath.Join(d, lib)
					if utils.FileExists(maybePath) {
						libPath = maybePath
						inLdLibraryPath = true
						break
					}
				}

				// Look for the library in the ld.so.cache.
				if libPath == "" {
					// XXX; Figure out how to disambiguate libraries, most
					// likely by examining c.store directly instead of via
					// the public interface.
					//
					// ld-linux apparently goes by hwcap, osVersion, search
					// path (ld.so.conf based -> internal).
					libPath = c.GetLibraryPath(lib)
					if libPath == "" {
						return nil, fmt.Errorf("dynlib: Failed to find library: %v", lib)
					}
				}

				// Register the library, assuming it's not in what will
				// presumably be `LD_LIBRARY_PATH` inside the hugbox.
				if !inLdLibraryPath {
					libraries[lib] = libPath
				}
				checkedLib[lib] = true

				if !checkedFile[libPath] {
					newToCheck[libPath] = true
				}
			}
		}
		toCheck = []string{}
		for k, _ := range newToCheck {
			toCheck = append(toCheck, k)
		}
	}

	// De-dup the libraries map by figuring out what can be symlinked.
	ret := make(map[string][]string)
	for lib, fn := range libraries {
		f, err := filepath.EvalSymlinks(fn)
		if err != nil {
			return nil, err
		}

		vec := ret[f]
		vec = append(vec, lib)
		ret[f] = vec
	}

	// XXX: This should sanity check to ensure that aliases are distinct.

	return ret, nil
}

type cacheEntry struct {
	key, value string
	flags      uint32
	osVersion  uint32
	hwcap      uint64
}

func getNewLdCache(b []byte) ([]byte, int, error) {
	const entrySz = 4 + 4 + 4

	// The new format is embedded in the old format, so do some light
	// parsing/validation to get to the new format's header.
	cacheMagic := []byte{
		'l', 'd', '.', 's', 'o', '-', '1', '.', '7', '.', '0', 0,
	}

	// old_magic
	if !bytes.HasPrefix(b, cacheMagic) {
		return nil, 0, fmt.Errorf("dynlib: ld.so.cache has invalid old_magic")
	}
	off := len(cacheMagic)
	b = b[off:]

	// nlibs
	if len(b) < 4 {
		return nil, 0, fmt.Errorf("dynlib: ld.so.cache truncated (nlibs)")
	}
	nlibs := int(binary.LittleEndian.Uint32(b))
	off += 4
	b = b[4:]

	// libs[nlibs]
	nSkip := entrySz * nlibs
	if len(b) < nSkip {
		return nil, 0, fmt.Errorf("dynlib: ld.so.cache truncated (libs[])")
	}
	off += nSkip
	b = b[nSkip:]

	// new_magic is 8 byte aligned.
	padLen := (((off+8-1)/8)*8 - off)
	if len(b) < padLen {
		return nil, 0, fmt.Errorf("dynlib: ld.so.cache truncated (pad)")
	}
	return b[padLen:], nlibs, nil
}

// LoadCache loads and parses the `ld.so.cache` file.
//
// See `sysdeps/generic/dl-cache.h` in the glibc source tree for details
// regarding the format.
func LoadCache() (*Cache, error) {
	const entrySz = 4 + 4 + 4 + 4 + 8

	if !IsSupported() {
		return nil, errUnsupported
	}

	c := new(Cache)
	c.store = make(map[string][]*cacheEntry)

	b, err := ioutil.ReadFile(ldSoCache)
	if err != nil {
		return nil, err
	}

	// It is likely safe to assume that everyone is running glibc >= 2.2 at
	// this point, so extract the "new format" from the "old format".
	b, _, err = getNewLdCache(b)
	if err != nil {
		return nil, err
	}
	stringTable := b

	// new_magic.
	cacheMagicNew := []byte{
		'g', 'l', 'i', 'b', 'c', '-', 'l', 'd', '.', 's', 'o', '.', 'c', 'a', 'c',
		'h', 'e', '1', '.', '1',
	}
	if !bytes.HasPrefix(b, cacheMagicNew) {
		return nil, fmt.Errorf("dynlib: ld.so.cache has invalid new_magic")
	}
	b = b[len(cacheMagicNew):]

	// nlibs, len_strings, unused[].
	if len(b) < 2*4+5*4 {
		return nil, fmt.Errorf("dynlib: ld.so.cache truncated (new header)")
	}
	nlibs := int(binary.LittleEndian.Uint32(b))
	b = b[4:]
	lenStrings := int(binary.LittleEndian.Uint32(b))
	b = b[4+20:] // Also skip unused[].
	rawLibs := b[:nlibs*entrySz]
	b = b[len(rawLibs):]
	if len(b) != lenStrings {
		return nil, fmt.Errorf("dynlib: lenStrings appears invalid")
	}

	getString := func(idx int) (string, error) {
		if idx < 0 || idx > len(stringTable) {
			return "", fmt.Errorf("dynlib: string table index out of bounds")
		}
		l := bytes.IndexByte(stringTable[idx:], 0)
		if l == 0 {
			return "", nil
		}
		return string(stringTable[idx : idx+l]), nil
	}

	// libs[]
	var flagCheckFn func(uint32) bool
	var capCheckFn func(uint64) bool
	switch runtime.GOARCH {
	case "amd64":
		flagCheckFn = func(flags uint32) bool {
			const wantFlags = flagX8664Lib64 | flagElfLibc6
			return flags&wantFlags == flags
		}
		capCheckFn = func(hwcap uint64) bool {
			// Not used on this arch AFAIK.
			return true
		}
	default: // XXX: Figure out 386.  Probably also need to look at hwcap there.
		panic(errUnsupported)
	}

	for i := 0; i < nlibs; i++ {
		rawE := rawLibs[entrySz*i : entrySz*(i+1)]

		e := new(cacheEntry)
		e.flags = binary.LittleEndian.Uint32(rawE[0:])
		kIdx := int(binary.LittleEndian.Uint32(rawE[4:]))
		vIdx := int(binary.LittleEndian.Uint32(rawE[8:]))
		e.osVersion = binary.LittleEndian.Uint32(rawE[12:])
		e.hwcap = binary.LittleEndian.Uint64(rawE[16:])

		e.key, err = getString(kIdx)
		if err != nil {
			return nil, fmt.Errorf("dynlib: failed to query key: %v", err)
		}
		e.value, err = getString(vIdx)
		if err != nil {
			return nil, fmt.Errorf("dynlib: failed to query value: %v", err)
		}

		if flagCheckFn(e.flags) && capCheckFn(e.hwcap) {
			vec := c.store[e.key]
			vec = append(vec, e)
			c.store[e.key] = vec
		} else {
			log.Printf("dynlib: ignoring library: %v (flags: %x, hwcap: %x)", e.key, e.flags, e.hwcap)
		}
	}

	// For debugging purposes dump the ambiguous entries.  It would be nice if
	// we could disambiguate these somehow, but as far as I can tell this is
	// actually fairly rare, and doesn't directly affect any libraries we
	// currently care about.
	for lib, entries := range c.store {
		if len(entries) == 1 {
			continue
		}
		paths := []string{}
		for _, e := range entries {
			paths = append(paths, e.value)
		}

		log.Printf("dynlib: debug: Ambiguous entry: %v: %v", lib, paths)
	}

	return c, nil
}
