package vse

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	"go.etcd.io/bbolt"
)

var (
	bucketVectors   = []byte("vectors")
	bucketSegments  = []byte("segments")
	bucketIDGen     = []byte("idgen")
	bucketStats     = []byte("stats")
)

type VectorRecord struct {
	ID        VectorID `json:"id"`
	ExtID     string   `json:"ext_id"`
	Segment   SegmentID `json:"segment"`
	Offset    int      `json:"offset"`
	Dimension int      `json:"dim"`
	Bucket    string   `json:"bucket"`
	ObjectKey string   `json:"object_key"`
	Metadata  map[string]string `json:"metadata"`
	CreatedAt time.Time `json:"created_at"`
}

type segmentRecord struct {
	Meta SegmentMeta `json:"meta"`
	CreatedAt time.Time `json:"created_at"`
}

type BoltStore struct {
	db *bbolt.DB
}

func NewBoltStore(path string) (*BoltStore, error) {
	db, err := bbolt.Open(path, 0644, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("boltdb open %s: %w", path, err)
	}

	err = db.Update(func(tx *bbolt.Tx) error {
		for _, b := range [][]byte{bucketVectors, bucketSegments, bucketIDGen, bucketStats} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return fmt.Errorf("create bucket %s: %w", b, err)
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}

	return &BoltStore{db: db}, nil
}

func (s *BoltStore) PutVector(rec *VectorRecord) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketVectors)
		key := make([]byte, 8)
		binary.LittleEndian.PutUint64(key, uint64(rec.ID))
		data, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return b.Put(key, data)
	})
}

func (s *BoltStore) GetVector(id VectorID) (*VectorRecord, error) {
	var rec *VectorRecord
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketVectors)
		key := make([]byte, 8)
		binary.LittleEndian.PutUint64(key, uint64(id))
		data := b.Get(key)
		if data == nil {
			return nil
		}
		var r VectorRecord
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		rec = &r
		return nil
	})
	return rec, err
}

func (s *BoltStore) GetVectorByExtID(extID string) (*VectorRecord, error) {
	var rec *VectorRecord
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketVectors)
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var r VectorRecord
			if err := json.Unmarshal(v, &r); err != nil {
				continue
			}
			if r.ExtID == extID {
				rec = &r
				return nil
			}
		}
		return nil
	})
	return rec, err
}

func (s *BoltStore) DeleteVector(id VectorID) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketVectors)
		key := make([]byte, 8)
		binary.LittleEndian.PutUint64(key, uint64(id))
		return b.Delete(key)
	})
}

func (s *BoltStore) DeleteVectorByExtID(extID string) (VectorID, error) {
	var id VectorID
	err := s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketVectors)
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var r VectorRecord
			if err := json.Unmarshal(v, &r); err != nil {
				continue
			}
			if r.ExtID == extID {
				if err := b.Delete(k); err != nil {
					return err
				}
				id = r.ID
				return nil
			}
		}
		return nil
	})
	return id, err
}

func (s *BoltStore) CountVectors() (int64, error) {
	var count int64
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketVectors)
		count = int64(b.Stats().KeyN)
		return nil
	})
	return count, err
}

func (s *BoltStore) PutSegment(meta *SegmentMeta) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSegments)
		key := make([]byte, 4)
		binary.LittleEndian.PutUint32(key, uint32(meta.ID))
		rec := segmentRecord{Meta: *meta, CreatedAt: time.Now()}
		data, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		return b.Put(key, data)
	})
}

func (s *BoltStore) GetSegment(id SegmentID) (*SegmentMeta, error) {
	var meta *SegmentMeta
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSegments)
		key := make([]byte, 4)
		binary.LittleEndian.PutUint32(key, uint32(id))
		data := b.Get(key)
		if data == nil {
			return nil
		}
		var rec segmentRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return err
		}
		meta = &rec.Meta
		return nil
	})
	return meta, err
}

func (s *BoltStore) ListSegments() ([]*SegmentMeta, error) {
	var metas []*SegmentMeta
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSegments)
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var rec segmentRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				continue
			}
			m := rec.Meta
			metas = append(metas, &m)
		}
		return nil
	})
	return metas, err
}

func (s *BoltStore) DeleteSegment(id SegmentID) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketSegments)
		key := make([]byte, 4)
		binary.LittleEndian.PutUint32(key, uint32(id))
		return b.Delete(key)
	})
}

func (s *BoltStore) GetIDGen() uint64 {
	var n uint64
	s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketIDGen)
		data := b.Get([]byte("next_id"))
		if len(data) == 8 {
			n = binary.LittleEndian.Uint64(data)
		}
		return nil
	})
	return n
}

func (s *BoltStore) SetIDGen(n uint64) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketIDGen)
		data := make([]byte, 8)
		binary.LittleEndian.PutUint64(data, n)
		return b.Put([]byte("next_id"), data)
	})
}

func (s *BoltStore) PutStat(key string, value int64) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketStats)
		data := make([]byte, 8)
		binary.LittleEndian.PutUint64(data, uint64(value))
		return b.Put([]byte(key), data)
	})
}

func (s *BoltStore) GetStat(key string) (int64, error) {
	var val int64
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketStats)
		data := b.Get([]byte(key))
		if len(data) == 8 {
			val = int64(binary.LittleEndian.Uint64(data))
		}
		return nil
	})
	return val, err
}

func (s *BoltStore) IterateVectors(fn func(rec *VectorRecord) bool) error {
	return s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketVectors)
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var r VectorRecord
			if err := json.Unmarshal(v, &r); err != nil {
				continue
			}
			if !fn(&r) {
				break
			}
		}
		return nil
	})
}

func (s *BoltStore) VectorsInSegment(seg SegmentID) ([]*VectorRecord, error) {
	var recs []*VectorRecord
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketVectors)
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var r VectorRecord
			if err := json.Unmarshal(v, &r); err != nil {
				continue
			}
			if r.Segment == seg {
				recs = append(recs, &r)
			}
		}
		return nil
	})
	return recs, err
}

func (s *BoltStore) Close() error {
	return s.db.Close()
}
