// PebbleDB là lớp bọc tuân thủ giao diện dbm.DB của CometBFT.


package store

import (
	"fmt"
	"sync"

	"github.com/cockroachdb/pebble"
	dbm "github.com/cometbft/cometbft-db"
)

// PebbleDB cài đặt giao diện dbm.DB.
type PebbleDB struct {
	db *pebble.DB
}

// NewPebbleDB khởi tạo PebbleDB.
func NewPebbleDB(name string, dir string) (*PebbleDB, error) {
	dbPath := fmt.Sprintf("%s/%s.db", dir, name)
	
	// Tối ưu ghi blockchain
	opts := &pebble.Options{
		Cache:        pebble.NewCache(512 << 20), // Bộ đệm 512 MiB.
		MemTableSize: 64 << 20,                   // Bảng ghi nhớ 64 MiB.
	}

	db, err := pebble.Open(dbPath, opts)
	if err != nil {
		return nil, err
	}
	return &PebbleDB{db: db}, nil
}

// Get đọc giá trị của khóa.
func (pdb *PebbleDB) Get(key []byte) ([]byte, error) {
	val, closer, err := pdb.db.Get(key)
	if err == pebble.ErrNotFound {
		return nil, nil // Trả về nil nếu không tìm thấy theo quy định CometBFT.
	}
	if err != nil {
		return nil, err
	}
	// Sao chép dữ liệu trước khi đóng.
	ret := make([]byte, len(val))
	copy(ret, val)
	closer.Close()
	return ret, nil
}

// Has kiểm tra khóa có tồn tại hay không.
func (pdb *PebbleDB) Has(key []byte) (bool, error) {
	_, closer, err := pdb.db.Get(key)
	if err == pebble.ErrNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	closer.Close()
	return true, nil
}

// Set ghi một cặp khóa-giá trị.
func (pdb *PebbleDB) Set(key, value []byte) error {
	return pdb.db.Set(key, value, pebble.Sync)
}

// SetSync ghi đồng bộ.
func (pdb *PebbleDB) SetSync(key, value []byte) error {
	return pdb.db.Set(key, value, pebble.Sync)
}

// Delete xóa khóa.
func (pdb *PebbleDB) Delete(key []byte) error {
	return pdb.db.Delete(key, pebble.Sync)
}

// DeleteSync xóa khóa và chờ ghi đồng bộ hoàn tất.
func (pdb *PebbleDB) DeleteSync(key []byte) error {
	return pdb.db.Delete(key, pebble.Sync)
}

// Close đóng cơ sở dữ liệu.
func (pdb *PebbleDB) Close() error {
	return pdb.db.Close()
}

// NewBatch tạo một lô thao tác.
func (pdb *PebbleDB) NewBatch() dbm.Batch {
	return &PebbleBatch{batch: pdb.db.NewBatch()}
}

// Print xuất các số liệu đo.
func (pdb *PebbleDB) Print() error {
	fmt.Println(pdb.db.Metrics().String())
	return nil
}

// Stats trả về các số liệu đo.
func (pdb *PebbleDB) Stats() map[string]string {
	return map[string]string{
		"metrics": pdb.db.Metrics().String(),
	}
}

// Iterator duyệt khóa theo thứ tự tăng dần.
func (pdb *PebbleDB) Iterator(start, end []byte) (dbm.Iterator, error) {
	iter, err := pdb.db.NewIter(&pebble.IterOptions{
		LowerBound: start,
		UpperBound: end,
	})
	if err != nil {
		return nil, err
	}
	iter.First()
	return &pebbleIterator{iter: iter, start: start, end: end, isReverse: false, isInvalid: false}, nil
}

// ReverseIterator duyệt khóa theo thứ tự giảm dần.
func (pdb *PebbleDB) ReverseIterator(start, end []byte) (dbm.Iterator, error) {
	iter, err := pdb.db.NewIter(&pebble.IterOptions{
		LowerBound: start,
		UpperBound: end,
	})
	if err != nil {
		return nil, err
	}
	iter.Last()
	return &pebbleIterator{iter: iter, start: start, end: end, isReverse: true, isInvalid: false}, nil
}

// Compact dọn dẹp cơ sở dữ liệu.
func (pdb *PebbleDB) Compact(start, end []byte) error {
	return pdb.db.Compact(start, end, true)
}

// Iterator biểu diễn bộ duyệt các khóa.

type pebbleIterator struct {
	iter      *pebble.Iterator
	start     []byte
	end       []byte
	isReverse bool
	isInvalid bool
}

// Domain trả về giới hạn [start, end).
func (itr *pebbleIterator) Domain() ([]byte, []byte) {
	return itr.start, itr.end
}

// Valid kiểm tra bộ duyệt còn hợp lệ hay không.
func (itr *pebbleIterator) Valid() bool {
	if itr.isInvalid {
		return false
	}
	return itr.iter.Valid()
}

// Next chuyển sang khóa tiếp theo.
func (itr *pebbleIterator) Next() {
	if itr.isInvalid {
		return
	}
	if itr.isReverse {
		itr.iter.Prev()
	} else {
		itr.iter.Next()
	}
}

// Key trả về khóa hiện tại.
func (itr *pebbleIterator) Key() []byte {
	if !itr.Valid() {
		return nil
	}
	key := itr.iter.Key()
	// Sao chép khóa vì khóa chỉ còn hợp lệ đến lần gọi tiếp theo.
	keyCopy := make([]byte, len(key))
	copy(keyCopy, key)
	return keyCopy
}

// Value trả về giá trị hiện tại.
func (itr *pebbleIterator) Value() []byte {
	if !itr.Valid() {
		return nil
	}
	val := itr.iter.Value()
	// Sao chép giá trị vì nó chỉ còn hợp lệ đến lần gọi tiếp theo.
	valCopy := make([]byte, len(val))
	copy(valCopy, val)
	return valCopy
}

// Error trả về lỗi nếu có.
func (itr *pebbleIterator) Error() error {
	return itr.iter.Error()
}

// Close đóng bộ duyệt.
func (itr *pebbleIterator) Close() error {
	itr.isInvalid = true
	return itr.iter.Close()
}

// Batch đại diện cho một lô thao tác.

type PebbleBatch struct {
	batch *pebble.Batch
	sync.Mutex
}

func (b *PebbleBatch) Set(key, value []byte) error {
	return b.batch.Set(key, value, nil)
}

func (b *PebbleBatch) Delete(key []byte) error {
	return b.batch.Delete(key, nil)
}

func (b *PebbleBatch) Write() error {
	return b.batch.Commit(pebble.Sync)
}

func (b *PebbleBatch) WriteSync() error {
	return b.batch.Commit(pebble.Sync)
}

func (b *PebbleBatch) Close() error {
	return b.batch.Close()
}
