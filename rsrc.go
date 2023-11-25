package winres

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"sort"
	"unicode/utf16"
)

const (
	// Visual C++ pads resource data to 8 bytes.
	// 4 bytes is a minimum.
	dataAlignment = 8
)

// state is a temporary state used during the execution of ResourceSet.write()
type state struct {
	offset      int          // offset to write in the next directory entry
	relocAddr   []int        // addresses to add to the relocation table (addresses of the data entries)
	nameOffset  map[Name]int // offset of every name in the names string table
	namesData   []uint16     // final data for the names string table
	orderedKeys []Identifier // ordered keys for ResourceSet.types
	namesCount  int          // number of keys of type Name in ResourceSet.types
}

// write writes the whole rsrc section's content: the directory and the actual data.
// It returns a slice of addresses for the relocation table.
// https://docs.microsoft.com/en-us/previous-versions/ms809762(v=msdn.10)#pe-file-resources
func (rs *ResourceSet) write(w io.Writer) ([]int, error) {
	s := rs.prepare()
	if err := rs.writeTypeDir(w, s); err != nil {
		return nil, err
	}
	if err := rs.writeResDirs(w, s); err != nil {
		return nil, err
	}
	if err := rs.writeLangDirs(w, s); err != nil {
		return nil, err
	}
	// names will be inserted between the last directory level (data index) and actual data
	s.offset += len(s.namesData) * 2
	if err := rs.writeDataIndex(w, s); err != nil {
		return nil, err
	}
	if err := binary.Write(w, binary.LittleEndian, s.namesData); err != nil {
		return nil, err
	}
	if err := rs.writeData(w, s); err != nil {
		return nil, err
	}
	return append([]int{}, s.relocAddr...), nil
}

// order orders identifiers in the whole resource set.
func (rs *ResourceSet) order(s *state) {
	s.orderedKeys = make([]Identifier, 0, len(rs.Types))
	for ident, te := range rs.Types {
		s.orderedKeys = append(s.orderedKeys, ident)
		te.Order()
	}

	// Names in case sensitive ascending order, then IDs in ascending order
	sort.Slice(s.orderedKeys, func(i, j int) bool {
		return s.orderedKeys[i].lessThan(s.orderedKeys[j])
	})

	// Count Names by searching the first ID
	s.namesCount = sort.Search(len(s.orderedKeys), func(i int) bool {
		_, ok := s.orderedKeys[i].(ID)
		return ok
	})
}

// prepare calculates the names index offset and content, as this has to be known before writing the resource directory.
// It also freezes the order of identifiers in the directory, so that subsequent write functions can rely on it.
func (rs *ResourceSet) prepare() *state {
	var (
		nameSet     = make(map[Name]struct{})
		typeNameSet = make(map[Name]struct{})
		s           = &state{
			nameOffset: make(map[Name]int),
		}
	)

	rs.order(s)

	for ident, te := range rs.Types {
		if name, ok := ident.(Name); ok {
			typeNameSet[name] = struct{}{}
			nameSet[name] = struct{}{}
		}
		for _, name := range te.OrderedKeys[:te.NamesCount] {
			nameSet[name.(Name)] = struct{}{}
		}
	}

	names := make([]string, 0, len(nameSet))
	for n := range nameSet {
		names = append(names, string(n))
	}
	sort.Strings(names)

	offset := rs.dirSize()
	for _, n := range names {
		s.nameOffset[Name(n)] = offset
		u := utf16.Encode([]rune(n))
		s.namesData = append(s.namesData, uint16(len(u)))
		s.namesData = append(s.namesData, u...)
		offset += (len(u) + 1) * 2
	}

	// Names must be padded to align the resource data.
	sz := len(s.namesData) * 2
	s.namesData = append(s.namesData, make([]uint16, (alignData(sz)-sz)/2)...)
	return s
}

// writeTypeDir is where we write the resource type directory (1st level: type)
func (rs *ResourceSet) writeTypeDir(w io.Writer, s *state) error {
	if err := writeDirectoryTable(w, s.namesCount, len(s.orderedKeys)-s.namesCount); err != nil {
		return err
	}
	s.offset += sizeOfDirTable + len(s.orderedKeys)*sizeOfDirEntry
	for _, ident := range s.orderedKeys[:s.namesCount] {
		if err := writeDirectoryEntry(w, s.nameOffset[ident.(Name)], s.offset, true, true); err != nil {
			return err
		}
		s.offset += rs.Types[ident].size()
	}
	for _, ident := range s.orderedKeys[s.namesCount:] {
		if err := writeDirectoryEntry(w, int(ident.(ID)), s.offset, false, true); err != nil {
			return err
		}
		s.offset += rs.Types[ident].size()
	}
	return nil
}

// writeResDirs is where we write the resource directory (2nd level: type->resource)
func (rs *ResourceSet) writeResDirs(w io.Writer, s *state) error {
	for _, tid := range s.orderedKeys {
		if err := rs.Types[tid].write(w, s); err != nil {
			return err
		}
	}
	return nil
}

// writeLangDirs is where we write the localized resource directory (3rd level: type->resource->locale)
func (rs *ResourceSet) writeLangDirs(w io.Writer, s *state) error {
	for _, tid := range s.orderedKeys {
		te := rs.Types[tid]
		for _, ident := range te.OrderedKeys {
			if err := te.Resources[ident].write(w, s); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeDataIndex is where we write the resource data index (4th level: type->resource->locale->data address)
func (rs *ResourceSet) writeDataIndex(w io.Writer, s *state) error {
	for _, tid := range s.orderedKeys {
		te := rs.Types[tid]
		for _, rid := range te.OrderedKeys {
			re := te.Resources[rid]
			for _, lcid := range re.OrderedKeys {
				if err := re.Data[lcid].write(w, s); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// writeData is where we write the actual resource data (6th part, 5th being names)
func (rs *ResourceSet) writeData(w io.Writer, s *state) error {
	for _, tid := range s.orderedKeys {
		te := rs.Types[tid]
		for _, rid := range te.OrderedKeys {
			re := te.Resources[rid]
			for _, lcid := range re.OrderedKeys {
				if err := re.Data[lcid].writeData(w); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// fullSize returns the full size of the rsrc section's content: the directory, the names, and the actual data.
func (rs *ResourceSet) fullSize() int {
	s := rs.prepare()
	sz := rs.dirSize() + len(s.namesData)*2
	for _, te := range rs.Types {
		for _, re := range te.Resources {
			for _, de := range re.Data {
				sz += de.paddedDataSize()
			}
		}
	}
	return sz
}

// dirSize returns the size of the rsrc section's directory.
func (rs *ResourceSet) dirSize() int {
	sz := sizeOfDirTable + len(rs.Types)*sizeOfDirEntry
	for _, te := range rs.Types {
		sz += te.size()
		for _, re := range te.Resources {
			sz += re.size() + len(re.Data)*sizeOfDataEntry
		}
	}
	return sz
}

// numDataEntries returns the number of resource data entries to be written into the rsrc section.
func (rs *ResourceSet) numDataEntries() int {
	var n int
	for _, te := range rs.Types {
		for _, re := range te.Resources {
			n += len(re.Data)
		}
	}
	return n
}

type TypeEntry struct {
	Resources   map[Identifier]*ResourceEntry
	OrderedKeys []Identifier
	NamesCount  int
}

// size returns the size of the entry's directory table
func (te *TypeEntry) size() int {
	return sizeOfDirTable + len(te.Resources)*sizeOfDirEntry
}

// Order orders ids and names following the specification, and saves them in orderedKeys.
// It calls resourceEntry.Order() too, for each resource entry it owns.
func (te *TypeEntry) Order() {
	if te.OrderedKeys != nil {
		return
	}

	te.OrderedKeys = make([]Identifier, 0, len(te.Resources))
	for ident, re := range te.Resources {
		te.OrderedKeys = append(te.OrderedKeys, ident)

		// Order LCIDs in every resource
		re.order()
	}

	// Names in case sensitive ascending order, then IDs in ascending order
	sort.Slice(te.OrderedKeys, func(i, j int) bool {
		return te.OrderedKeys[i].lessThan(te.OrderedKeys[j])
	})

	// Count Names by searching the first ID
	te.NamesCount = sort.Search(len(te.OrderedKeys), func(i int) bool {
		_, ok := te.OrderedKeys[i].(ID)
		return ok
	})
}

// write writes the entry's directory table
func (te *TypeEntry) write(w io.Writer, s *state) error {
	if err := writeDirectoryTable(w, te.NamesCount, len(te.OrderedKeys)-te.NamesCount); err != nil {
		return err
	}
	for _, ident := range te.OrderedKeys[:te.NamesCount] {
		if err := writeDirectoryEntry(w, s.nameOffset[ident.(Name)], s.offset, true, true); err != nil {
			return err
		}
		s.offset += te.Resources[ident].size()
	}
	for _, ident := range te.OrderedKeys[te.NamesCount:] {
		if err := writeDirectoryEntry(w, int(ident.(ID)), s.offset, false, true); err != nil {
			return err
		}
		s.offset += te.Resources[ident].size()
	}
	return nil
}

type ResourceEntry struct {
	Data        map[ID]*DataEntry
	OrderedKeys []ID
}

// size returns the size of the entry's directory table
func (re *ResourceEntry) size() int {
	return sizeOfDirTable + len(re.Data)*sizeOfDirEntry
}

// order orders LCIDs and saves them in orderedKeys.
func (re *ResourceEntry) order() {
	if re.OrderedKeys != nil {
		return
	}

	re.OrderedKeys = make([]ID, 0, len(re.Data))
	for id := range re.Data {
		re.OrderedKeys = append(re.OrderedKeys, id)
	}

	// LCIDs in ascending order
	sort.Slice(re.OrderedKeys, func(i, j int) bool {
		return re.OrderedKeys[i].lessThan(re.OrderedKeys[j])
	})
}

// write writes the entry's directory table
func (re *ResourceEntry) write(w io.Writer, s *state) error {
	if err := writeDirectoryTable(w, 0, len(re.Data)); err != nil {
		return err
	}
	for _, lcid := range re.OrderedKeys {
		s.relocAddr = append(s.relocAddr, s.offset)
		if err := writeDirectoryEntry(w, int(lcid), s.offset, false, false); err != nil {
			return err
		}
		s.offset += sizeOfDataEntry
	}
	return nil
}

type DataEntry struct {
	Data []byte
}

func alignData(offset int) int {
	return (offset + dataAlignment - 1) &^ (dataAlignment - 1)
}

// paddedDataSize returns the room taken by data, including some padding at the end.
func (de *DataEntry) paddedDataSize() int {
	return alignData(len(de.Data))
}

func (de *DataEntry) write(w io.Writer, s *state) error {
	if err := writeDataEntry(w, s.offset, len(de.Data)); err != nil {
		return err
	}
	// Everything must be aligned, so we may skip a few byte when necessary after each resource data
	s.offset += de.paddedDataSize()
	return nil
}

func (de *DataEntry) writeData(w io.Writer) error {
	size, err := w.Write(de.Data)
	if err != nil {
		return err
	}
	// Everything must be aligned, so we may skip a few byte when necessary after each resource data
	b := [dataAlignment]byte{}
	_, err = w.Write(b[:de.paddedDataSize()-size])
	return err
}

// Binary format of the directory :

// https://docs.microsoft.com/en-us/windows/win32/debug/pe-format#the-rsrc-section
// https://docs.microsoft.com/en-us/previous-versions/ms809762(v=msdn.10)#pe-file-resources

type resourceDirectoryTable struct {
	Characteristics     uint32
	TimeDateStamp       uint32
	MajorVersion        uint16
	MinorVersion        uint16
	NumberOfNameEntries uint16
	NumberOfIDEntries   uint16
}

const sizeOfDirTable = 16

func writeDirectoryTable(w io.Writer, numNameEntries int, numIDEntries int) error {
	return binary.Write(w, binary.LittleEndian, resourceDirectoryTable{
		NumberOfNameEntries: uint16(numNameEntries),
		NumberOfIDEntries:   uint16(numIDEntries),
	})
}

type resourceDirectoryIDEntry struct {
	ID     uint32
	Offset uint32
}

const sizeOfDirEntry = 8

const subdirectoryOffset = 0x80000000
const nameOffset = 0x80000000

func writeDirectoryEntry(w io.Writer, id int, offset int, isName bool, isSubDir bool) error {
	e := resourceDirectoryIDEntry{
		ID:     uint32(id),
		Offset: uint32(offset),
	}
	if isSubDir {
		e.Offset |= subdirectoryOffset
	}
	if isName {
		e.ID |= nameOffset
	}
	return binary.Write(w, binary.LittleEndian, &e)
}

type resourceDataEntry struct {
	DataRVA  uint32
	Size     uint32
	Codepage uint32
	Reserved uint32
}

const sizeOfDataEntry = 16

func writeDataEntry(w io.Writer, offset int, dataSize int) error {
	return binary.Write(w, binary.LittleEndian, resourceDataEntry{
		DataRVA:  uint32(offset),
		Size:     uint32(dataSize),
		Codepage: uint32(0), // String tables being encoded in utf-16, this field is useless. Visual studio sets it to zero.
	})
}

// Reading functions:

func (rs *ResourceSet) read(section []byte, baseAddress uint32, typeID Identifier) error {
	r := bytes.NewReader(section)
	return dirEntry{}.walk(r, func(typeEntry dirEntry) error {
		if typeID != ID(0) &&
			typeEntry.ident != typeID &&
			(typeEntry.ident != RT_ICON || typeID != RT_GROUP_ICON) &&
			(typeEntry.ident != RT_CURSOR || typeID != RT_GROUP_CURSOR) {
			return nil
		}
		return typeEntry.walk(r, func(resourceEntry dirEntry) error {
			resourceEntry.leaf = true
			return resourceEntry.walk(r, func(langEntry dirEntry) error {
				data, err := langEntry.readData(r, baseAddress)
				if err != nil {
					return err
				}
				return rs.Set(typeEntry.ident, resourceEntry.ident, uint16(langEntry.ident.(ID)), data)
			})
		})
	})
}

type dirEntry struct {
	path   string
	ident  Identifier
	offset int64
	leaf   bool
}

func (de dirEntry) walk(section *bytes.Reader, fn func(dirEntry) error) error {
	entries, err := de.readDirTable(section)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		err = fn(entry)
		if err != nil {
			return err
		}
	}
	return nil
}

func (de dirEntry) readData(section *bytes.Reader, baseAddress uint32) ([]byte, error) {
	section.Seek(de.offset, io.SeekStart)

	entry := resourceDataEntry{}
	err := binaryRead(section, &entry)
	if err != nil {
		return nil, err
	}

	offset := int64(entry.DataRVA) - int64(baseAddress)
	if offset < 0 || offset+int64(entry.Size) > section.Size() {
		return nil, errors.New(errDataEntryOutOfBounds)
	}

	data := make([]byte, entry.Size)
	section.Seek(offset, io.SeekStart)
	section.Read(data)

	return data, nil
}

func (de dirEntry) readDirTable(section *bytes.Reader) ([]dirEntry, error) {
	section.Seek(de.offset, io.SeekStart)

	table := resourceDirectoryTable{}
	err := binaryRead(section, &table)
	if err != nil {
		return nil, err
	}

	dir := make([]resourceDirectoryIDEntry, table.NumberOfNameEntries+table.NumberOfIDEntries)
	err = binaryRead(section, &dir)
	if err != nil {
		return nil, err
	}

	entries := make([]dirEntry, len(dir))
	for i := range dir {
		if de.leaf != (dir[i].Offset&subdirectoryOffset == 0) {
			return nil, errors.New(errInvalidResDir)
		}
		if de.leaf && dir[i].ID&nameOffset != 0 {
			return nil, errors.New(errInvalidResDir)
		}
		entries[i].offset = int64(dir[i].Offset &^ subdirectoryOffset)
		if dir[i].ID&nameOffset == 0 {
			entries[i].ident = ID(dir[i].ID)
		} else {
			entries[i].ident, err = readName(section, dir[i].ID&^nameOffset)
			if err != nil {
				return nil, err
			}
		}
	}

	return entries, nil
}

func readName(section *bytes.Reader, offset uint32) (Name, error) {
	section.Seek(int64(offset), io.SeekStart)

	var length uint16
	err := binaryRead(section, &length)
	if err != nil {
		return "", err
	}

	b := make([]uint16, length)
	err = binaryRead(section, b)
	if err != nil {
		return "", err
	}

	return Name(utf16.Decode(b)), nil
}
