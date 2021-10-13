package sortedextent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/chubaofs/chubaofs/proto"
	"github.com/chubaofs/chubaofs/util/log"
)

type SortedExtents struct {
	sync.RWMutex
	eks []proto.ExtentKey
	set ExtentKeySet
}

func NewSortedExtents() *SortedExtents {
	return &SortedExtents{
		eks: make([]proto.ExtentKey, 0),
		set: NewExtentKeySet(),
	}
}

func (se *SortedExtents) Update(eks []proto.ExtentKey) {
	se.RWMutex.Lock()
	defer se.RWMutex.Unlock()

	se.eks = se.eks[:0]
	se.eks = append(se.eks, eks...)

	return
}

func (se *SortedExtents) String() string {
	se.RLock()
	data, err := json.Marshal(se.eks)
	se.RUnlock()
	if err != nil {
		return ""
	}
	return string(data)
}

func (se *SortedExtents) MarshalBinary() ([]byte, error) {
	var data []byte

	se.RLock()
	defer se.RUnlock()

	for _, ek := range se.eks {
		ekdata, err := ek.MarshalBinary()
		if err != nil {
			return nil, err
		}
		data = append(data, ekdata...)
	}
	return data, nil
}

func (se *SortedExtents) MarshalBinaryV2() ([]byte, error) {
	var data []byte

	se.RLock()
	defer se.RUnlock()
	data = make([]byte, len(se.eks)*proto.ExtentLength)

	for _, ek := range se.eks {
		ekdata, err := ek.MarshalBinaryV2()
		if err != nil {
			return nil, err
		}
		data = append(data, ekdata...)
	}
	return data, nil
}

func (se *SortedExtents) UnmarshalBinary(ctx context.Context, data []byte) error {
	var ek proto.ExtentKey

	buf := bytes.NewBuffer(data)
	for {
		if buf.Len() == 0 {
			break
		}
		if err := ek.UnmarshalBinary(buf); err != nil {
			return err
		}
		se.Append(ctx, ek)
	}
	return nil
}

func (se *SortedExtents) UnmarshalBinaryV2(ctx context.Context, data []byte) error {
	var ek proto.ExtentKey
	for start := 0; start < len(data); {
		if len(data[start:]) < proto.ExtentLength {
			return fmt.Errorf("extentLength buff err, need at least %d, but buff len:%d", proto.ExtentLength, len(data))
		}
		if err := ek.UnmarshalBinaryV2(data[start : start+proto.ExtentLength]); err != nil {
			return err
		}
		se.Append(ctx, ek)
		start += proto.ExtentLength
	}
	return nil
}

// Insert makes insertion support for SortedExtentKeys.
// This method will insert the specified ek into the correct position in the extent key chain,
// adjust the existing extent keys in the chain before inserting, and perform the extent keys
// that overlap with the extent keys to be inserted. Split, modify or exchange.
// Finally, the completely useless extent keys are returned to the caller, and the data pointed
// to by these completely useless extent keys can be safely deleted.
//
// Related unit test cases:
//   1.TestSortedExtents_Insert01
//   2.TestSortedExtents_Insert02
//   3.TestSortedExtents_Insert03
//   4.TestSortedExtents_Insert04
// These test cases cover 100% of the this method.
func (se *SortedExtents) Insert(ctx context.Context, ek proto.ExtentKey) (deleteExtents []proto.ExtentKey) {
	se.RWMutex.Lock()
	defer se.RWMutex.Unlock()

	defer func() {
		if se.set.Length() > 0 {
			se.set.Reset()
		}
	}()

	// -------------------------------------------------------------------------------------
	// Sample:
	//                        |=============================|
	//                        ↑           ek                ↑
	//                  ek.FileOffset             ek.FileOffset+ek.Size
	//                        ↓                             ↓
	//                        //////////////////////////////
	//                              shadow (overlap area)
	//                        ↓                             ↓
	//     |===========|=============================================|=============|
	//         cur-1   ↑                   cur                       ↑    cur+1
	//          cur.FileOffset                              cur.FileOffset+cur.Size
	//                 ↑______↑                             ↑________↑
	//                fixedFront                             fixedBack
	//                 ↓      ↓                             ↓        ↓
	//     |===========|======|=============================|========|=============|
	//        cur-1   fixedFront            ek               fixedBack    cur+1
	//
	// -------------------------------------------------------------------------------------
	// About fixedFront(proto.ExtentKey):
	//      FileOffset  :	cur.FileOffset
	//      PartitionId :	cur.PartitionId
	//      ExtentId    : 	cur.ExtentId
	//      ExtentOffset: 	cur.ExtentOffset
	// 		Size		: 	ek.FileOffset-cur.FileOffset
	//
	// -------------------------------------------------------------------------------------
	// About fixedBack(proto.ExtentKey):
	//		FileOffset: 	ek.FileOffset+ek.Size
	//      PartitionId:	cur.PartitionId
	//      ExtentId:		cur.ExtentId
	// 		ExtentOffset:	cur.ExtentOffset+ek.FileOffset-cur.FileOffset+ek.Size
	//		Size:			cur.Size-(ek.FileOffset-cur.FileOffset+ek.Size)
	//
	// -------------------------------------------------------------------------------------
	// About the insert position:
	//   In the process of traversing the extent key chain, if the insertion point is not found
	//   and the FileOffset of the current node is greater than the extent key to be inserted,
	//   it is currently the best insertion point. At this time, the extent key will be inserted
	//   in front of the current node and the inserted state will be marked (set inserted to true).
	//   This indicates that the insertion has been completed.
	//   If the incomplete insertion is thrown after the traversal is completed (inserted is false),
	//   it will inserted at the end.

	var (
		fixedFront, fixedBack *proto.ExtentKey
		inserted              = false
		index                 = 0
		cur                   *proto.ExtentKey

		// Previous overlap flag.
		// In the process of traversing the extent key chain, this mark indicates whether
		// the previously traversed extent key overlaps with the extent key which to be inserted.
		// Through this mark, we can know in advance whether the conditions for terminating
		// the traversal are met:
		//
		//       ek:                  |=====|
		//                            ↓     ↓
		//      eks:  |=====|......|=====|=====|=====|=====|.....|=====|
		//             first              prev   cur  next        last
		//                                 ↗      ↑     ↖           ↑
		//  overlap:                    yes      no     no          no
		//
		// If the current extent key overlaps but does not currently overlap, it means that
		// all subsequent extent keys that have not been traversed meet the requirement that
		// the lower edge is greater than the upper boundary of the extent key to be inserted,
		// and there will be no overlap relationship.
		prevOverlap = false
	)

	// Try to quickly find the first overlapped position to improve traverse process.
	if len(se.eks) > 0 {
		if last := se.eks[len(se.eks)-1]; last.FileOffset+uint64(last.Size) <= ek.FileOffset {

			// When the lower boundary of the extent key (ek) to be inserted exceeds the upper boundary
			// of the last node in the extent key chain (se.eks), it indicates that the  best insertion
			// position is at the end and has no overlap relationship with other nodes.
			//
			//    ek:                                 |=====|
			//                                        ↓     ↓
			//   eks:  |=====|......|=====|=====|     ///////
			//          first              last ↓     ↓
			//                                  ↓     ↓
			//                         upper(last) <= lower(ek)
			//
			index = len(se.eks)
		} else if ek.FileOffset >= se.eks[0].FileOffset+uint64(se.eks[0].Size) {

			// Try to quickly find the first position that satisfies the overlap relationship based on
			// binary search. If it is found successfully, the traversal process starts from this position,
			// which can effectively improve the processing speed.
			//
			//      ek:                         |=====|
			//                 first overlap    ↓     ↓
			//                              ↘   ///////
			//     eks:  |=====|......|=====|=====|=====|......|=====|
			//            first         ↗   ↑  ↑      ↖         last
			//  overlap:              no    ↑ yes      yes
			//                              ↑
			//                       best start index
			//
			if boostStart := findFirstOverlapPosition(se.eks, &ek); boostStart > 0 && boostStart < len(se.eks) {
				index = boostStart
			}
		}
	}

	for {
		if index >= len(se.eks) {
			break
		}
		// Reset working variables.
		fixedFront, fixedBack = nil, nil
		cur = &se.eks[index]

		if !inserted && (ek.FileOffset <= cur.FileOffset) {
			if merged := se.insertOrMergeAt(index, ek); !merged {
				index++
			}
			inserted = true
			continue
		}

		if se.maybeMergeWithPrev(index) {
			continue
		}

		// Check whether the two ExtentKeys overlap
		if !cur.Overlap(&ek) {
			if prevOverlap {
				break
			}
			index++
			continue
		}
		prevOverlap = true

		if ek.FileOffset > cur.FileOffset {
			fixedFront = &proto.ExtentKey{
				FileOffset:   cur.FileOffset,
				PartitionId:  cur.PartitionId,
				ExtentId:     cur.ExtentId,
				ExtentOffset: cur.ExtentOffset,
				Size:         uint32(ek.FileOffset - cur.FileOffset),
			}
		}
		if ek.FileOffset+uint64(ek.Size) < cur.FileOffset+uint64(cur.Size) {
			fixedBack = &proto.ExtentKey{
				FileOffset:   ek.FileOffset + uint64(ek.Size),
				PartitionId:  cur.PartitionId,
				ExtentId:     cur.ExtentId,
				ExtentOffset: cur.ExtentOffset + ek.FileOffset - cur.FileOffset + uint64(ek.Size),
				Size:         cur.Size - uint32(ek.FileOffset-cur.FileOffset+uint64(ek.Size)),
			}
		}

		if fixedFront == nil && fixedBack == nil {
			// That means the cur is totally overlap by the new extent key (ek).
			// E.g.
			//        |=================================================================|
			//        ↓                              ek                                 ↓
			//        ↓                |===========================|                    ↓
			//        ↓                ↓             cur           ↓                    ↓
			//   ek.FileOffset <= cur.FileOffset <= cur.FileOffset+cur.Size <= ek.FileOffset+ek.Size
			//
			// In this case the cur need be remove from extent key chan (se.eks).
			se.set.Put(cur)
			if index == len(se.eks)-1 {
				se.eks = se.eks[:index]
			} else {
				se.eks = append(se.eks[:index], se.eks[index+1:]...)
			}
			se.eks = append(se.eks)
			continue // Continue and do not advance index cause of element removed
		} else {
			if fixedFront != nil {
				// Make exchange between cur and fixedFront.
				se.eks[index] = *fixedFront
				if fixedBack != nil {
					var eks []proto.ExtentKey
					eks = append([]proto.ExtentKey{}, se.eks[:index+1]...)
					eks = append(eks, *fixedBack)
					eks = append(eks, se.eks[index+1:]...)
					se.eks = eks
				}
			} else if fixedBack != nil {
				// Make exchange between cur and fixedBack
				se.eks[index] = *fixedBack
			}
			if se.maybeMergeWithPrev(index) {
				continue
			}
		}

		// Advance
		index++
	}

	if !inserted {
		// In the process of traversing eks, no suitable insertion position is found,
		// indicating that the ek to be inserted should be at the end.
		se.insertOrMergeAt(len(se.eks), ek)
	}

	// Analyze garbage
	if se.set.Length() > 0 {
		for i := 0; i < len(se.eks); i++ {
			se.set.Remove(&se.eks[i])
			if se.set.Length() == 0 {
				break
			}
		}
		deleteExtents = se.set.ToExtentKeys()
	}

	return
}

// Insert ek into the specified position in the ek chain, and check whether the data is continuous with
// the ek data before the insertion position before inserting, and merge with it if it is continuous.
func (se *SortedExtents) insertOrMergeAt(index int, ek proto.ExtentKey) (merged bool) {
	if index > 0 &&
		se.eks[index-1].PartitionId == ek.PartitionId &&
		se.eks[index-1].ExtentId == ek.ExtentId &&
		se.eks[index-1].FileOffset+uint64(se.eks[index-1].Size) == ek.FileOffset &&
		se.eks[index-1].ExtentOffset+uint64(se.eks[index-1].Size) == ek.ExtentOffset {
		se.eks[index-1].Size = se.eks[index-1].Size + ek.Size
		merged = true
	} else if index < len(se.eks) {
		var eks []proto.ExtentKey
		eks = append([]proto.ExtentKey{}, se.eks[:index]...)
		eks = append(eks, ek)
		eks = append(eks, se.eks[index:]...)
		se.eks = eks
	} else {
		se.eks = append(se.eks, ek)
	}
	return
}

// Check whether the ek at the specified position can be merged with the previous ek,
// and merge if the data is continuous.
func (se *SortedExtents) maybeMergeWithPrev(index int) (merged bool) {
	if index > 0 && index < len(se.eks) &&
		se.eks[index-1].PartitionId == se.eks[index].PartitionId &&
		se.eks[index-1].ExtentId == se.eks[index].ExtentId &&
		se.eks[index-1].FileOffset+uint64(se.eks[index-1].Size) == se.eks[index].FileOffset &&
		se.eks[index-1].ExtentOffset+uint64(se.eks[index-1].Size) == se.eks[index].ExtentOffset {
		se.eks[index-1].Size = se.eks[index-1].Size + se.eks[index].Size
		var eks []proto.ExtentKey
		eks = append([]proto.ExtentKey{}, se.eks[:index]...)
		if index+1 < len(se.eks) {
			eks = append(eks, se.eks[index+1:]...)
		}
		se.eks = eks
		merged = true
	}
	return
}

func (se *SortedExtents) Append(ctx context.Context, ek proto.ExtentKey) (deleteExtents []proto.ExtentKey) {
	endOffset := ek.FileOffset + uint64(ek.Size)

	se.Lock()
	defer se.Unlock()

	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprintf("panic occurred when append extent key!\n"+
				"Partition: %v\n"+
				"Exists extent keys: %v\n"+
				"Append extent key: %v\n"+
				"Panic message: %v\n",
				ek.PartitionId, se.eks, ek, r)
			log.LogErrorf(msg)
		}
	}()

	if len(se.eks) <= 0 {
		se.eks = append(se.eks, ek)
		return
	}
	lastKey := se.eks[len(se.eks)-1]
	if lastKey.FileOffset+uint64(lastKey.Size) <= ek.FileOffset {
		se.eks = append(se.eks, ek)
		return
	}
	firstKey := se.eks[0]
	if firstKey.FileOffset >= endOffset {
		eks := se.doCopyExtents()
		se.eks = se.eks[:0]
		se.eks = append(se.eks, ek)
		se.eks = append(se.eks, eks...)
		return
	}

	var startIndex, endIndex int

	invalidExtents := make([]proto.ExtentKey, 0)
	for idx, key := range se.eks {
		if ek.FileOffset > key.FileOffset {
			startIndex = idx + 1
			continue
		}
		if endOffset >= key.FileOffset+uint64(key.Size) {
			invalidExtents = append(invalidExtents, key)
			continue
		}
		break
	}

	endIndex = startIndex + len(invalidExtents)
	upperExtents := make([]proto.ExtentKey, len(se.eks)-endIndex)
	copy(upperExtents, se.eks[endIndex:])
	se.eks = se.eks[:startIndex]
	se.eks = append(se.eks, ek)
	se.eks = append(se.eks, upperExtents...)
	// check if ek and key are the same extent file with size extented
	deleteExtents = make([]proto.ExtentKey, 0, len(invalidExtents))
	for _, key := range invalidExtents {
		if key.PartitionId != ek.PartitionId || key.ExtentId != ek.ExtentId {
			deleteExtents = append(deleteExtents, key)
		}
	}
	return
}

func (se *SortedExtents) Truncate(offset uint64) (deleteExtents []proto.ExtentKey) {
	var endIndex int

	se.Lock()
	defer se.Unlock()

	endIndex = -1
	for idx, key := range se.eks {
		if key.FileOffset >= offset {
			endIndex = idx
			break
		}
	}

	if endIndex < 0 {
		deleteExtents = make([]proto.ExtentKey, 0)
	} else {
		deleteExtents = make([]proto.ExtentKey, len(se.eks)-endIndex)
		copy(deleteExtents, se.eks[endIndex:])
		se.eks = se.eks[:endIndex]
	}

	numKeys := len(se.eks)
	if numKeys > 0 {
		lastKey := &se.eks[numKeys-1]
		if lastKey.FileOffset+uint64(lastKey.Size) > offset {
			lastKey.Size = uint32(offset - lastKey.FileOffset)
		}
	}
	return
}

func (se *SortedExtents) Len() int {
	se.RLock()
	defer se.RUnlock()
	return len(se.eks)
}

// Returns the file size
func (se *SortedExtents) Size() uint64 {
	se.RLock()
	defer se.RUnlock()

	last := len(se.eks)
	if last <= 0 {
		return uint64(0)
	}
	return se.eks[last-1].FileOffset + uint64(se.eks[last-1].Size)
}

func (se *SortedExtents) Range(f func(ek proto.ExtentKey) bool) {
	se.RLock()
	defer se.RUnlock()

	for _, ek := range se.eks {
		if !f(ek) {
			break
		}
	}
}

func (se *SortedExtents) Clone() *SortedExtents {
	newSe := NewSortedExtents()

	se.RLock()
	defer se.RUnlock()

	newSe.eks = se.doCopyExtents()
	return newSe
}

func (se *SortedExtents) CopyExtents() []proto.ExtentKey {
	se.RLock()
	defer se.RUnlock()
	return se.doCopyExtents()
}

func (se *SortedExtents) doCopyExtents() []proto.ExtentKey {
	eks := make([]proto.ExtentKey, len(se.eks))
	copy(eks, se.eks)
	return eks
}

func (se *SortedExtents) HasExtent(inEk proto.ExtentKey) (ok bool, ekInfo *proto.ExtentKey) {
	se.RLock()
	defer se.RUnlock()
	for _, ek := range se.eks {
		if ek.PartitionId == inEk.PartitionId && ek.ExtentId == inEk.ExtentId {
			ekInfo = &ek
			ok = true
			return
			//if ek.FileOffset >= inEk.FileOffset || (ek.FileOffset + uint64(ek.Size)) > inEk.FileOffset {
			//   check file offset
			//}
		}
	}
	return false, nil
}

// This method is based on recursive binary search to find the first overlapping position.
func findFirstOverlapPosition(eks []proto.ExtentKey, ek *proto.ExtentKey) int {
	switch {
	case len(eks) < 1:
		return -1
	case len(eks) == 1:
		if ek.FileOffset < eks[0].FileOffset+uint64(eks[0].Size) {
			return 0
		} else {
			return -1
		}
	default:
	}
	var (
		mid        = len(eks) / 2
		left       = eks[:mid]
		right      = eks[mid:]
		off, boost int
	)
	if leftLast := left[len(left)-1]; ek.FileOffset < leftLast.FileOffset+uint64(leftLast.Size) {
		off, boost = 0, findFirstOverlapPosition(left, ek)
	} else {
		off, boost = mid, findFirstOverlapPosition(right, ek)
	}
	if boost >= 0 {
		return off + boost
	}
	return -1
}

type ExtentKeySet []proto.ExtentKey

func (s ExtentKeySet) sort() {
	sort.SliceStable(s, func(i, j int) bool {
		return s[i].PartitionId < s[j].PartitionId ||
			(s[i].PartitionId == s[j].PartitionId &&
				s[i].ExtentId < s[j].ExtentId)
	})
}

func (s ExtentKeySet) search(ek *proto.ExtentKey) (i int, found bool) {
	i = sort.Search(len(s), func(i int) bool {
		return ek.PartitionId < s[i].PartitionId || (ek.PartitionId == s[i].PartitionId && ek.ExtentId <= s[i].ExtentId)
	})
	found = i >= 0 && i < len(s) && ek.PartitionId == s[i].PartitionId && ek.ExtentId == s[i].ExtentId
	return
}

func (s *ExtentKeySet) Put(ek *proto.ExtentKey) {
	if _, found := s.search(ek); !found {
		*s = append(*s, *ek)
		s.sort()
	}
}

func (s ExtentKeySet) Has(ek *proto.ExtentKey) (has bool) {
	_, has = s.search(ek)
	return
}

func (s *ExtentKeySet) Remove(ek *proto.ExtentKey) {
	if i, found := s.search(ek); found {
		if i == len(*s)-1 {
			*s = (*s)[:i]
			return
		}
		*s = append((*s)[:i], (*s)[i+1:]...)
	}
	return
}

func (s ExtentKeySet) ToExtentKeys() []proto.ExtentKey {
	return s
}

func (s ExtentKeySet) Length() int {
	return len(s)
}

func (s *ExtentKeySet) Reset() {
	*s = (*s)[:0]
}

func NewExtentKeySet() ExtentKeySet {
	return ExtentKeySet(make([]proto.ExtentKey, 0))
}