/*
 * Minio Cloud Storage, (C) 2016 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"encoding/json"
	"math/rand"
	"path"
	"sort"
	"sync"
	"time"
)

// Erasure block size.
const (
	erasureBlockSize          = 4 * 1024 * 1024 // 4MiB.
	erasureAlgorithmKlauspost = "klauspost/reedsolomon/vandermonde"
	erasureAlgorithmISAL      = "isa-l/reedsolomon/cauchy"
)

// objectPartInfo Info of each part kept in the multipart metadata
// file after CompleteMultipartUpload() is called.
type objectPartInfo struct {
	Number int    `json:"number"`
	Name   string `json:"name"`
	ETag   string `json:"etag"`
	Size   int64  `json:"size"`
}

// A xlMetaV1 represents a metadata header mapping keys to sets of values.
type xlMetaV1 struct {
	Version string `json:"version"`
	Format  string `json:"format"`
	Stat    struct {
		Size    int64     `json:"size"`
		ModTime time.Time `json:"modTime"`
		Version int64     `json:"version"`
	} `json:"stat"`
	Erasure struct {
		Algorithm    string `json:"algorithm"`
		DataBlocks   int    `json:"data"`
		ParityBlocks int    `json:"parity"`
		BlockSize    int64  `json:"blockSize"`
		Index        int    `json:"index"`
		Distribution []int  `json:"distribution"`
		Checksum     []struct {
			Name      string `json:"name"`
			Algorithm string `json:"algorithm"`
			Hash      string `json:"hash"`
		} `json:"checksum"`
	} `json:"erasure"`
	Minio struct {
		Release string `json:"release"`
	} `json:"minio"`
	Meta  map[string]string `json:"meta"`
	Parts []objectPartInfo  `json:"parts,omitempty"`
}

// byPartName is a collection satisfying sort.Interface.
type byPartNumber []objectPartInfo

func (t byPartNumber) Len() int           { return len(t) }
func (t byPartNumber) Swap(i, j int)      { t[i], t[j] = t[j], t[i] }
func (t byPartNumber) Less(i, j int) bool { return t[i].Number < t[j].Number }

// ObjectPartIndex - returns the index of matching object part number.
func (m xlMetaV1) ObjectPartIndex(partNumber int) (index int) {
	for i, part := range m.Parts {
		if partNumber == part.Number {
			index = i
			return index
		}
	}
	return -1
}

// AddObjectPart - add a new object part in order.
func (m *xlMetaV1) AddObjectPart(partNumber int, partName string, partETag string, partSize int64) {
	partInfo := objectPartInfo{
		Number: partNumber,
		Name:   partName,
		ETag:   partETag,
		Size:   partSize,
	}

	// Update part info if it already exists.
	for i, part := range m.Parts {
		if partNumber == part.Number {
			m.Parts[i] = partInfo
			return
		}
	}

	// Proceed to include new part info.
	m.Parts = append(m.Parts, partInfo)

	// Parts in xlMeta should be in sorted order by part number.
	sort.Sort(byPartNumber(m.Parts))
}

// objectToPartOffset - translate offset of an object to offset of its individual part.
func (m xlMetaV1) objectToPartOffset(offset int64) (partIndex int, partOffset int64, err error) {
	partOffset = offset
	// Seek until object offset maps to a particular part offset.
	for i, part := range m.Parts {
		partIndex = i
		// Last part can be of '0' bytes, treat it specially and
		// return right here.
		if part.Size == 0 {
			return partIndex, partOffset, nil
		}
		// Offset is smaller than size we have reached the proper part offset.
		if partOffset < part.Size {
			return partIndex, partOffset, nil
		}
		// Continue to towards the next part.
		partOffset -= part.Size
	}
	// Offset beyond the size of the object return InvalidRange.
	return 0, 0, InvalidRange{}
}

// readXLMetadata - returns the object metadata `xl.json` content from
// one of the disks picked at random.
func (xl xlObjects) readXLMetadata(bucket, object string) (xlMeta xlMetaV1, err error) {
	// Count for errors encountered.
	var xlJSONErrCount = 0

	// Allocate 4MB buffer.
	var buffer = make([]byte, blockSize)

	// Return the first successful lookup from a random list of disks.
	for xlJSONErrCount < len(xl.storageDisks) {
		disk := xl.getRandomDisk() // Choose a random disk on each attempt.
		var n int64
		n, err = disk.ReadFile(bucket, path.Join(object, xlMetaJSONFile), int64(0), buffer)
		if err == nil {
			err = json.Unmarshal(buffer[:n], &xlMeta)
			if err == nil {
				return xlMeta, nil
			}
		}
		xlJSONErrCount++ // Update error count.
	}
	return xlMetaV1{}, err
}

// newXLMetaV1 - initializes new xlMetaV1.
func newXLMetaV1(dataBlocks, parityBlocks int) (xlMeta xlMetaV1) {
	xlMeta = xlMetaV1{}
	xlMeta.Version = "1"
	xlMeta.Format = "xl"
	xlMeta.Minio.Release = minioReleaseTag
	xlMeta.Erasure.Algorithm = erasureAlgorithmKlauspost
	xlMeta.Erasure.DataBlocks = dataBlocks
	xlMeta.Erasure.ParityBlocks = parityBlocks
	xlMeta.Erasure.BlockSize = erasureBlockSize
	xlMeta.Erasure.Distribution = randErasureDistribution(dataBlocks + parityBlocks)
	return xlMeta
}

func (xl xlObjects) renameXLMetadata(srcBucket, srcPrefix, dstBucket, dstPrefix string) error {
	var wg = &sync.WaitGroup{}
	var mErrs = make([]error, len(xl.storageDisks))

	srcJSONFile := path.Join(srcPrefix, xlMetaJSONFile)
	dstJSONFile := path.Join(dstPrefix, xlMetaJSONFile)
	// Rename `xl.json` to all disks in parallel.
	for index, disk := range xl.storageDisks {
		wg.Add(1)
		// Rename `xl.json` in a routine.
		go func(index int, disk StorageAPI) {
			defer wg.Done()
			rErr := disk.RenameFile(srcBucket, srcJSONFile, dstBucket, dstJSONFile)
			if rErr != nil {
				mErrs[index] = rErr
				return
			}
			mErrs[index] = nil
		}(index, disk)
	}
	// Wait for all the routines.
	wg.Wait()

	// Return the first error.
	for _, err := range mErrs {
		if err == nil {
			continue
		}
		return err
	}
	return nil
}

// writeXLMetadata - write `xl.json` on all disks in order.
func (xl xlObjects) writeXLMetadata(bucket, prefix string, xlMeta xlMetaV1) error {
	var wg = &sync.WaitGroup{}
	var mErrs = make([]error, len(xl.storageDisks))

	jsonFile := path.Join(prefix, xlMetaJSONFile)
	// Start writing `xl.json` to all disks in parallel.
	for index, disk := range xl.storageDisks {
		wg.Add(1)
		// Write `xl.json` in a routine.
		go func(index int, disk StorageAPI, metadata xlMetaV1) {
			defer wg.Done()

			// Save the disk order index.
			metadata.Erasure.Index = index + 1

			metadataBytes, err := json.Marshal(&metadata)
			if err != nil {
				mErrs[index] = err
				return
			}
			n, mErr := disk.AppendFile(bucket, jsonFile, metadataBytes)
			if mErr != nil {
				mErrs[index] = mErr
				return
			}
			if n != int64(len(metadataBytes)) {
				mErrs[index] = errUnexpected
				return
			}
			mErrs[index] = nil
		}(index, disk, xlMeta)
	}

	// Wait for all the routines.
	wg.Wait()

	// Return the first error.
	for _, err := range mErrs {
		if err == nil {
			continue
		}
		return err
	}
	return nil
}

// randErasureDistribution - uses Knuth Fisher-Yates shuffle algorithm.
func randErasureDistribution(numBlocks int) []int {
	rand.Seed(time.Now().UTC().UnixNano()) // Seed with current time.
	distribution := make([]int, numBlocks)
	for i := 0; i < numBlocks; i++ {
		distribution[i] = i + 1
	}
	for i := 0; i < numBlocks; i++ {
		// Choose index uniformly in [i, numBlocks-1]
		r := i + rand.Intn(numBlocks-i)
		distribution[r], distribution[i] = distribution[i], distribution[r]
	}
	return distribution
}
