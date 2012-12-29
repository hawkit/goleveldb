// Copyright (c) 2012, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// This LevelDB Go implementation is based on LevelDB C++ implementation.
// Which contains the following header:
//   Copyright (c) 2011 The LevelDB Authors. All rights reserved.
//   Use of this source code is governed by a BSD-style license that can be
//   found in the LEVELDBCPP_LICENSE file. See the LEVELDBCPP_AUTHORS file
//   for names of contributors.

package db

import (
	"leveldb"
	"leveldb/descriptor"
	"leveldb/log"
	"sync"
)

type session struct {
	sync.RWMutex

	desc   descriptor.Descriptor
	opt    *iOptions
	cmp    *iComparer
	filter *iFilter
	tops   *tOps

	manifest       *log.Writer
	manifestWriter descriptor.Writer

	st struct {
		sync.RWMutex
		version         *version
		versions        []*version
		nextNum         stateNum
		logNum          uint64
		seq             uint64
		compactPointers [kNumLevels]iKey
	}
}

func newSession(desc descriptor.Descriptor, opt *leveldb.Options) *session {
	s := new(session)
	s.desc = desc
	s.opt = &iOptions{s, opt}
	s.cmp = &iComparer{opt.GetComparer()}
	filter := opt.GetFilter()
	if filter != nil {
		s.filter = &iFilter{filter}
	}
	s.tops = newTableOps(s, s.opt.GetMaxOpenFiles())
	s.setVersion(&version{s: s})
	return s
}

// Create a new database session
func (s *session) create() (err error) {
	// create manifest
	err = s.createManifest(s.allocFileNum(), nil, nil)
	if err != nil {
		return
	}
	return
}

// Recover a database session
func (s *session) recover() (err error) {
	var m descriptor.File
	m, err = s.desc.GetMainManifest()
	if err != nil {
		return
	}

	r, err := m.Open()
	if err != nil {
		return
	}

	st := &s.st

	cmpName := s.cmp.cmp.Name()
	staging := st.version.newStaging()
	srec := new(sessionRecord)
	lr := log.NewReader(r, true)
	for lr.Next() {
		rec := new(sessionRecord)
		err = rec.decode(lr.Record())
		if err != nil {
			continue
		}

		if rec.hasComparer && rec.comparer != cmpName {
			return leveldb.ErrInvalid("invalid comparer, " +
				"want '" + cmpName + "', " +
				"got '" + rec.comparer + "'")
		}

		// save compact pointers
		for _, rp := range rec.compactPointers {
			st.compactPointers[rp.level] = iKey(rp.key)
		}

		// commit record to version staging
		staging.commit(rec)

		if rec.hasLogNum {
			srec.setLogNum(rec.logNum)
		}
		if rec.hasNextNum {
			srec.setNextNum(rec.nextNum)
		}
		if rec.hasSeq {
			srec.setSeq(rec.seq)
		}
	}
	// check for error in log reader
	err = lr.Error()
	if err != nil {
		return
	}

	switch false {
	case srec.hasNextNum:
		err = leveldb.ErrCorrupt("manifest missing next file number")
	case srec.hasLogNum:
		err = leveldb.ErrCorrupt("manifest missing log file number")
	case srec.hasSeq:
		err = leveldb.ErrCorrupt("manifest missing seq number")
	}
	if err != nil {
		return
	}

	s.setVersion(staging.finish())
	s.setFileNum(srec.nextNum)
	s.recordCommited(srec)

	return
}

func (s *session) commit(r *sessionRecord) (err error) {
	// spawn new version based on current version
	nv := s.st.version.spawn(r)

	if s.manifest == nil {
		// manifest log writer not yet created, create one
		err = s.createManifest(s.allocFileNum(), r, nv)
	} else {
		err = s.flushManifest(r)
	}

	// finally, apply new version if no error rise
	if err == nil {
		s.setVersion(nv)
	}

	return
}

func (s *session) needCompaction() bool {
	st := &s.st
	st.RLock()
	defer st.RUnlock()
	v := st.version
	return v.compactionScore >= 1 || v.seekCompactionTable != nil
}

func (s *session) pickCompaction() (c *compaction) {
	st := &s.st

	st.RLock()
	v := st.version
	bySize := v.compactionScore >= 1
	bySeek := v.seekCompactionTable != nil
	st.RUnlock()

	icmp := s.cmp
	ucmp := icmp.cmp

	var level int
	var t0 tFiles
	if bySize {
		level = v.compactionLevel
		cp := s.st.compactPointers[level]
		tt := v.tables[level]
		for _, t := range tt {
			if cp == nil || icmp.Compare(t.largest, cp) > 0 {
				t0 = append(t0, t)
				break
			}
		}
		if len(t0) == 0 {
			t0 = append(t0, tt[0])
		}
	} else if bySeek {
		level = v.seekCompactionLevel
		t0 = append(t0, v.seekCompactionTable)
	} else {
		return
	}

	c = &compaction{s: s, version: v, level: level}
	if level == 0 {
		min, max := t0.getRange(icmp)
		t0 = nil
		v.tables[0].getOverlaps(min.ukey(), max.ukey(), &t0, false, ucmp)
	}

	c.tables[0] = t0
	c.expand()
	return
}

func (s *session) getCompactionRange(level int, min, max []byte) (c *compaction) {
	st := &s.st

	st.RLock()
	v := st.version
	st.RUnlock()

	var t0 tFiles
	v.tables[level].getOverlaps(min, max, &t0, level != 0, s.cmp.cmp)
	if len(t0) == 0 {
		return nil
	}

	c = &compaction{s: s, version: v, level: level}
	c.tables[0] = t0
	c.expand()
	return
}

type compaction struct {
	s       *session
	version *version

	level  int
	tables [2]tFiles

	gp              tFiles
	gpidx           int
	seenKey         bool
	overlappedBytes uint64
	min, max        iKey

	tPtrs [kNumLevels]int
}

func (c *compaction) expand() {
	s := c.s
	v := c.version
	icmp := s.cmp
	ucmp := icmp.cmp

	level := c.level
	vt0, vt1 := v.tables[level], v.tables[level+1]

	t0, t1 := c.tables[0], c.tables[1]
	min, max := t0.getRange(icmp)
	vt1.getOverlaps(min.ukey(), max.ukey(), &t1, true, ucmp)

	// Get entire range covered by compaction
	amin, amax := append(t0, t1...).getRange(icmp)

	// See if we can grow the number of inputs in "level" without
	// changing the number of "level+1" files we pick up.
	if len(t1) > 0 {
		var exp0 tFiles
		vt0.getOverlaps(amin.ukey(), amax.ukey(), &exp0, level != 0, ucmp)
		if len(exp0) > len(t0) && t1.size()+exp0.size() < kExpCompactionMaxBytes {
			var exp1 tFiles
			xmin, xmax := exp0.getRange(icmp)
			vt1.getOverlaps(xmin.ukey(), xmax.ukey(), &exp1, true, ucmp)
			if len(exp1) == len(t1) {
				s.printf("Compaction: expanding, level=%d from=`%d+%d (%d+%d bytes)' to=`%d+%d (%d+%d bytes)'",
					level, len(t0), len(t1), t0.size(), t1.size(),
					len(exp0), len(exp1), exp0.size(), exp1.size())
				min, max = xmin, xmax
				t0, t1 = exp0, exp1
				amin, amax = append(t0, t1...).getRange(icmp)
			}
		}
	}

	// Compute the set of grandparent files that overlap this compaction
	// (parent == level+1; grandparent == level+2)
	if level+2 < kNumLevels {
		v.tables[level+2].getOverlaps(amin.ukey(), amax.ukey(), &c.gp, true, ucmp)
	}

	c.tables[0], c.tables[1] = t0, t1
	c.min, c.max = min, max
}

func (c *compaction) trivial() bool {
	return len(c.tables[0]) == 1 && len(c.tables[1]) == 0 && c.gp.size() <= kMaxGrandParentOverlapBytes
}

func (c *compaction) isBaseLevelForKey(key []byte) bool {
	s := c.s
	v := c.version
	ucmp := s.cmp.cmp
	for level, tt := range v.tables[c.level+2:] {
		for c.tPtrs[level] < len(tt) {
			t := tt[c.tPtrs[level]]
			if ucmp.Compare(key, t.largest.ukey()) <= 0 {
				// We've advanced far enough
				if ucmp.Compare(key, t.smallest.ukey()) >= 0 {
					// Key falls in this file's range, so definitely not base level
					return false
				}
				break
			}
			c.tPtrs[level]++
		}
	}
	return true
}

func (c *compaction) shouldStopBefore(key iKey) bool {
	icmp := c.s.cmp
	for ; c.gpidx < len(c.gp); c.gpidx++ {
		gp := c.gp[c.gpidx]
		if icmp.Compare(key, gp.largest) <= 0 {
			break
		}
		if c.seenKey {
			c.overlappedBytes += gp.size
		}
	}
	c.seenKey = true

	if c.overlappedBytes > kMaxGrandParentOverlapBytes {
		// Too much overlap for current output; start new output
		c.overlappedBytes = 0
		return true
	}
	return false
}

func (c *compaction) newIterator() leveldb.Iterator {
	s := c.s
	icmp := s.cmp

	level := c.level
	icap := 2
	if c.level == 0 {
		icap = len(c.tables[0]) + 1
	}
	iters := make([]leveldb.Iterator, 0, icap)

	ro := &leveldb.ReadOptions{}

	for i, tt := range c.tables {
		if len(tt) == 0 {
			continue
		}

		if level+i == 0 {
			for _, t := range tt {
				iters = append(iters, s.tops.newIterator(t, ro))
			}
		} else {
			iter := leveldb.NewIndexedIterator(tt.newIndexIterator(s.tops, icmp, ro))
			iters = append(iters, iter)
		}
	}

	return leveldb.NewMergedIterator(iters, icmp)
}