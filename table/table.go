package table

import (
	"bytes"
	"encoding/binary"
	//	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/dgraph-io/badger/y"
	"github.com/pkg/errors"
)

type keyOffset struct {
	key    []byte
	offset int64
	len    int64
}

type Table struct {
	sync.Mutex

	offset    int64
	fd        *os.File // Do not own.
	tableSize int64    // Initialized in OpenTable, using fd.Stat().

	blockIndex []keyOffset
}

type Block struct {
	offset int64
	data   []byte
}

func (b Block) NewIterator() *BlockIterator {
	return &BlockIterator{data: b.data}
}

type byKey []keyOffset

func (b byKey) Len() int               { return len(b) }
func (b byKey) Swap(i int, j int)      { b[i], b[j] = b[j], b[i] }
func (b byKey) Less(i int, j int) bool { return bytes.Compare(b[i].key, b[j].key) < 0 }

// OpenTable assumes file has only one table and opens it.
func OpenTable(fd *os.File) (*Table, error) {
	t := &Table{
		fd:     fd,
		offset: 0, // Consider removing this field.
	}
	fileInfo, err := fd.Stat()
	if err != nil {
		return nil, err
	}
	t.tableSize = fileInfo.Size()

	if err := t.readIndex(); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *Table) readIndex() error {
	buf := make([]byte, 4)
	if _, err := t.fd.ReadAt(buf, t.offset+t.tableSize-4); err != nil {
		return errors.Wrap(err, "While reading block index")
	}
	restartsLen := int(binary.BigEndian.Uint32(buf))

	buf = make([]byte, 4*restartsLen)
	if _, err := t.fd.ReadAt(buf, t.offset+t.tableSize-4-int64(len(buf))); err != nil {
		return errors.Wrap(err, "While reading block index")
	}

	offsets := make([]uint32, restartsLen)
	for i := 0; i < restartsLen; i++ {
		offsets[i] = binary.BigEndian.Uint32(buf[:4])
		buf = buf[4:]
	}

	// The last offset stores the end of the last block.
	for i := 0; i < len(offsets); i++ {
		var o int64
		if i == 0 {
			o = 0
		} else {
			o = int64(offsets[i-1])
		}

		ko := keyOffset{
			offset: o,
			len:    int64(offsets[i]) - o,
		}
		t.blockIndex = append(t.blockIndex, ko)
	}

	if len(t.blockIndex) == 1 {
		return nil
	}

	che := make(chan error, len(t.blockIndex))
	for i := 0; i < len(t.blockIndex); i++ {

		bo := &t.blockIndex[i]
		go func(ko *keyOffset) {
			var h header

			offset := t.offset + ko.offset
			buf := make([]byte, h.Size())
			if _, err := t.fd.ReadAt(buf, offset); err != nil {
				che <- errors.Wrap(err, "While reading first header in block")
				return
			}

			h.Decode(buf)
			y.AssertTrue(h.plen == 0)

			offset += int64(h.Size())
			buf = make([]byte, h.klen)
			if _, err := t.fd.ReadAt(buf, offset); err != nil {
				che <- errors.Wrap(err, "While reading first key in block")
				return
			}

			ko.key = buf
			che <- nil
		}(bo)
	}

	for _ = range t.blockIndex {
		err := <-che
		if err != nil {
			return err
		}
	}
	sort.Sort(byKey(t.blockIndex))

	return nil
}

// blockIndexFor finds the block that might contain k.
//func (t *Table) blockIndexFor(k []byte) int {
//	idx := sort.Search(len(t.blockIndex), func(idx int) bool {
//		ko := t.blockIndex[idx]
//		return bytes.Compare(ko.key, k) > 0
//	})
//	// k cannot be in block[idx] because block[idx].smallest is strictly > k.
//	// If k is in the table, it would have to be in block[idx-1].
//	// If idx=0, then table does not contain k.
//	fmt.Printf("~~~blockIndexFor: k=%v idx=%d\n", k, idx)

//	if idx > 0 {
//		idx--
//	}

//	ko := t.blockIndex[idx]
//	if bytes.Compare(k, ko.key) < 0 {
//		return -1
//	}
//	return idx
//}

func (t *Table) block(idx int) (Block, error) {
	if idx >= len(t.blockIndex) {
		return Block{}, errors.New("Block out of index.")
	}

	ko := t.blockIndex[idx]

	// TODO: add Block caching here.
	block := Block{
		offset: ko.offset + t.offset,
		data:   make([]byte, int(ko.len)),
	}
	if _, err := t.fd.ReadAt(block.data, block.offset); err != nil {
		return block, err
	}
	return block, nil
}

//func (t *Table) BlockForKey(k []byte) (Block, error) {
//	idx := t.blockIndexFor(k)
//	if idx == -1 {
//		return Block{}, errors.New("No Block found")
//	}
//	return t.block(idx)
//}

func (t *Table) NewIterator() *TableIterator {
	return &TableIterator{t: t}
}